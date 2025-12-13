package worker

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"zessen-go/internal/config"
	"zessen-go/internal/logging"
	"zessen-go/internal/state"
)

func TestWaitBlocksForRetries(t *testing.T) {
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
		Retry:          config.Retry{MaxAttemptsPerLead: 2, BackoffSeconds: []int{0, 0}},
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

	account := config.SMTPAccount{Host: "smtp.example.com", Port: 25, User: "u", Password: "p", MailFrom: "bounce@example.com", ID: "smtp-1"}
	logWriter, err := logging.NewWriter(cfg.Paths.LogsDir)
	if err != nil {
		t.Fatalf("log writer: %v", err)
	}
	logWriter.Start()
	defer logWriter.Stop()

	runner := NewRunner(cfg, []config.SMTPAccount{account}, logWriter, state.NewManager(cfg.Paths.StateDir))
	runner.DryRun = true
	runner.TestFailOnce = true

	runner.Enqueue([]string{"user@example.com"}, state.Snapshot{})
	runner.Start(1)
	runner.Wait()

	snap := runner.Stats.Snapshot()
	if snap.Sent != 1 {
		t.Fatalf("expected 1 sent, got %d", snap.Sent)
	}
	if snap.Pending != 0 {
		t.Fatalf("expected pending 0, got %d", snap.Pending)
	}
}

func TestMessageIDUsesDomain(t *testing.T) {
	account := config.SMTPAccount{Host: "smtp.example.com", MailFrom: "bounce@univ.fr"}
	r := Runner{}
	id := r.messageID(account)
	if !strings.Contains(id, "@univ.fr") {
		t.Fatalf("expected domain from mailfrom, got %s", id)
	}
}
