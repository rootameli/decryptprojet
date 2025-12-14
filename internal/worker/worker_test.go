package worker

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"zessen-go/internal/config"
	"zessen-go/internal/logging"
	"zessen-go/internal/metrics"
	smtps "zessen-go/internal/smtp"
	"zessen-go/internal/state"
)

func TestWaitBlocksForRetries(t *testing.T) {
	cfg, tmplDir := buildTestConfig(t)
	account := config.SMTPAccount{Host: "smtp.example.com", Port: 25, User: "u", Password: "p", MailFrom: "bounce@example.com", ID: "smtp-1"}
	logWriter, _ := logging.NewWriter(cfg.Paths.LogsDir)
	logWriter.Start()
	defer logWriter.Stop()
	runner, err := NewRunner(cfg, []config.SMTPAccount{account}, logWriter, state.NewManager(cfg.Paths.StateDir))
	if err != nil {
		t.Fatalf("runner: %v", err)
	}
	_ = tmplDir
	runner.DryRun = true
	runner.TestFailOnce = true

	runner.Enqueue([]string{"user@example.com"}, state.Snapshot{})
	runner.Start(context.Background(), 1)
	runner.Wait()

	snap := runner.Stats.Snapshot()
	if snap.Sent != 1 {
		t.Fatalf("expected 1 sent, got %d", snap.Sent)
	}
	if snap.Pending != 0 {
		t.Fatalf("expected pending 0, got %d", snap.Pending)
	}
}

func TestMessageIDFormatAndUniqueness(t *testing.T) {
	account := config.SMTPAccount{Host: "smtp.example.com", MailFrom: "bounce@univ.fr"}
	r := Runner{}
	seen := make(map[string]struct{})
	for i := 0; i < 5000; i++ {
		id := r.messageID(account)
		if _, ok := seen[id]; ok {
			t.Fatalf("duplicate id: %s", id)
		}
		seen[id] = struct{}{}
		msg := buildMessageForTest(id)
		if !strings.Contains(msg, fmt.Sprintf("Message-ID: <%s>", id)) {
			t.Fatalf("missing brackets for %s", id)
		}
	}
}

func TestCheckpointResume(t *testing.T) {
	cfg, _ := buildTestConfig(t)
	baseMgr := state.NewManager(cfg.Paths.StateDir)
	leads := make([]string, 0, 100)
	leadStates := make(map[string]state.LeadState)
	for i := 0; i < 100; i++ {
		email := fmt.Sprintf("user%d@example.com", i)
		leads = append(leads, email)
		done := i < 60
		leadStates[email] = state.LeadState{Attempts: 1, Done: done}
	}
	snap := state.Snapshot{CampaignID: "camp", BatchID: "batch-1", Leads: leadStates, SMTPs: map[string]state.SMTPState{}, Stats: metrics.Snapshot{Sent: 60, Pending: 40, Total: 100}}
	if err := baseMgr.Save(snap); err != nil {
		t.Fatalf("save: %v", err)
	}
	loaded, err := baseMgr.Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	logWriter, _ := logging.NewWriter(cfg.Paths.LogsDir)
	logWriter.Start()
	defer logWriter.Stop()
	runner, err := NewRunner(cfg, []config.SMTPAccount{{Host: "smtp.example.com", Port: 25, MailFrom: "bounce@example.com", ID: "smtp-1"}}, logWriter, baseMgr)
	if err != nil {
		t.Fatalf("runner: %v", err)
	}
	runner.DryRun = true
	runner.Enqueue(leads, loaded)
	runner.Start(context.Background(), 1)
	runner.Wait()
	snapFinal, _ := runner.Snapshot()
	if len(snapFinal.Leads) != 100 {
		t.Fatalf("expected 100 leads in snapshot, got %d", len(snapFinal.Leads))
	}
	stats := runner.Stats.Snapshot()
	if stats.Sent != 100 || stats.Pending != 0 {
		t.Fatalf("unexpected stats: %+v", stats)
	}
}

func TestDryRunSmoke(t *testing.T) {
	cfg, _ := buildTestConfig(t)
	logWriter, _ := logging.NewWriter(cfg.Paths.LogsDir)
	logWriter.Start()
	defer logWriter.Stop()
	runner, err := NewRunner(cfg, []config.SMTPAccount{{Host: "smtp.example.com", Port: 25, MailFrom: "bounce@example.com", ID: "smtp-1"}}, logWriter, state.NewManager(cfg.Paths.StateDir))
	if err != nil {
		t.Fatalf("runner: %v", err)
	}
	runner.DryRun = true
	leads := []string{"ok@example.com", "bad@unauthorized.com"}
	runner.Enqueue(leads, state.Snapshot{})
	runner.Start(context.Background(), 1)
	runner.Wait()
	stats := runner.Stats.Snapshot()
	if stats.Sent+stats.Failed != int64(len(leads)) {
		t.Fatalf("invariant broken: sent %d failed %d total %d", stats.Sent, stats.Failed, len(leads))
	}
}

func buildTestConfig(t *testing.T) (config.Config, string) {
	t.Helper()
	base := t.TempDir()
	tmplDir := filepath.Join(base, "templates")
	if err := os.MkdirAll(tmplDir, 0o755); err != nil {
		t.Fatalf("mkdir templates: %v", err)
	}
	for _, name := range []string{"profileA.html", "profileB.html", "profileC.html"} {
		if err := os.WriteFile(filepath.Join(tmplDir, name), []byte("<b>hi</b>"), 0o644); err != nil {
			t.Fatalf("write template: %v", err)
		}
	}
	cfg := config.Config{
		CampaignID: "camp",
		Paths: config.Paths{
			LeadsFile:    filepath.Join(base, "leads.txt"),
			SMTPSFile:    filepath.Join(base, "smtps.txt"),
			TemplatesDir: tmplDir,
			LogsDir:      filepath.Join(base, "logs"),
			StateDir:     filepath.Join(base, "state"),
		},
		Concurrency:    config.Concurrency{MaxWorkers: 1},
		Retry:          config.Retry{MaxAttemptsPerLead: 2, BackoffSeconds: []int{0, 0, 0}},
		Timeouts:       config.Timeouts{ConnectTimeoutSeconds: 1, SendTimeoutSeconds: 1, JobTimeoutSeconds: 1},
		CircuitBreaker: config.CircuitBreaker{FailThreshold: 3, CooldownSeconds: 1, DisableAfterCooldowns: 2},
		Batch:          config.Batch{BatchSize: 10},
		Profiles: map[string]config.Profile{
			config.ProfileA: {SubjectPool: []string{"sub"}, FromNamePool: []string{"from"}, XMailer: "mailerA", TemplateFile: "profileA.html"},
			config.ProfileB: {SubjectPool: []string{"sub"}, FromNamePool: []string{"from"}, XMailer: "mailerB", TemplateFile: "profileB.html"},
			config.ProfileC: {SubjectPool: []string{"sub"}, FromNamePool: []string{"from"}, XMailer: "mailerC", TemplateFile: "profileC.html"},
		},
		DomainsAllowlist: []string{"example.com"},
	}
	return cfg, tmplDir
}

func buildMessageForTest(id string) string {
	headers := map[string]string{"Message-ID": id}
	msg := smtps.BuildMessage("from", "from@example.com", "to@example.com", "s", "<b>body</b>", headers)
	return string(msg)
}
