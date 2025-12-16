package worker

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/net/publicsuffix"

	"zessen-go/internal/config"
	"zessen-go/internal/logging"
	"zessen-go/internal/metrics"
	smtps "zessen-go/internal/smtp"
	"zessen-go/internal/state"
)

var executablePath = os.Executable
var enumerateCandidatesFn = enumerateCandidates

// Job represents a unit of work for a lead.
type Job struct {
	Email    string
	Attempt  int
	Deadline time.Time
	BatchIdx int
}

// WorkerState tracks SMTP circuit breaker info.
type WorkerState struct {
	Account        config.SMTPAccount
	Status         string
	FailCount      int
	Cooldowns      int
	Session        *smtps.Session
	CooldownUntil  time.Time
	SentCount      int
	HostPool       []string
	HostState      map[string]*HostStatus
	HostIndex      int
	BatchRemaining int
	mu             sync.Mutex
}

// HostStatus tracks per-host circuit breaker info.
type HostStatus struct {
	Status        string
	FailCount     int
	Cooldowns     int
	CooldownUntil time.Time
}

// Runner handles job queue and workers.
type Runner struct {
	Cfg              config.Config
	Jobs             chan Job
	Stats            *metrics.Stats
	Log              *logging.Writer
	State            *state.Manager
	Accounts         []config.SMTPAccount
	MaxAttempts      int
	Backoff          []time.Duration
	StopCh           chan struct{}
	ResumeSnapshot   state.Snapshot
	BatchID          string
	DryRun           bool
	DomainsAllowlist map[string]struct{}
	LimitPerSMTP     int
	attempts         map[string]int
	done             map[string]bool
	smtpState        map[string]*state.SMTPState
	progressMu       sync.Mutex
	batchCursor      int64
	ctx              context.Context
	cancel           context.CancelFunc
	wg               sync.WaitGroup
	stopOnce         sync.Once
	TestFailOnce     bool
	templates        map[string]string
	jobTimeout       time.Duration
	dryRunDelay      time.Duration
}

// NewRunner constructs a runner.
func NewRunner(cfg config.Config, accounts []config.SMTPAccount, log *logging.Writer, st *state.Manager) (*Runner, error) {
	backoff := config.BackoffDurations(cfg.Retry.BackoffSeconds)
	doms := make(map[string]struct{})
	for _, d := range cfg.DomainsAllowlist {
		doms[strings.ToLower(d)] = struct{}{}
	}
	templates := make(map[string]string)
	for key, profile := range cfg.Profiles {
		path := config.ResolveTemplatePath(cfg.Paths.TemplatesDir, profile.TemplateFile)
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("load template %s: %w", key, err)
		}
		templates[key] = string(data)
	}
	ctx, cancel := context.WithCancel(context.Background())
	batchCursor := int64(0)
	if cfg.Batch.BatchSize <= 0 {
		cfg.Batch.BatchSize = 1
	}
	return &Runner{
		Cfg:              cfg,
		Jobs:             make(chan Job, 1024),
		Stats:            &metrics.Stats{},
		Log:              log,
		State:            st,
		Accounts:         accounts,
		MaxAttempts:      cfg.Retry.MaxAttemptsPerLead,
		Backoff:          backoff,
		StopCh:           make(chan struct{}),
		BatchID:          fmt.Sprintf("batch-%d", time.Now().Unix()),
		DomainsAllowlist: doms,
		attempts:         make(map[string]int),
		done:             make(map[string]bool),
		smtpState:        make(map[string]*state.SMTPState),
		batchCursor:      batchCursor,
		ctx:              ctx,
		cancel:           cancel,
		templates:        templates,
		jobTimeout:       time.Duration(cfg.Timeouts.JobTimeoutSeconds) * time.Second,
		dryRunDelay:      10 * time.Millisecond,
	}, nil
}

// Enqueue leads respecting resume information.
func (r *Runner) Enqueue(leads []string, resume state.Snapshot) {
	r.progressMu.Lock()
	defer r.progressMu.Unlock()
	if resume.BatchID != "" {
		r.BatchID = resume.BatchID
	}
	if resume.Stats.Sent+resume.Stats.Failed > 0 {
		r.batchCursor = resume.Stats.Sent + resume.Stats.Failed
	}
	pending := int64(0)
	total := int64(len(leads))
	for email, st := range resume.Leads {
		r.attempts[email] = st.Attempts
		if st.Done {
			r.done[email] = true
		}
	}
	for _, smtp := range resume.SMTPs {
		copy := smtp
		r.smtpState[smtp.ID] = &copy
	}
	for _, lead := range leads {
		if r.done[lead] {
			continue
		}
		attempt := r.attempts[lead]
		r.attempts[lead] = attempt
		batchIdx := int(r.batchCursor / int64(r.Cfg.Batch.BatchSize))
		r.batchCursor++
		r.enqueueJob(Job{Email: lead, Attempt: attempt, Deadline: time.Now().Add(r.jobTimeout), BatchIdx: batchIdx})
		pending++
	}
	r.Stats.SetPending(pending, total)
	if resume.Stats.Total > 0 {
		atomic.StoreInt64(&r.Stats.Sent, resume.Stats.Sent)
		atomic.StoreInt64(&r.Stats.Failed, resume.Stats.Failed)
		atomic.StoreInt64(&r.Stats.Healthy, resume.Stats.Healthy)
		atomic.StoreInt64(&r.Stats.Cooldown, resume.Stats.Cooldown)
		atomic.StoreInt64(&r.Stats.Disabled, resume.Stats.Disabled)
		atomic.StoreInt64(&r.Stats.LatencySum, resume.Stats.LatencySum)
		atomic.StoreInt64(&r.Stats.LatencyCount, resume.Stats.LatencyCount)
	}
	atomic.StoreInt64(&r.Stats.Pending, pending)
	atomic.StoreInt64(&r.Stats.Total, total)
	r.ResumeSnapshot = resume
}

func (r *Runner) enqueueJob(job Job) {
	r.wg.Add(1)
	r.Jobs <- job
}

// Start launches worker goroutines.
func (r *Runner) Start(ctx context.Context, maxWorkers int) {
	r.ctx = ctx
	for i := 0; i < maxWorkers && i < len(r.Accounts); i++ {
		account := r.Accounts[i]
		ws := r.newWorkerState(account)
		r.progressMu.Lock()
		r.smtpState[account.ID] = &state.SMTPState{ID: account.ID, Status: "healthy"}
		r.progressMu.Unlock()
		go r.workerLoop(ws)
	}
	r.updateSMTPCounts()
}

func (r *Runner) newWorkerState(account config.SMTPAccount) *WorkerState {
	pool := account.Hosts
	if len(pool) == 0 {
		pool = []string{account.Host}
	}
	st := make(map[string]*HostStatus)
	for _, h := range pool {
		st[h] = &HostStatus{Status: "healthy"}
	}
	batchSize := r.Cfg.Batch.BatchSize
	if account.Rotation.Enabled && account.Rotation.BatchSize > 0 {
		batchSize = account.Rotation.BatchSize
	}
	return &WorkerState{Account: account, Status: "healthy", HostPool: pool, HostState: st, BatchRemaining: batchSize}
}

func (r *Runner) workerLoop(ws *WorkerState) {
	for {
		select {
		case job, ok := <-r.Jobs:
			if !ok {
				return
			}
			r.handleJob(ws, job)
		case <-r.StopCh:
			return
		}
	}
}

func (r *Runner) handleJob(ws *WorkerState, job Job) {
	done := true
	defer func() {
		if done {
			r.wg.Done()
		}
	}()
	if job.Deadline.IsZero() {
		job.Deadline = time.Now().Add(r.jobTimeout)
	}
	jobCtx, cancel := context.WithDeadline(r.ctx, job.Deadline)
	defer cancel()
	if err := jobCtx.Err(); err != nil {
		r.failJobFinal(job, reasonFromContext(err))
		return
	}
	select {
	case <-jobCtx.Done():
		r.failJobFinal(job, reasonFromContext(jobCtx.Err()))
		return
	default:
	}
	if !r.allowed(job.Email) {
		r.failJobFinal(job, "domain not allowed")
		return
	}
	if r.LimitPerSMTP > 0 && r.sentCount(ws) >= r.LimitPerSMTP {
		r.disableSMTP(ws)
		r.failJobFinal(job, "smtp send limit reached")
		return
	}
	host, wait := r.selectHost(ws)
	if wait > 0 {
		if time.Until(job.Deadline) <= wait {
			r.failJobFinal(job, "job timeout")
			return
		}
		r.requeue(job, wait)
		return
	}
	status, wait := r.smtpStatus(ws)
	if status == "disabled" {
		r.failJobFinal(job, "smtp disabled")
		return
	}
	if status == "cooldown" && wait > 0 {
		if time.Until(job.Deadline) <= wait {
			r.failJobFinal(job, "job timeout")
			return
		}
		r.requeue(job, wait)
		return
	}
	attempt := job.Attempt + 1
	r.recordAttempt(job.Email, attempt)
	var session *smtps.Session
	var err error
	if !r.DryRun {
		session, err = r.ensureSession(ws, host)
		if err != nil {
			r.handleFailure(ws, job, err, jobCtx)
			return
		}
	}
	profileKey := config.SelectProfileForBatch(job.BatchIdx)
	profile := r.Cfg.Profiles[profileKey]
	msgID := r.messageID(ws.Account)
	headers := map[string]string{
		"Message-ID":    msgID,
		"X-Campaign-ID": r.Cfg.CampaignID,
		"X-Batch-ID":    r.BatchID,
		"X-SMTP-ID":     ws.Account.ID,
		"X-Mailer":      profile.XMailer,
		"X-Trace-ID":    fmt.Sprintf("trace-%d", time.Now().UnixNano()),
	}
	subject := profile.SubjectPool[rand.Intn(len(profile.SubjectPool))]
	fromName := profile.FromNamePool[rand.Intn(len(profile.FromNamePool))]
	body := r.templates[profileKey]
	msg := smtps.BuildMessage(fromName, ws.Account.MailFrom, job.Email, subject, body, headers)
	start := time.Now()
	if r.DryRun {
		delay := r.dryRunDelay
		if delay <= 0 {
			delay = 10 * time.Millisecond
		}
		select {
		case <-jobCtx.Done():
			r.failJobFinal(job, reasonFromContext(jobCtx.Err()))
			return
		case <-time.After(delay):
		}
	} else {
		if err := jobCtx.Err(); err != nil {
			r.failJobFinal(job, reasonFromContext(err))
			return
		}
		err = session.SendEmail(job.Email, msg, time.Duration(r.Cfg.Timeouts.SendTimeoutSeconds)*time.Second)
	}
	if r.TestFailOnce && job.Attempt == 0 {
		err = fmt.Errorf("simulated failure")
		r.TestFailOnce = false
	}
	latency := time.Since(start)
	if err != nil {
		r.invalidateSession(ws)
		r.handleFailure(ws, job, err, jobCtx)
		return
	}
	if err := jobCtx.Err(); err != nil {
		r.invalidateSession(ws)
		r.failJobFinal(job, reasonFromContext(err))
		return
	}
	r.onSuccess(ws)
	r.onHostSuccess(ws)
	r.markLeadDone(job.Email, attempt)
	r.Stats.AddSent(latency)
	r.Log.Publish("sent.log", logging.Event{Type: "sent", Message: "delivered", Data: map[string]interface{}{"email": job.Email, "smtp": ws.Account.ID, "host": host, "latency_ms": latency.Milliseconds(), "profile": profileKey}})
}

func (r *Runner) allowed(email string) bool {
	if len(r.DomainsAllowlist) == 0 {
		return false
	}
	parts := strings.Split(email, "@")
	if len(parts) != 2 {
		return false
	}
	domain := strings.ToLower(parts[1])
	_, ok := r.DomainsAllowlist[domain]
	return ok
}

func (r *Runner) hostBatchSize(ws *WorkerState) int {
	if ws.Account.Rotation.Enabled && ws.Account.Rotation.BatchSize > 0 {
		return ws.Account.Rotation.BatchSize
	}
	return r.Cfg.Batch.BatchSize
}

func (r *Runner) selectHost(ws *WorkerState) (string, time.Duration) {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	if len(ws.HostPool) == 0 {
		return ws.Account.Host, 0
	}
	now := time.Now()
	// batch rotation
	if ws.BatchRemaining <= 0 {
		ws.HostIndex = (ws.HostIndex + 1) % len(ws.HostPool)
		ws.BatchRemaining = r.hostBatchSize(ws)
		if ws.Session != nil {
			ws.Session.Close()
			ws.Session = nil
		}
	}
	// find healthy host
	for i := 0; i < len(ws.HostPool); i++ {
		idx := (ws.HostIndex + i) % len(ws.HostPool)
		host := ws.HostPool[idx]
		hs, ok := ws.HostState[host]
		if !ok {
			hs = &HostStatus{Status: "healthy"}
			ws.HostState[host] = hs
		}
		if hs.Status == "disabled" {
			continue
		}
		if hs.Status == "cooldown" && hs.CooldownUntil.After(now) {
			continue
		}
		if hs.Status == "cooldown" && !hs.CooldownUntil.After(now) {
			hs.Status = "healthy"
			hs.FailCount = 0
		}
		ws.HostIndex = idx
		if ws.BatchRemaining <= 0 {
			ws.BatchRemaining = r.hostBatchSize(ws)
		}
		ws.Account.Host = host
		return host, 0
	}
	// otherwise wait for earliest cooldown
	wait := time.Duration(0)
	for _, hs := range ws.HostState {
		if hs.Status == "cooldown" && hs.CooldownUntil.After(now) {
			if wait == 0 || hs.CooldownUntil.Sub(now) < wait {
				wait = hs.CooldownUntil.Sub(now)
			}
		}
	}
	if wait == 0 {
		wait = time.Second
	}
	return ws.Account.Host, wait
}

func (r *Runner) ensureSession(ws *WorkerState, host string) (*smtps.Session, error) {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	if ws.Session != nil && ws.Account.Host == host {
		return ws.Session, nil
	}
	if ws.Session != nil {
		ws.Session.Close()
		ws.Session = nil
	}
	account := smtps.Account{Host: host, Port: ws.Account.Port, User: ws.Account.User, Password: ws.Account.Password, MailFrom: ws.Account.MailFrom, ID: ws.Account.ID}
	session, err := smtps.Dial(account, time.Duration(r.Cfg.Timeouts.ConnectTimeoutSeconds)*time.Second)
	if err != nil {
		return nil, err
	}
	ws.Account.Host = host
	ws.Session = session
	return session, nil
}

func (r *Runner) handleFailure(ws *WorkerState, job Job, err error, jobCtx context.Context) {
	r.Log.Publish("failed.log", logging.Event{Type: "failed", Message: err.Error(), Data: map[string]string{"email": job.Email, "smtp": ws.Account.ID}})
	ws.mu.Lock()
	ws.FailCount++
	ws.mu.Unlock()
	r.recordHostFailure(ws)
	r.updateCircuit(ws)
	if job.Attempt >= r.MaxAttempts-1 {
		r.Stats.AddFailed()
		r.markLeadDone(job.Email, job.Attempt+1)
		return
	}
	select {
	case <-jobCtx.Done():
		r.failJobFinal(job, reasonFromContext(jobCtx.Err()))
		return
	default:
	}
	backoff := r.Backoff[min(len(r.Backoff)-1, job.Attempt)]
	jitter := time.Duration(rand.Intn(200)) * time.Millisecond
	delay := backoff + jitter
	if time.Until(job.Deadline) <= delay {
		r.failJobFinal(job, "job timeout")
		return
	}
	r.requeue(Job{Email: job.Email, Attempt: job.Attempt + 1, Deadline: job.Deadline, BatchIdx: job.BatchIdx}, delay)
}

func (r *Runner) recordHostFailure(ws *WorkerState) {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	host := ws.Account.Host
	hs, ok := ws.HostState[host]
	if !ok {
		hs = &HostStatus{Status: "healthy"}
		ws.HostState[host] = hs
	}
	hs.FailCount++
	if hs.FailCount >= r.Cfg.CircuitBreaker.FailThreshold {
		hs.Cooldowns++
		hs.FailCount = 0
		hs.Status = "cooldown"
		hs.CooldownUntil = time.Now().Add(time.Duration(r.Cfg.CircuitBreaker.CooldownSeconds) * time.Second)
		if hs.Cooldowns >= r.Cfg.CircuitBreaker.DisableAfterCooldowns {
			hs.Status = "disabled"
		}
	}
}

func (r *Runner) updateCircuit(ws *WorkerState) {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	now := time.Now()
	statePtr := r.getSMTPState(ws.Account.ID)
	if ws.FailCount >= r.Cfg.CircuitBreaker.FailThreshold && ws.Status != "disabled" {
		ws.Status = "cooldown"
		ws.Cooldowns++
		ws.CooldownUntil = now.Add(time.Duration(r.Cfg.CircuitBreaker.CooldownSeconds) * time.Second)
		statePtr.Status = ws.Status
		statePtr.FailCountConsec = ws.FailCount
		statePtr.CooldownsTriggered = ws.Cooldowns
		statePtr.CooldownUntil = ws.CooldownUntil
		if ws.Cooldowns >= r.Cfg.CircuitBreaker.DisableAfterCooldowns {
			ws.Status = "disabled"
			statePtr.Status = "disabled"
		} else {
		}
	} else {
		statePtr.FailCountConsec = ws.FailCount
		statePtr.Status = ws.Status
	}
	r.updateSMTPCounts()
}

func (r *Runner) smtpStatus(ws *WorkerState) (string, time.Duration) {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	now := time.Now()
	if ws.Status == "cooldown" && now.Before(ws.CooldownUntil) {
		return ws.Status, ws.CooldownUntil.Sub(now)
	}
	if ws.Status == "cooldown" && !now.Before(ws.CooldownUntil) {
		ws.Status = "healthy"
		ws.FailCount = 0
		ws.CooldownUntil = time.Time{}
		r.setSMTPStatus(ws.Account.ID, ws.Status, ws.FailCount, ws.Cooldowns, ws.CooldownUntil)
	}
	return ws.Status, 0
}

func (r *Runner) setSMTPStatus(id, status string, failCount, cooldowns int, until time.Time) {
	r.progressMu.Lock()
	st, ok := r.smtpState[id]
	if !ok {
		st = &state.SMTPState{ID: id}
		r.smtpState[id] = st
	}
	st.Status = status
	st.FailCountConsec = failCount
	st.CooldownsTriggered = cooldowns
	st.CooldownUntil = until
	r.progressMu.Unlock()
	r.updateSMTPCounts()
}

func (r *Runner) getSMTPState(id string) *state.SMTPState {
	r.progressMu.Lock()
	defer r.progressMu.Unlock()
	st, ok := r.smtpState[id]
	if !ok {
		st = &state.SMTPState{ID: id, Status: "healthy"}
		r.smtpState[id] = st
	}
	return st
}

func (r *Runner) updateSMTPCounts() {
	r.progressMu.Lock()
	defer r.progressMu.Unlock()
	var healthy, cooldown, disabled int64
	for _, st := range r.smtpState {
		switch st.Status {
		case "cooldown":
			cooldown++
		case "disabled":
			disabled++
		default:
			healthy++
		}
	}
	r.Stats.SetSMTPState(healthy, cooldown, disabled)
}

func (r *Runner) invalidateSession(ws *WorkerState) {
	ws.mu.Lock()
	if ws.Session != nil {
		ws.Session.Close()
	}
	ws.Session = nil
	ws.mu.Unlock()
}

func (r *Runner) onSuccess(ws *WorkerState) {
	ws.mu.Lock()
	ws.FailCount = 0
	ws.Status = "healthy"
	ws.SentCount++
	ws.CooldownUntil = time.Time{}
	statePtr := r.getSMTPState(ws.Account.ID)
	statePtr.Status = ws.Status
	statePtr.FailCountConsec = ws.FailCount
	ws.mu.Unlock()
	r.updateSMTPCounts()
}

func (r *Runner) onHostSuccess(ws *WorkerState) {
	ws.mu.Lock()
	host := ws.Account.Host
	if hs, ok := ws.HostState[host]; ok {
		hs.FailCount = 0
		hs.Status = "healthy"
	}
	ws.BatchRemaining--
	ws.mu.Unlock()
}

func (r *Runner) recordAttempt(email string, attempt int) {
	r.progressMu.Lock()
	r.attempts[email] = attempt
	r.progressMu.Unlock()
}

func (r *Runner) markLeadDone(email string, attempt int) {
	r.progressMu.Lock()
	r.done[email] = true
	r.attempts[email] = attempt
	r.progressMu.Unlock()
}

func (r *Runner) requeue(job Job, delay time.Duration) {
	r.wg.Add(1)
	time.AfterFunc(delay, func() {
		if time.Now().After(job.Deadline) {
			r.failJobFinal(job, "job timeout")
			r.wg.Done()
			return
		}
		if err := r.ctx.Err(); err != nil {
			r.failJobFinal(job, reasonFromContext(err))
			r.wg.Done()
			return
		}
		select {
		case <-r.ctx.Done():
			r.failJobFinal(job, reasonFromContext(r.ctx.Err()))
			r.wg.Done()
			return
		case <-r.StopCh:
			r.wg.Done()
			return
		case r.Jobs <- job:
			return
		case <-time.After(time.Second):
			if r.ctx.Err() != nil {
				r.wg.Done()
				return
			}
			r.failJobFinal(job, "requeue timeout")
			r.wg.Done()
			return
		}
	})
}

func (r *Runner) failJobFinal(job Job, reason string) {
	r.Log.Publish("failed.log", logging.Event{Type: "failed", Message: reason, Data: map[string]string{"email": job.Email}})
	r.Stats.AddFailed()
	r.markLeadDone(job.Email, job.Attempt+1)
}

func reasonFromContext(err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return "job timeout"
	}
	if err != nil {
		return "run cancelled"
	}
	return ""
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// Wait blocks until all in-flight jobs and retries are complete.
func (r *Runner) Wait() {
	r.wg.Wait()
	r.stopOnce.Do(func() {
		close(r.Jobs)
		close(r.StopCh)
	})
}

func (r *Runner) messageID(account config.SMTPAccount) string {
	domain := account.Host
	parts := strings.Split(account.MailFrom, "@")
	if len(parts) == 2 {
		domain = parts[1]
	}
	return fmt.Sprintf("%d-%d@%s", time.Now().UnixNano(), rand.Int63(), domain)
}

func (r *Runner) sentCount(ws *WorkerState) int {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	return ws.SentCount
}

func (r *Runner) disableSMTP(ws *WorkerState) {
	ws.mu.Lock()
	ws.Status = "disabled"
	ws.mu.Unlock()
	r.setSMTPStatus(ws.Account.ID, "disabled", ws.FailCount, ws.Cooldowns, ws.CooldownUntil)
	r.updateSMTPCounts()
}

// Snapshot captures current progress for checkpointing.
func (r *Runner) Snapshot() (state.Snapshot, error) {
	r.progressMu.Lock()
	leads := make(map[string]state.LeadState, len(r.attempts))
	for email, attempts := range r.attempts {
		leads[email] = state.LeadState{Attempts: attempts, Done: r.done[email]}
	}
	smtps := make(map[string]state.SMTPState, len(r.smtpState))
	for id, st := range r.smtpState {
		copy := *st
		smtps[id] = copy
	}
	r.progressMu.Unlock()
	return state.Snapshot{
		CampaignID: r.Cfg.CampaignID,
		BatchID:    r.BatchID,
		Timestamp:  time.Now(),
		Leads:      leads,
		SMTPs:      smtps,
		Stats:      r.Stats.SnapshotData(),
	}, nil
}

// ValidateSMTP performs non intrusive test.
func ValidateSMTP(account config.SMTPAccount, timeout time.Duration) error {
	session, err := smtps.Dial(smtps.Account{Host: account.Host, Port: account.Port, User: account.User, Password: account.Password, MailFrom: account.MailFrom, ID: account.ID}, timeout)
	if err != nil {
		return err
	}
	session.Close()
	return nil
}

// SMTPTestResult captures diagnostic information for test-smtps.
type SMTPTestResult struct {
	ID              string   `json:"id"`
	Host            string   `json:"host"`
	OriginalHost    string   `json:"original_host,omitempty"`
	CandidateHost   string   `json:"candidate_host,omitempty"`
	CandidateSource string   `json:"candidate_source,omitempty"`
	Port            int      `json:"port"`
	LatencyMS       int64    `json:"latency_ms"`
	TLSMode         string   `json:"tls_mode"`
	TLSVersion      string   `json:"tls_version"`
	OK              bool     `json:"ok"`
	Error           string   `json:"error,omitempty"`
	ErrorClass      string   `json:"error_class,omitempty"`
	CertDNSNames    []string `json:"cert_dns_names,omitempty"`
}

// TestSMTP collects non-intrusive SMTP diagnostics and retries alternative hosts when SAN mismatch occurs.
var smtpProbeFn = runSMTPProbe

func TestSMTP(account config.SMTPAccount, timeout time.Duration, rcpt string) []SMTPTestResult {
	base, err := smtpProbeFn(account, account.Host, timeout, true, rcpt)
	base.CertDNSNames = filterCertDNSNames(base.CertDNSNames)
	results := []SMTPTestResult{base}
	if err == nil && base.OK {
		return results
	}

	rawDNSNames := extractCertDNSNames(err)
	dnsNames := filterCertDNSNames(rawDNSNames)
	if len(dnsNames) > 0 {
		fmt.Printf("cert valid for: %s\n", strings.Join(dnsNames, ","))
	}

	tried := map[string]struct{}{account.Host: {}}
	probeCandidate := func(host, source string) bool {
		if host == "" {
			return false
		}
		if _, ok := tried[host]; ok {
			return false
		}
		tried[host] = struct{}{}
		candAcc := account
		candAcc.Host = host
		candRes, _ := smtpProbeFn(candAcc, host, timeout, true, rcpt)
		candRes.CandidateHost = host
		candRes.CandidateSource = source
		candRes.CertDNSNames = dnsNames
		candRes.OriginalHost = account.Host
		candRes.Host = candAcc.Host
		results = append(results, candRes)
		return candRes.OK
	}

	for _, dns := range dnsNames {
		if probeCandidate(dns, "cert_dns") {
			return results
		}
	}

	if len(dnsNames) == 0 {
		return results
	}

	baseDomain := chooseBaseDomain(rawDNSNames)
	if baseDomain == "" {
		return results
	}

	for _, cand := range enumerateCandidatesFn(baseDomain) {
		if probeCandidate(cand, "subpy") {
			return results
		}
	}
	return results
}

func runSMTPProbe(account config.SMTPAccount, host string, timeout time.Duration, sendTest bool, rcpt string) (SMTPTestResult, error) {
	res := SMTPTestResult{ID: account.ID, Host: host, OriginalHost: account.Host, Port: account.Port}
	start := time.Now()
	session, err := smtps.Dial(smtps.Account{Host: host, Port: account.Port, User: account.User, Password: account.Password, MailFrom: account.MailFrom, ID: account.ID}, timeout)
	if err != nil {
		res.Error = err.Error()
		res.ErrorClass = classifySMTPErr(err)
		res.CertDNSNames = extractCertDNSNames(err)
		return res, err
	}
	res.LatencyMS = time.Since(start).Milliseconds()
	mode, version := session.TLSInfo()
	res.TLSMode = mode
	res.TLSVersion = version
	res.OK = true
	if sendTest {
		target := account.MailFrom
		if rcpt != "" {
			target = rcpt
		}
		msg := smtps.BuildMessage("Test", account.MailFrom, target, "SMTP connectivity test", "<p>smtp test</p>", map[string]string{})
		if err := session.SendEmail(target, msg, timeout); err != nil {
			res.OK = false
			res.Error = err.Error()
			session.Close()
			return res, err
		}
	}
	session.Close()
	return res, nil
}

func classifySMTPErr(err error) string {
	switch {
	case errors.As(err, &x509.HostnameError{}):
		return "hostname"
	case certVerifyHostnameErr(err):
		return "hostname"
	case errors.As(err, &tls.RecordHeaderError{}):
		return "tls_record"
	default:
		return "other"
	}
}

func certVerifyHostnameErr(err error) bool {
	var certErr *tls.CertificateVerificationError
	if !errors.As(err, &certErr) || certErr == nil {
		return false
	}
	var hostErr x509.HostnameError
	return errors.As(certErr.Err, &hostErr)
}

func extractCertDNSNames(err error) []string {
	var names []string

	add := func(list []string) {
		for _, n := range list {
			if n == "" {
				continue
			}
			names = append(names, n)
		}
	}

	var hostErr x509.HostnameError
	if errors.As(err, &hostErr) && hostErr.Certificate != nil {
		add(hostErr.Certificate.DNSNames)
	}

	var certErr *tls.CertificateVerificationError
	if errors.As(err, &certErr) && certErr != nil {
		for _, c := range certErr.UnverifiedCertificates {
			add(c.DNSNames)
		}
		var innerHost x509.HostnameError
		if errors.As(certErr.Err, &innerHost) && innerHost.Certificate != nil {
			add(innerHost.Certificate.DNSNames)
		}
	}

	if len(names) == 0 {
		return nil
	}

	seen := make(map[string]struct{})
	deduped := names[:0]
	for _, n := range names {
		if _, ok := seen[n]; ok {
			continue
		}
		seen[n] = struct{}{}
		deduped = append(deduped, n)
	}
	return deduped
}

func filterCertDNSNames(names []string) []string {
	seen := make(map[string]struct{})
	var filtered []string
	for _, n := range names {
		n = strings.ToLower(strings.TrimSpace(n))
		if n == "" || n == "localhost" {
			continue
		}
		if strings.Contains(n, "*") {
			continue
		}
		if ip := net.ParseIP(n); ip != nil {
			continue
		}
		if _, ok := seen[n]; ok {
			continue
		}
		seen[n] = struct{}{}
		filtered = append(filtered, n)
	}
	return filtered
}

func chooseBaseDomain(names []string) string {
	for _, name := range names {
		clean := strings.TrimPrefix(name, "*.")
		clean = strings.TrimSpace(clean)
		if clean == "" {
			continue
		}
		base, err := publicsuffix.EffectiveTLDPlusOne(clean)
		if err != nil {
			continue
		}
		if base != "" {
			return base
		}
	}
	return ""
}

func enumerateCandidates(domain string) []string {
	script := resolveSubScriptPath()
	if script == "" {
		return nil
	}
	cmd := exec.Command(script, domain)
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	_ = cmd.Run()

	seen := make(map[string]struct{})
	var hosts []string
	add := func(h string) {
		h = strings.TrimSpace(h)
		if !isDomainCandidate(h, domain) {
			return
		}
		if _, ok := seen[h]; ok {
			return
		}
		seen[h] = struct{}{}
		hosts = append(hosts, h)
	}

	outputFile := fmt.Sprintf("%s.txt", domain)
	for _, path := range []string{
		filepath.Join(filepath.Dir(script), outputFile),
		outputFile,
	} {
		if data, err := os.ReadFile(path); err == nil {
			for _, line := range strings.Split(string(data), "\n") {
				add(line)
			}
		}
	}

	for _, line := range strings.Split(strings.TrimSpace(stdout.String()), "\n") {
		add(line)
	}

	return hosts
}

func isDomainCandidate(host, domain string) bool {
	host = strings.ToLower(strings.TrimSpace(host))
	domain = strings.ToLower(strings.TrimSpace(domain))
	if host == "" || domain == "" {
		return false
	}
	return host == domain || strings.HasSuffix(host, "."+domain)
}

func resolveSubScriptPath() string {
	candidates := []string{"./sub.py"}
	if exe, err := executablePath(); err == nil {
		candidates = append(candidates, filepath.Join(filepath.Dir(exe), "sub.py"))
	}
	seen := make(map[string]struct{})
	for _, c := range candidates {
		if c == "" {
			continue
		}
		if _, ok := seen[c]; ok {
			continue
		}
		seen[c] = struct{}{}
		info, err := os.Stat(c)
		if err != nil || info.IsDir() {
			continue
		}
		return c
	}
	return ""
}

type ValidatedOverride struct {
	Host string `json:"host"`
	Port int    `json:"port,omitempty"`
}

func LoadValidatedOverrides(path string) (map[string]ValidatedOverride, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return map[string]ValidatedOverride{}, nil
		}
		return nil, err
	}
	var overrides map[string]ValidatedOverride
	if err := json.Unmarshal(data, &overrides); err != nil {
		return nil, err
	}
	if overrides == nil {
		overrides = map[string]ValidatedOverride{}
	}
	return overrides, nil
}

func SaveValidatedOverrides(path string, overrides map[string]ValidatedOverride) error {
	if overrides == nil {
		overrides = map[string]ValidatedOverride{}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}

	keys := make([]string, 0, len(overrides))
	for k := range overrides {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var buf bytes.Buffer
	buf.WriteString("{\n")
	for i, k := range keys {
		entry := overrides[k]
		raw, err := json.Marshal(entry)
		if err != nil {
			return err
		}
		buf.WriteString(fmt.Sprintf("  %q: %s", k, string(raw)))
		if i < len(keys)-1 {
			buf.WriteString(",")
		}
		buf.WriteString("\n")
	}
	buf.WriteString("}\n")
	return os.WriteFile(path, buf.Bytes(), 0o644)
}
