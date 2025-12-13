package config

import (
    "encoding/json"
    "errors"
    "fmt"
    "os"
    "path/filepath"
    "strings"
    "time"
)

const (
    ProfileA = "A"
    ProfileB = "B"
    ProfileC = "C"
)

// Config represents the application configuration.
type Config struct {
    CampaignID string `json:"campaign_id"`
    Paths      Paths  `json:"paths"`
    Concurrency Concurrency `json:"concurrency"`
    Retry      Retry   `json:"retry"`
    Timeouts   Timeouts `json:"timeouts"`
    CircuitBreaker CircuitBreaker `json:"circuit_breaker"`
    Allowlist []string `json:"allowlist"`
    Profiles   map[string]Profile `json:"profiles"`
    Batch      Batch `json:"batch"`
    DomainsAllowlist []string `json:"domains_allowlist"`
}

type Paths struct {
    LeadsFile    string `json:"leads_file"
`
    SMTPSFile    string `json:"smtps_file"`
    TemplatesDir string `json:"templates_dir"`
    LogsDir      string `json:"logs_dir"`
    StateDir     string `json:"state_dir"`
}

type Concurrency struct {
    MaxWorkers int `json:"max_workers"`
}

type Retry struct {
    MaxAttemptsPerLead int     `json:"max_attempts_per_lead"`
    BackoffSeconds     []int   `json:"backoff_seconds"`
}

type Timeouts struct {
    ConnectTimeoutSeconds int `json:"connect_timeout_seconds"`
    SendTimeoutSeconds    int `json:"send_timeout_seconds"`
    JobTimeoutSeconds     int `json:"job_timeout_seconds"`
}

type CircuitBreaker struct {
    FailThreshold          int `json:"fail_threshold"`
    CooldownSeconds        int `json:"cooldown_seconds"`
    DisableAfterCooldowns  int `json:"disable_after_cooldowns"`
}

type Batch struct {
    BatchSize int `json:"batch_size"`
}

type Profile struct {
    SubjectPool []string `json:"subject_pool"`
    FromNamePool []string `json:"fromname_pool"`
    XMailer string `json:"x_mailer"`
    TemplateFile string `json:"template_file"`
    LinkParams map[string]string `json:"link_params"`
}

// SMTPAccount represents one SMTP credential line.
type SMTPAccount struct {
    Host     string
    Port     int
    User     string
    Password string
    MailFrom string
    ID       string
}

// Load reads and validates configuration.
func Load(path string) (Config, error) {
    file, err := os.Open(path)
    if err != nil {
        return Config{}, fmt.Errorf("open config: %w", err)
    }
    defer file.Close()

    var cfg Config
    decoder := json.NewDecoder(file)
    if err := decoder.Decode(&cfg); err != nil {
        return Config{}, fmt.Errorf("decode config: %w", err)
    }
    if err := cfg.Validate(); err != nil {
        return Config{}, err
    }
    return cfg, nil
}

// Validate ensures the config is well-formed.
func (c Config) Validate() error {
    if c.CampaignID == "" {
        return errors.New("campaign_id is required")
    }
    if c.Paths.LeadsFile == "" || c.Paths.SMTPSFile == "" || c.Paths.TemplatesDir == "" || c.Paths.LogsDir == "" || c.Paths.StateDir == "" {
        return errors.New("all path fields are required")
    }
    if c.Concurrency.MaxWorkers <= 0 {
        return errors.New("concurrency.max_workers must be > 0")
    }
    if c.Retry.MaxAttemptsPerLead <= 0 {
        return errors.New("retry.max_attempts_per_lead must be > 0")
    }
    if len(c.Retry.BackoffSeconds) == 0 {
        return errors.New("retry.backoff_seconds must have at least one entry")
    }
    if c.Timeouts.ConnectTimeoutSeconds <= 0 || c.Timeouts.SendTimeoutSeconds <= 0 || c.Timeouts.JobTimeoutSeconds <= 0 {
        return errors.New("timeouts must be > 0")
    }
    if c.CircuitBreaker.FailThreshold <= 0 || c.CircuitBreaker.CooldownSeconds <= 0 || c.CircuitBreaker.DisableAfterCooldowns <= 0 {
        return errors.New("circuit_breaker settings must be > 0")
    }
    if c.Batch.BatchSize <= 0 {
        return errors.New("batch.batch_size must be > 0")
    }
    if len(c.Profiles) != 3 {
        return errors.New("profiles must contain exactly 3 entries (A,B,C)")
    }
    for _, key := range []string{ProfileA, ProfileB, ProfileC} {
        p, ok := c.Profiles[key]
        if !ok {
            return fmt.Errorf("missing profile %s", key)
        }
        if p.TemplateFile == "" || p.XMailer == "" || len(p.SubjectPool) == 0 || len(p.FromNamePool) == 0 {
            return fmt.Errorf("profile %s missing required fields", key)
        }
    }
    // ensure directories exist
    for _, dir := range []string{c.Paths.LogsDir, c.Paths.StateDir, c.Paths.TemplatesDir} {
        if err := os.MkdirAll(dir, 0o755); err != nil {
            return fmt.Errorf("create dir %s: %w", dir, err)
        }
    }
    return nil
}

// ParseSMTPFile reads smtps.txt lines into accounts.
func ParseSMTPFile(path string) ([]SMTPAccount, error) {
    raw, err := os.ReadFile(path)
    if err != nil {
        return nil, fmt.Errorf("read smtps file: %w", err)
    }
    var accounts []SMTPAccount
    lines := SplitLines(string(raw))
    for idx, line := range lines {
        if line == "" {
            continue
        }
        parts := SplitFields(line)
        if len(parts) != 5 {
            return nil, fmt.Errorf("invalid smtp line %d", idx+1)
        }
        port, err := ParseInt(parts[1])
        if err != nil {
            return nil, fmt.Errorf("invalid port on line %d: %w", idx+1, err)
        }
        accounts = append(accounts, SMTPAccount{Host: parts[0], Port: port, User: parts[2], Password: parts[3], MailFrom: parts[4], ID: fmt.Sprintf("smtp-%d", idx+1)})
    }
    return accounts, nil
}

// SplitLines splits respecting windows line endings.
func SplitLines(input string) []string {
    var lines []string
    current := ""
    for _, r := range input {
        if r == '\n' {
            lines = append(lines, Trim(current))
            current = ""
            continue
        }
        current += string(r)
    }
    lines = append(lines, Trim(current))
    return lines
}

// Trim trims spaces and carriage returns.
func Trim(s string) string {
    return strings.TrimSpace(s)
}

// SplitFields splits host|port|user|pass|from.
func SplitFields(line string) []string {
    var parts []string
    current := ""
    for _, r := range line {
        if r == '|' {
            parts = append(parts, current)
            current = ""
            continue
        }
        if r == '\r' {
            continue
        }
        current += string(r)
    }
    parts = append(parts, current)
    return parts
}

// ParseInt parses string to int.
func ParseInt(v string) (int, error) {
    var out int
    _, err := fmt.Sscanf(v, "%d", &out)
    if err != nil {
        return 0, err
    }
    return out, nil
}

// SelectProfileForBatch rotates profiles slowly based on batch index.
func SelectProfileForBatch(batchIndex int) string {
    switch batchIndex % 3 {
    case 0:
        return ProfileA
    case 1:
        return ProfileB
    default:
        return ProfileC
    }
}

// ResolveTemplatePath returns absolute template path.
func ResolveTemplatePath(baseDir, templateFile string) string {
    if filepath.IsAbs(templateFile) {
        return templateFile
    }
    return filepath.Join(baseDir, templateFile)
}

// BackoffDurations converts config backoff to durations with jitter.
func BackoffDurations(backoffs []int) []time.Duration {
    result := make([]time.Duration, len(backoffs))
    for i, v := range backoffs {
        result[i] = time.Duration(v) * time.Second
    }
    return result
}

