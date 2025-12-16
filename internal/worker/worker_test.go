package worker

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

	snap := runner.Stats.SnapshotData()
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
	stats := runner.Stats.SnapshotData()
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
	stats := runner.Stats.SnapshotData()
	if stats.Sent+stats.Failed != int64(len(leads)) {
		t.Fatalf("invariant broken: sent %d failed %d total %d", stats.Sent, stats.Failed, len(leads))
	}
}

func TestProfileStickinessPerBatch(t *testing.T) {
	cfg, _ := buildTestConfig(t)
	cfg.Batch.BatchSize = 2
	cfg.Concurrency.MaxWorkers = 2
	logWriter, _ := logging.NewWriter(cfg.Paths.LogsDir)
	logWriter.Start()
	defer logWriter.Stop()
	runner, err := NewRunner(cfg, []config.SMTPAccount{{Host: "smtp.example.com", Port: 25, MailFrom: "bounce@example.com", ID: "smtp-1"}}, logWriter, state.NewManager(cfg.Paths.StateDir))
	if err != nil {
		t.Fatalf("runner: %v", err)
	}
	runner.DryRun = true
	leads := []string{"user0@example.com", "user1@example.com", "user2@example.com", "user3@example.com", "user4@example.com", "user5@example.com"}
	runner.Enqueue(leads, state.Snapshot{})
	runner.Start(context.Background(), cfg.Concurrency.MaxWorkers)
	runner.Wait()
	logWriter.Stop()
	sentPath := filepath.Join(cfg.Paths.LogsDir, "sent.log")
	data, err := os.ReadFile(sentPath)
	if err != nil {
		t.Fatalf("read sent log: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	profileByLead := make(map[int]string)
	for _, line := range lines {
		var ev logging.Event
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		payload, ok := ev.Data.(map[string]interface{})
		if !ok {
			t.Fatalf("unexpected data shape")
		}
		email, _ := payload["email"].(string)
		profile, _ := payload["profile"].(string)
		var idx int
		fmt.Sscanf(email, "user%d@", &idx)
		profileByLead[idx] = profile
	}
	expected := map[int]string{0: config.ProfileA, 1: config.ProfileA, 2: config.ProfileB, 3: config.ProfileB, 4: config.ProfileC, 5: config.ProfileC}
	for k, v := range expected {
		if got := profileByLead[k]; got != v {
			t.Fatalf("lead %d expected profile %s got %s", k, v, got)
		}
	}
}

func TestJobTimeoutStopsLead(t *testing.T) {
	cfg, _ := buildTestConfig(t)
	logWriter, _ := logging.NewWriter(cfg.Paths.LogsDir)
	logWriter.Start()
	defer logWriter.Stop()
	runner, err := NewRunner(cfg, []config.SMTPAccount{{Host: "smtp.example.com", Port: 25, MailFrom: "bounce@example.com", ID: "smtp-1"}}, logWriter, state.NewManager(cfg.Paths.StateDir))
	if err != nil {
		t.Fatalf("runner: %v", err)
	}
	runner.DryRun = true
	runner.jobTimeout = 5 * time.Millisecond
	runner.dryRunDelay = 20 * time.Millisecond
	runner.Enqueue([]string{"user@example.com"}, state.Snapshot{})
	runner.Start(context.Background(), 1)
	runner.Wait()
	stats := runner.Stats.SnapshotData()
	if stats.Failed != 1 || stats.Pending != 0 {
		t.Fatalf("expected timeout to fail lead, got %+v", stats)
	}
}

func TestRequeueDoesNotLeakOnCancelledContext(t *testing.T) {
	cfg, _ := buildTestConfig(t)
	logWriter, _ := logging.NewWriter(cfg.Paths.LogsDir)
	logWriter.Start()
	defer logWriter.Stop()
	runner, err := NewRunner(cfg, []config.SMTPAccount{{Host: "smtp.example.com", Port: 25, MailFrom: "bounce@example.com", ID: "smtp-1"}}, logWriter, state.NewManager(cfg.Paths.StateDir))
	if err != nil {
		t.Fatalf("runner: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	runner.ctx = ctx
	runner.cancel = cancel
	runner.Jobs = make(chan Job, 1)
	runner.Jobs <- Job{Email: "filled", Deadline: time.Now().Add(time.Second)}
	cancel()
	runner.requeue(Job{Email: "retry@example.com", Deadline: time.Now().Add(50 * time.Millisecond)}, 5*time.Millisecond)
	done := make(chan struct{})
	go func() {
		runner.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatalf("requeue did not complete after cancellation")
	}
}

func TestSMTPTestResultJSONShape(t *testing.T) {
	res := SMTPTestResult{ID: "smtp-1", Host: "host", OriginalHost: "orig", CandidateHost: "cand", Port: 587, LatencyMS: 12, TLSMode: "starttls", TLSVersion: "TLS1.3", OK: true, ErrorClass: "hostname", CertDNSNames: []string{"a"}}
	raw, err := json.Marshal(res)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded map[string]interface{}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, key := range []string{"id", "host", "original_host", "port", "latency_ms", "tls_mode", "tls_version", "ok", "candidate_host", "error_class"} {
		if _, ok := decoded[key]; !ok {
			t.Fatalf("missing key %s", key)
		}
	}
}

func TestSMTPHostnameMismatchEnumeratesCandidates(t *testing.T) {
	origProbe := smtpProbeFn
	defer func() { smtpProbeFn = origProbe }()

	first := true
	smtpProbeFn = func(acc config.SMTPAccount, host string, timeout time.Duration, sendTest bool, rcpt string) (SMTPTestResult, error) {
		if first {
			first = false
			cert := &x509.Certificate{DNSNames: []string{"postassl.it"}}
			return SMTPTestResult{ID: acc.ID, Host: host}, x509.HostnameError{Certificate: cert, Host: host}
		}
		return SMTPTestResult{ID: acc.ID, Host: host, CandidateHost: host, OriginalHost: acc.Host, OK: true}, nil
	}

	acc := config.SMTPAccount{Host: "orig", Port: 587, MailFrom: "bounce@example.com", ID: "smtp-1"}
	res := TestSMTP(acc, time.Second, "dest@example.com")
	if len(res) < 2 {
		t.Fatalf("expected candidate results, got %d", len(res))
	}
	if res[1].CandidateHost == "" {
		t.Fatalf("expected candidate host to be set")
	}
	if res[1].OriginalHost != "orig" {
		t.Fatalf("expected original host propagation")
	}
	if res[1].Host != res[1].CandidateHost {
		t.Fatalf("expected host to reflect candidate test host")
	}
}

func TestSMTPTestRespectsRecipientOverride(t *testing.T) {
	origProbe := smtpProbeFn
	defer func() { smtpProbeFn = origProbe }()
	captured := ""
	smtpProbeFn = func(acc config.SMTPAccount, host string, timeout time.Duration, sendTest bool, rcpt string) (SMTPTestResult, error) {
		captured = rcpt
		return SMTPTestResult{ID: acc.ID, Host: host, OK: true}, nil
	}
	acc := config.SMTPAccount{Host: "orig", Port: 587, MailFrom: "bounce@example.com", ID: "smtp-1"}
	_ = TestSMTP(acc, time.Second, "recipient@example.com")
	if captured != "recipient@example.com" {
		t.Fatalf("expected recipient override to propagate, got %s", captured)
	}
}

func TestExtractCertDNSNamesCoversVerificationError(t *testing.T) {
	cert := &x509.Certificate{DNSNames: []string{"a.example.com", "b.example.com"}}
	err := &tls.CertificateVerificationError{UnverifiedCertificates: []*x509.Certificate{cert}, Err: x509.HostnameError{Certificate: cert, Host: "c.example.com"}}
	names := extractCertDNSNames(err)
	if len(names) != 2 {
		t.Fatalf("expected dns names, got %v", names)
	}
	seen := make(map[string]struct{})
	for _, n := range names {
		seen[n] = struct{}{}
	}
	if _, ok := seen["a.example.com"]; !ok {
		t.Fatalf("expected a.example.com in names")
	}
	if _, ok := seen["b.example.com"]; !ok {
		t.Fatalf("expected b.example.com in names")
	}
}

func TestHostRotationBatchSticky(t *testing.T) {
	cfg, _ := buildTestConfig(t)
	cfg.Batch.BatchSize = 1
	account := config.SMTPAccount{Host: "primary", Port: 25, MailFrom: "bounce@example.com", ID: "smtp-1", Hosts: []string{"primary", "alt"}, Rotation: config.HostnameRotation{Enabled: true, BatchSize: 2}}
	logWriter, _ := logging.NewWriter(cfg.Paths.LogsDir)
	runner, err := NewRunner(cfg, []config.SMTPAccount{account}, logWriter, state.NewManager(cfg.Paths.StateDir))
	if err != nil {
		t.Fatalf("runner: %v", err)
	}
	ws := runner.newWorkerState(account)
	host1, _ := runner.selectHost(ws)
	if host1 != "primary" {
		t.Fatalf("expected first host primary, got %s", host1)
	}
	runner.onHostSuccess(ws)
	hostAgain, _ := runner.selectHost(ws)
	if hostAgain != "primary" {
		t.Fatalf("expected batch stickiness, got %s", hostAgain)
	}
	runner.onHostSuccess(ws)
	host2, _ := runner.selectHost(ws)
	if host2 == hostAgain {
		t.Fatalf("expected rotation after batch completion")
	}
}

func TestAllowlistCaseInsensitive(t *testing.T) {
	cfg, _ := buildTestConfig(t)
	cfg.DomainsAllowlist = []string{"Example.COM"}
	logWriter, _ := logging.NewWriter(cfg.Paths.LogsDir)
	logWriter.Start()
	defer logWriter.Stop()
	runner, err := NewRunner(cfg, []config.SMTPAccount{{Host: "smtp.example.com", Port: 25, MailFrom: "bounce@example.com", ID: "smtp-1"}}, logWriter, state.NewManager(cfg.Paths.StateDir))
	if err != nil {
		t.Fatalf("runner: %v", err)
	}
	if !runner.allowed("user@example.com") {
		t.Fatalf("expected allowlist to be case-insensitive")
	}
}

func TestResolveSubScriptPathFallsBackToExecutableDir(t *testing.T) {
	oldExec := executablePath
	t.Cleanup(func() { executablePath = oldExec })

	wd, _ := os.Getwd()
	tempDir := t.TempDir()
	if err := os.Chdir(tempDir); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(wd) })

	scriptDir := t.TempDir()
	scriptPath := filepath.Join(scriptDir, "sub.py")
	if err := os.WriteFile(scriptPath, []byte("#!/bin/sh\necho a\n"), 0o755); err != nil {
		t.Fatalf("write script: %v", err)
	}
	executablePath = func() (string, error) {
		return filepath.Join(scriptDir, "zessen-go"), nil
	}

	resolved := resolveSubScriptPath()
	if resolved != scriptPath {
		t.Fatalf("expected %s, got %s", scriptPath, resolved)
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

func TestEnumerateCandidatesUsesScript(t *testing.T) {
	wd, _ := os.Getwd()
	tempDir := t.TempDir()
	if err := os.Chdir(tempDir); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(wd) })

	scriptPath := filepath.Join(tempDir, "sub.py")
	script := "#!/bin/sh\necho Subdomains saved to $1.txt\necho host1.example.com > $1.txt\necho host2.example.com >> $1.txt\necho not-a-host\n"
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write script: %v", err)
	}

	got := enumerateCandidates("example.com")
	want := []string{"host1.example.com", "host2.example.com"}
	if len(got) != len(want) {
		t.Fatalf("unexpected candidate count: %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("candidate %d: want %s, got %s", i, want[i], got[i])
		}
	}
}
