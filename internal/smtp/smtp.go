package smtp

import (
    "crypto/tls"
    "fmt"
    "net"
    gosmtp "net/smtp"
    "strings"
    "time"
)

// Session represents a persistent SMTP session.
type Session struct {
    account Account
    client  *gosmtp.Client
    lastUsed time.Time
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

// Dial opens a TLS connection and authenticates.
func Dial(account Account, connectTimeout time.Duration) (*Session, error) {
    addr := fmt.Sprintf("%s:%d", account.Host, account.Port)
    dialer := net.Dialer{Timeout: connectTimeout}
    conn, err := tls.DialWithDialer(&dialer, "tcp", addr, &tls.Config{ServerName: account.Host, InsecureSkipVerify: false})
    if err != nil {
        return nil, fmt.Errorf("dial: %w", err)
    }
    client, err := gosmtp.NewClient(conn, account.Host)
    if err != nil {
        return nil, fmt.Errorf("new client: %w", err)
    }
    if ok, _ := client.Extension("AUTH"); ok {
        auth := gosmtp.PlainAuth("", account.User, account.Password, account.Host)
        if err := client.Auth(auth); err != nil {
            client.Quit()
            return nil, fmt.Errorf("auth: %w", err)
        }
    }
    return &Session{account: account, client: client, lastUsed: time.Now()}, nil
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

// BuildMessage constructs headers and body in stable order.
func BuildMessage(fromName, fromEmail, toEmail, subject, htmlBody string, headers map[string]string) []byte {
    var b strings.Builder
    b.WriteString(fmt.Sprintf("From: %s <%s>\r\n", fromName, fromEmail))
    b.WriteString(fmt.Sprintf("To: %s\r\n", toEmail))
    b.WriteString(fmt.Sprintf("Subject: %s\r\n", subject))
    b.WriteString(fmt.Sprintf("Date: %s\r\n", time.Now().Format(time.RFC1123Z)))
    if msgID, ok := headers["Message-ID"]; ok {
        b.WriteString(fmt.Sprintf("Message-ID: <%s>\r\n", msgID))
        delete(headers, "Message-ID")
    }
    b.WriteString("MIME-Version: 1.0\r\n")
    b.WriteString("Content-Type: text/html; charset=\"UTF-8\"\r\n")
    // stable custom headers order
    keys := []string{"X-Campaign-ID", "X-Batch-ID", "X-SMTP-ID", "X-Mailer", "X-Trace-ID"}
    for _, k := range keys {
        if v, ok := headers[k]; ok && v != "" {
            b.WriteString(fmt.Sprintf("%s: %s\r\n", k, v))
            delete(headers, k)
        }
    }
    for k, v := range headers {
        b.WriteString(fmt.Sprintf("%s: %s\r\n", k, v))
    }
    b.WriteString("\r\n")
    b.WriteString(htmlBody)
    return []byte(b.String())
}

