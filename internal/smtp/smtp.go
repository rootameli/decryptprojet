package smtp

import (
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	gosmtp "net/smtp"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Session represents a persistent SMTP session.
type Session struct {
	account  Account
	client   *gosmtp.Client
	lastUsed time.Time
	tlsState *tls.ConnectionState
	tlsMode  string
}

// Account wraps SMTP credentials.
type Account struct {
	Host     string
	Port     int
	User     string
	Password string
	MailFrom string
	ID       string
}

type tlsStrategy int

const (
	tlsImplicit tlsStrategy = iota
	tlsStartTLSRequired
	tlsStartTLSIfAvailable
)

func tlsStrategyForPort(port int) tlsStrategy {
	switch port {
	case 465:
		return tlsImplicit
	case 587:
		return tlsStartTLSRequired
	default:
		return tlsStartTLSIfAvailable
	}
}

type startTLSClient interface {
	Extension(ext string) (bool, string)
	StartTLS(config *tls.Config) error
}

type tlsUpgradeError struct{ err error }

func (e tlsUpgradeError) Error() string { return e.err.Error() }
func (e tlsUpgradeError) Unwrap() error { return e.err }

var errInsecureConnection = errors.New("insecure connection: tls not negotiated")

func negotiateTLS(client startTLSClient, cfg *tls.Config, strategy tlsStrategy) (bool, error) {
	upgraded := false
	switch strategy {
	case tlsStartTLSRequired:
		if ok, _ := client.Extension("STARTTLS"); !ok {
			return false, tlsUpgradeError{fmt.Errorf("STARTTLS required on 587")}
		}
		if err := client.StartTLS(cfg); err != nil {
			return false, tlsUpgradeError{fmt.Errorf("starttls: %w", err)}
		}
		upgraded = true

	case tlsStartTLSIfAvailable:
		if ok, _ := client.Extension("STARTTLS"); ok {
			if err := client.StartTLS(cfg); err != nil {
				return false, tlsUpgradeError{fmt.Errorf("starttls: %w", err)}
			}
			upgraded = true
		}
	}
	return upgraded, nil
}

func helloName() string {
	host, _ := os.Hostname()
	host = strings.TrimSpace(host)
	if host == "" {
		host = "localhost"
	}
	return host
}

type dialOptions struct {
	allowInsecureAuth bool
	disableStartTLS   bool
}

// Dial opens a connection with the appropriate TLS/STARTTLS strategy and authenticates.
func Dial(account Account, connectTimeout time.Duration) (*Session, error) {
	primary, primaryErr := dialWithOptions(account, connectTimeout, dialOptions{})
	if primaryErr == nil {
		return primary, nil
	}

	var tlsErr tlsUpgradeError
	var fallbackErr error
	attemptFallback := errors.As(primaryErr, &tlsErr) || errors.Is(primaryErr, errInsecureConnection)

	if attemptFallback && account.Port == 587 && errors.As(primaryErr, &tlsErr) {
		fallbackAcc := account
		fallbackAcc.Port = 465
		if implicit, err := dialWithOptions(fallbackAcc, connectTimeout, dialOptions{}); err == nil {
			return implicit, nil
		} else {
			fallbackErr = err
		}
	}

	if attemptFallback {
		// Last resort: allow insecure auth without attempting STARTTLS, to preserve legacy behaviour when nothing else works.
		insecure, insecureErr := dialWithOptions(account, connectTimeout, dialOptions{allowInsecureAuth: true, disableStartTLS: true})
		if insecureErr == nil {
			return insecure, nil
		}

		if fallbackErr != nil {
			return nil, fmt.Errorf("primary failed: %v; fallback 465 failed: %v; insecure fallback failed: %v", primaryErr, fallbackErr, insecureErr)
		}
		return nil, fmt.Errorf("primary failed: %v; insecure fallback failed: %v", primaryErr, insecureErr)
	}
	return nil, primaryErr
}

func dialWithOptions(account Account, connectTimeout time.Duration, opts dialOptions) (*Session, error) {
	addr := net.JoinHostPort(account.Host, strconv.Itoa(account.Port))
	dialer := net.Dialer{Timeout: connectTimeout}
	strategy := tlsStrategyForPort(account.Port)
	if opts.disableStartTLS && strategy != tlsImplicit {
		strategy = tlsStartTLSIfAvailable
	}

	tlsCfg := &tls.Config{ServerName: account.Host, InsecureSkipVerify: false}
	sessionMode := ""
	localName := helloName()

	var (
		client *gosmtp.Client
		err    error
	)

	switch strategy {
	case tlsImplicit:
		conn, dialErr := tls.DialWithDialer(&dialer, "tcp", addr, tlsCfg)
		if dialErr != nil {
			return nil, fmt.Errorf("dial: %w", dialErr)
		}
		client, err = gosmtp.NewClient(conn, account.Host)
		if err == nil {
			sessionMode = "implicit_tls"
		}
	default:
		conn, dialErr := dialer.Dial("tcp", addr)
		if dialErr != nil {
			return nil, fmt.Errorf("dial: %w", dialErr)
		}
		client, err = gosmtp.NewClient(conn, account.Host)
	}

	if err != nil {
		return nil, fmt.Errorf("new client: %w", err)
	}

	if err := client.Hello(localName); err != nil {
		client.Quit()
		return nil, fmt.Errorf("hello: %w", err)
	}

	upgraded := false
	if strategy != tlsImplicit && !opts.disableStartTLS {
		if upgraded, err = negotiateTLS(client, tlsCfg, strategy); err != nil {
			client.Quit()
			return nil, err
		}
		if upgraded {
			sessionMode = "starttls"
		}
	}

	cs, hasTLS := client.TLSConnectionState()
	tlsActive := hasTLS && cs.HandshakeComplete
	if tlsActive {
		sessionMode = chooseSessionMode(sessionMode, strategy)
	}

	if !tlsActive && !opts.allowInsecureAuth {
		client.Quit()
		return nil, errInsecureConnection
	}

	if ok, _ := client.Extension("AUTH"); ok {
		auth := gosmtp.PlainAuth("", account.User, account.Password, account.Host)
		if err := client.Auth(auth); err != nil {
			client.Quit()
			return nil, fmt.Errorf("auth: %w", err)
		}
	}
	sess := &Session{account: account, client: client, lastUsed: time.Now(), tlsMode: sessionMode}
	if hasTLS {
		sess.tlsState = &cs
	}
	return sess, nil
}

func chooseSessionMode(current string, strategy tlsStrategy) string {
	if current != "" {
		return current
	}
	if strategy == tlsImplicit {
		return "implicit_tls"
	}
	return "starttls"
}

// Close closes the session.
func (s *Session) Close() {
	if s.client != nil {
		s.client.Quit()
	}
}

// SendEmail sends a prepared email body.
func (s *Session) SendEmail(to string, msg []byte, sendTimeout time.Duration) error {
	if s.client == nil {
		return fmt.Errorf("client nil")
	}
	deadline := time.Now().Add(sendTimeout)
	if err := s.client.Mail(s.account.MailFrom); err != nil {
		return fmt.Errorf("mail: %w", err)
	}
	if err := s.client.Rcpt(to); err != nil {
		return fmt.Errorf("rcpt: %w", err)
	}
	wc, err := s.client.Data()
	if err != nil {
		return fmt.Errorf("data: %w", err)
	}
	if c, ok := wc.(interface{ SetWriteDeadline(time.Time) error }); ok {
		_ = c.SetWriteDeadline(deadline)
	}
	if _, err := wc.Write(msg); err != nil {
		return fmt.Errorf("write: %w", err)
	}
	if err := wc.Close(); err != nil {
		return fmt.Errorf("close: %w", err)
	}
	s.lastUsed = time.Now()
	return nil
}

// TLSInfo returns the negotiated TLS mode and version string, if any.
func (s *Session) TLSInfo() (string, string) {
	version := ""
	if s.tlsState != nil {
		version = tlsVersionToString(s.tlsState.Version)
	}
	mode := s.tlsMode
	if mode == "" && s.tlsState != nil && s.tlsState.HandshakeComplete {
		mode = "starttls"
	}
	return mode, version
}

func tlsVersionToString(v uint16) string {
	switch v {
	case tls.VersionTLS13:
		return "TLS1.3"
	case tls.VersionTLS12:
		return "TLS1.2"
	case tls.VersionTLS11:
		return "TLS1.1"
	case tls.VersionTLS10:
		return "TLS1.0"
	default:
		return ""
	}
}

// BuildMessage constructs headers and body in stable order.
func BuildMessage(fromName, fromEmail, toEmail, subject, htmlBody string, headers map[string]string) []byte {
	local := make(map[string]string, len(headers))
	for k, v := range headers {
		local[k] = v
	}
	var b strings.Builder
	b.WriteString(fmt.Sprintf("From: %s <%s>\r\n", fromName, fromEmail))
	b.WriteString(fmt.Sprintf("To: %s\r\n", toEmail))
	b.WriteString(fmt.Sprintf("Subject: %s\r\n", subject))
	b.WriteString(fmt.Sprintf("Date: %s\r\n", time.Now().Format(time.RFC1123Z)))
	if msgID, ok := local["Message-ID"]; ok {
		if !strings.HasPrefix(msgID, "<") {
			msgID = fmt.Sprintf("<%s>", msgID)
		}
		b.WriteString(fmt.Sprintf("Message-ID: %s\r\n", msgID))
	}
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: text/html; charset=\"UTF-8\"\r\n")
	// stable custom headers order
	keys := []string{"X-Campaign-ID", "X-Batch-ID", "X-SMTP-ID", "X-Mailer", "X-Trace-ID"}
	consumed := make(map[string]struct{})
	for _, k := range keys {
		consumed[k] = struct{}{}
		if v, ok := local[k]; ok && v != "" {
			b.WriteString(fmt.Sprintf("%s: %s\r\n", k, v))
		}
	}
	extras := make([]string, 0, len(local))
	for k := range local {
		if k == "Message-ID" {
			continue
		}
		if _, ok := consumed[k]; ok {
			continue
		}
		extras = append(extras, k)
	}
	sort.Strings(extras)
	for _, k := range extras {
		b.WriteString(fmt.Sprintf("%s: %s\r\n", k, local[k]))
	}
	b.WriteString("\r\n")
	b.WriteString(htmlBody)
	return []byte(b.String())
}
