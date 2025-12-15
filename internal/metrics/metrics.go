package metrics

import (
	"fmt"
	"sync/atomic"
	"time"
)

// Stats captures counters for a run.
type Stats struct {
	Sent         int64
	Failed       int64
	Pending      int64
	Healthy      int64
	Cooldown     int64
	Disabled     int64
	Total        int64
	LatencySum   int64
	LatencyCount int64
}

// Snapshot is an immutable view of Stats for reporting or persistence.
type Snapshot struct {
	Sent         int64 `json:"sent"`
	Failed       int64 `json:"failed"`
	Pending      int64 `json:"pending"`
	Healthy      int64 `json:"healthy"`
	Cooldown     int64 `json:"cooldown"`
	Disabled     int64 `json:"disabled"`
	Total        int64 `json:"total"`
	LatencySum   int64 `json:"latency_sum"`
	LatencyCount int64 `json:"latency_count"`
}

// AddSent increments sent and latency.
func (s *Stats) AddSent(latency time.Duration) {
	atomic.AddInt64(&s.Sent, 1)
	atomic.AddInt64(&s.Pending, -1)
	atomic.AddInt64(&s.LatencySum, latency.Milliseconds())
	atomic.AddInt64(&s.LatencyCount, 1)
}

// AddFailed increments failures.
func (s *Stats) AddFailed() {
	atomic.AddInt64(&s.Failed, 1)
	atomic.AddInt64(&s.Pending, -1)
}

// SetPending sets pending count.
func (s *Stats) SetPending(pending, total int64) {
	atomic.StoreInt64(&s.Pending, pending)
	atomic.StoreInt64(&s.Total, total)
}

// SetSMTPState snapshots smtp counters.
func (s *Stats) SetSMTPState(healthy, cooldown, disabled int64) {
	atomic.StoreInt64(&s.Healthy, healthy)
	atomic.StoreInt64(&s.Cooldown, cooldown)
	atomic.StoreInt64(&s.Disabled, disabled)
}

// AddCooldown increments cooldown count.
func (s *Stats) AddCooldown() {
	atomic.AddInt64(&s.Cooldown, 1)
}

// AddDisabled increments disabled count.
func (s *Stats) AddDisabled() {
	atomic.AddInt64(&s.Disabled, 1)
}

// Snapshot returns immutable copy.
func (s *Stats) Snapshot() Stats {
	return Stats{
		Sent:         atomic.LoadInt64(&s.Sent),
		Failed:       atomic.LoadInt64(&s.Failed),
		Pending:      atomic.LoadInt64(&s.Pending),
		Healthy:      atomic.LoadInt64(&s.Healthy),
		Cooldown:     atomic.LoadInt64(&s.Cooldown),
		Disabled:     atomic.LoadInt64(&s.Disabled),
		Total:        atomic.LoadInt64(&s.Total),
		LatencySum:   atomic.LoadInt64(&s.LatencySum),
		LatencyCount: atomic.LoadInt64(&s.LatencyCount),
	}
}

// SnapshotData converts to a JSON-ready snapshot.
func (s *Stats) SnapshotData() Snapshot {
	snap := s.Snapshot()
	return Snapshot{
		Sent:         snap.Sent,
		Failed:       snap.Failed,
		Pending:      snap.Pending,
		Healthy:      snap.Healthy,
		Cooldown:     snap.Cooldown,
		Disabled:     snap.Disabled,
		Total:        snap.Total,
		LatencySum:   snap.LatencySum,
		LatencyCount: snap.LatencyCount,
	}
}

// Renderer prints stats periodically.
type Renderer struct {
	Stats *Stats
}

// Start begins rendering every interval.
func (r *Renderer) Start(interval time.Duration, stop <-chan struct{}) {
	ticker := time.NewTicker(interval)
	go func() {
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				snap := r.Stats.Snapshot()
				avgLatency := "n/a"
				if snap.LatencyCount > 0 {
					avgLatency = fmt.Sprintf("%dms", snap.LatencySum/snap.LatencyCount)
				}
				fmt.Printf("sent=%d failed=%d pending=%d healthy=%d cooldown=%d disabled=%d avg_latency=%s\n",
					snap.Sent, snap.Failed, snap.Pending, snap.Healthy, snap.Cooldown, snap.Disabled, avgLatency)
			case <-stop:
				return
			}
		}
	}()
}
