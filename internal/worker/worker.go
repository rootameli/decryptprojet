package worker

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"zessen-go/internal/config"
	"zessen-go/internal/logging"
	"zessen-go/internal/metrics"
	smtps "zessen-go/internal/smtp"
	"zessen-go/internal/state"
)

// Job represents a unit of work for a lead.
type Job struct {
	Email   string
	Attempt int
}

// WorkerState tracks SMTP circuit breaker info.
type WorkerState struct {
	Account       config.SMTPAccount
	Status        string
	FailCount     int
	Cooldowns     int
	Session       *smtps.Session
	CooldownUntil time.Time
	SentCount     int
	mu            sync.Mutex
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
	ctx              context.Context
	cancel           context.CancelFunc
	wg               sync.WaitGroup
	stopOnce         sync.Once
	TestFailOnce     bool
	templates        map[string]string
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
		ctx:              ctx,
		cancel:           cancel,
		templates:        templates,
	}, nil
}

// Enqueue leads respecting resume information.
func (r *Runner) Enqueue(leads []string, resume state.Snapshot) {
	r.progressMu.Lock()
	defer r.progressMu.Unlock()
	if resume.BatchID != "" {
		r.BatchID = resume.BatchID
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
		r.enqueueJob(Job{Email: lead, Attempt: attempt})
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
		ws := &WorkerState{Account: account, Status: "healthy"}
		r.progressMu.Lock()
		r.smtpState[account.ID] = &state.SMTPState{ID: account.ID, Status: "healthy"}
		r.progressMu.Unlock()
		go r.workerLoop(ws)
	}
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
	select {
	case <-r.ctx.Done():
		r.failJobFinal(job, "run cancelled")
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
	status, wait := r.smtpStatus(ws)
	if status == "disabled" {
		r.failJobFinal(job, "smtp disabled")
		return
	}
	if status == "cooldown" && wait > 0 {
		r.requeue(job, wait)
		return
	}
	attempt := job.Attempt + 1
	r.recordAttempt(job.Email, attempt)
	var session *smtps.Session
	var err error
	if !r.DryRun {
		session, err = r.ensureSession(ws)
		if err != nil {
			r.handleFailure(ws, job, err)
			return
		}
	}
	snap := r.Stats.Snapshot()
	batchIndex := int((snap.Sent + snap.Failed) / int64(r.Cfg.Batch.BatchSize))
	profileKey := config.SelectProfileForBatch(batchIndex)
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
		time.Sleep(10 * time.Millisecond)
	} else {
		err = session.SendEmail(job.Email, msg, time.Duration(r.Cfg.Timeouts.SendTimeoutSeconds)*time.Second)
	}
	if r.TestFailOnce && job.Attempt == 0 {
		err = fmt.Errorf("simulated failure")
		r.TestFailOnce = false
	}
	latency := time.Since(start)
	if err != nil {
		r.invalidateSession(ws)
		r.handleFailure(ws, job, err)
		return
	}
	r.onSuccess(ws)
	r.markLeadDone(job.Email, attempt)
	r.Stats.AddSent(latency)
	r.Log.Publish("sent.log", logging.Event{Type: "sent", Message: "delivered", Data: map[string]interface{}{"email": job.Email, "smtp": ws.Account.ID, "latency_ms": latency.Milliseconds()}})
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

func (r *Runner) ensureSession(ws *WorkerState) (*smtps.Session, error) {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	if ws.Session != nil {
		return ws.Session, nil
	}
	account := smtps.Account{Host: ws.Account.Host, Port: ws.Account.Port, User: ws.Account.User, Password: ws.Account.Password, MailFrom: ws.Account.MailFrom, ID: ws.Account.ID}
	session, err := smtps.Dial(account, time.Duration(r.Cfg.Timeouts.ConnectTimeoutSeconds)*time.Second)
	if err != nil {
		return nil, err
	}
	ws.Session = session
	return session, nil
}

func (r *Runner) handleFailure(ws *WorkerState, job Job, err error) {
	r.Log.Publish("failed.log", logging.Event{Type: "failed", Message: err.Error(), Data: map[string]string{"email": job.Email, "smtp": ws.Account.ID}})
	ws.mu.Lock()
	ws.FailCount++
	ws.mu.Unlock()
	r.updateCircuit(ws)
	if job.Attempt >= r.MaxAttempts-1 {
		r.Stats.AddFailed()
		r.markLeadDone(job.Email, job.Attempt+1)
		return
	}
	select {
	case <-r.ctx.Done():
		r.failJobFinal(job, "run cancelled")
		return
	default:
	}
	backoff := r.Backoff[min(len(r.Backoff)-1, job.Attempt)]
	jitter := time.Duration(rand.Intn(200)) * time.Millisecond
	r.requeue(Job{Email: job.Email, Attempt: job.Attempt + 1}, backoff+jitter)
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
			r.Stats.AddDisabled()
		} else {
			r.Stats.AddCooldown()
		}
	} else {
		statePtr.FailCountConsec = ws.FailCount
		statePtr.Status = ws.Status
	}
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
	defer r.progressMu.Unlock()
	st, ok := r.smtpState[id]
	if !ok {
		st = &state.SMTPState{ID: id}
		r.smtpState[id] = st
	}
	st.Status = status
	st.FailCountConsec = failCount
	st.CooldownsTriggered = cooldowns
	st.CooldownUntil = until
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
		if err := r.ctx.Err(); err != nil {
			r.wg.Done()
			return
		}
		select {
		case <-r.ctx.Done():
			r.wg.Done()
			return
		case <-r.StopCh:
			r.wg.Done()
			return
		case r.Jobs <- job:
		}
	})
}

func (r *Runner) failJobFinal(job Job, reason string) {
	r.Log.Publish("failed.log", logging.Event{Type: "failed", Message: reason, Data: map[string]string{"email": job.Email}})
	r.Stats.AddFailed()
	r.markLeadDone(job.Email, job.Attempt+1)
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
	r.Stats.AddDisabled()
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
