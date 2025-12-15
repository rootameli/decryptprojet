package metrics

import (
	"fmt"
	"sort"
	"sync"
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
	latencyMu    sync.Mutex
	latencies    []int64
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
	s.recordLatency(latency.Milliseconds())
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

func (s *Stats) recordLatency(ms int64) {
	s.latencyMu.Lock()
	defer s.latencyMu.Unlock()
	const maxSamples = 2048
	if len(s.latencies) >= maxSamples {
		copy(s.latencies, s.latencies[1:])
		s.latencies[maxSamples-1] = ms
		return
	}
	s.latencies = append(s.latencies, ms)
}

// Percentile returns the latency percentile in ms, or -1 if unavailable.
func (s *Stats) Percentile(p float64) int64 {
	s.latencyMu.Lock()
	defer s.latencyMu.Unlock()
	if len(s.latencies) == 0 {
		return -1
	}
	samples := make([]int64, len(s.latencies))
	copy(samples, s.latencies)
	sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
	idx := int(float64(len(samples)-1) * p)
	if idx < 0 {
		idx = 0
	}
	if idx >= len(samples) {
		idx = len(samples) - 1
	}
	return samples[idx]
}

// Renderer prints stats periodically.
type Renderer struct {
	Stats *Stats
}

// Start begins rendering every interval.
func (r *Renderer) Start(interval time.Duration, stop <-chan struct{}) {
	ticker := time.NewTicker(interval)
	start := time.Now()
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
				sentRate := float64(0)
				elapsedMinutes := time.Since(start).Minutes()
				if elapsedMinutes > 0 {
					sentRate = float64(snap.Sent) / elapsedMinutes
				}
				failRate := float64(0)
				totalDelivered := snap.Sent + snap.Failed
				if totalDelivered > 0 {
					failRate = float64(snap.Failed) / float64(totalDelivered) * 100
				}
				p95 := r.Stats.Percentile(0.95)
				p95Str := "n/a"
				if p95 >= 0 {
					p95Str = fmt.Sprintf("%dms", p95)
				}
				fmt.Printf("sent=%d failed=%d pending=%d healthy=%d cooldown=%d disabled=%d avg_latency=%s sent/min=%.2f fail_rate=%.2f%% p95=%s\n",
					snap.Sent, snap.Failed, snap.Pending, snap.Healthy, snap.Cooldown, snap.Disabled, avgLatency, sentRate, failRate, p95Str)
			case <-stop:
				return
			}
		}
	}()
}
