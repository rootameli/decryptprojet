package worker

import (
	"fmt"
	"math/rand"
	"os"
	"strings"
	"sync"
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
	Account   config.SMTPAccount
	Status    string
	FailCount int
	Cooldowns int
	Session   *smtps.Session
	mu        sync.Mutex
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
	DurationLimit    time.Duration
	wg               sync.WaitGroup
	TestFailOnce     bool
}

// NewRunner constructs a runner.
func NewRunner(cfg config.Config, accounts []config.SMTPAccount, log *logging.Writer, st *state.Manager) *Runner {
	backoff := config.BackoffDurations(cfg.Retry.BackoffSeconds)
	doms := make(map[string]struct{})
	for _, d := range cfg.DomainsAllowlist {
		doms[d] = struct{}{}
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
	}
}

// Enqueue leads respecting resume information.
func (r *Runner) Enqueue(leads []string, resume state.Snapshot) {
	pending := int64(0)
	doneSet := make(map[string]bool)
	for _, l := range resume.Leads {
		if l.Done {
			doneSet[l.Email] = true
		}
	}
	for _, lead := range leads {
		if doneSet[lead] {
			continue
		}
		r.enqueueJob(Job{Email: lead, Attempt: 0})
		pending++
	}
	r.Stats.SetPending(pending)
	r.ResumeSnapshot = resume
}

func (r *Runner) enqueueJob(job Job) {
	r.wg.Add(1)
	r.Jobs <- job
}

// Start launches worker goroutines.
func (r *Runner) Start(maxWorkers int) {
	for i := 0; i < maxWorkers && i < len(r.Accounts); i++ {
		account := r.Accounts[i]
		ws := &WorkerState{Account: account, Status: "healthy"}
		go r.workerLoop(ws)
	}
}

func (r *Runner) workerLoop(ws *WorkerState) {
	for {
		select {
		case job := <-r.Jobs:
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
	if !r.allowed(job.Email) {
		r.Log.Publish("failed.log", logging.Event{Type: "failed", Message: "domain not allowed", Data: map[string]string{"email": job.Email}})
		r.Stats.AddFailed()
		return
	}
	if ws.Status == "disabled" {
		// requeue job with different worker
		done = false
		r.Jobs <- job
		return
	}
	var session *smtps.Session
	var err error
	if !r.DryRun {
		session, err = r.ensureSession(ws)
		if err != nil {
			r.onFailure(ws, job, err)
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
	templatePath := config.ResolveTemplatePath(r.Cfg.Paths.TemplatesDir, profile.TemplateFile)
	bodyBytes, _ := os.ReadFile(templatePath)
	body := string(bodyBytes)
	msg := smtps.BuildMessage(fromName, ws.Account.MailFrom, job.Email, subject, body, headers)
	start := time.Now()
	if r.DryRun {
		// simulate send
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
		r.onFailure(ws, job, err)
		return
	}
	ws.FailCount = 0
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
	domain := parts[1]
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

func (r *Runner) onFailure(ws *WorkerState, job Job, err error) {
	ws.FailCount++
	if ws.FailCount >= r.Cfg.CircuitBreaker.FailThreshold {
		ws.Status = "cooldown"
		ws.Cooldowns++
		r.Stats.AddCooldown()
		go func() {
			time.Sleep(time.Duration(r.Cfg.CircuitBreaker.CooldownSeconds) * time.Second)
			if ws.Cooldowns >= r.Cfg.CircuitBreaker.DisableAfterCooldowns {
				ws.Status = "disabled"
				r.Stats.AddDisabled()
			} else {
				ws.FailCount = 0
				ws.Status = "healthy"
			}
		}()
	}
	r.Log.Publish("failed.log", logging.Event{Type: "failed", Message: err.Error(), Data: map[string]string{"email": job.Email, "smtp": ws.Account.ID}})
	if job.Attempt+1 >= r.MaxAttempts {
		r.Stats.AddFailed()
		return
	}
	backoff := r.Backoff[min(len(r.Backoff)-1, job.Attempt)]
	jitter := time.Duration(rand.Intn(200)) * time.Millisecond
	delay := backoff + jitter
	r.wg.Add(1)
	time.AfterFunc(delay, func() {
		r.Jobs <- Job{Email: job.Email, Attempt: job.Attempt + 1}
	})
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
}

func (r *Runner) messageID(account config.SMTPAccount) string {
	domain := account.Host
	parts := strings.Split(account.MailFrom, "@")
	if len(parts) == 2 {
		domain = parts[1]
	}
	return fmt.Sprintf("%d-%d@%s", time.Now().UnixNano(), rand.Int63(), domain)
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
