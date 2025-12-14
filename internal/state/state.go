package state

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"zessen-go/internal/metrics"
)

// LeadState tracks progress for a lead.
type LeadState struct {
	Attempts int  `json:"attempts"`
	Done     bool `json:"done"`
}

// SMTPState captures circuit breaker info.
type SMTPState struct {
	ID                 string    `json:"id"`
	FailCountConsec    int       `json:"fail_count_consecutive"`
	CooldownsTriggered int       `json:"cooldowns_triggered"`
	Status             string    `json:"status"`
	CooldownUntil      time.Time `json:"cooldown_until"`
}

// Snapshot is persisted to disk.
type Snapshot struct {
	CampaignID string               `json:"campaign_id"`
	BatchID    string               `json:"batch_id"`
	Timestamp  time.Time            `json:"timestamp"`
	Leads      map[string]LeadState `json:"leads"`
	SMTPs      map[string]SMTPState `json:"smtps"`
	Stats      metrics.Snapshot     `json:"stats"`
}

// Manager handles checkpoint persistence.
type Manager struct {
	dir string
	mu  sync.Mutex
}

// NewManager creates a manager.
func NewManager(dir string) *Manager {
	_ = os.MkdirAll(dir, 0o755)
	return &Manager{dir: dir}
}

// Save writes snapshot.
func (m *Manager) Save(snapshot Snapshot) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	path := filepath.Join(m.dir, "state.json")
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("open state: %w", err)
	}
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	if err := enc.Encode(snapshot); err != nil {
		f.Close()
		return fmt.Errorf("encode state: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close state tmp: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("rename state: %w", err)
	}
	return nil
}

// Load reads snapshot.
func (m *Manager) Load() (Snapshot, error) {
	path := filepath.Join(m.dir, "state.json")
	f, err := os.Open(path)
	if err != nil {
		return Snapshot{}, err
	}
	defer f.Close()
	var snap Snapshot
	if err := json.NewDecoder(f).Decode(&snap); err != nil {
		return Snapshot{}, err
	}
	return snap, nil
}

// AutoSave periodically writes snapshots until context cancellation.
func (m *Manager) AutoSave(ctx context.Context, every time.Duration, provider func() (Snapshot, error)) {
	ticker := time.NewTicker(every)
	go func() {
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				snap, err := provider()
				if err == nil {
					_ = m.Save(snap)
				}
			case <-ctx.Done():
				return
			}
		}
	}()
}
