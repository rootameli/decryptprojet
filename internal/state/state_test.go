package state

import (
	"testing"
	"time"

	"zessen-go/internal/metrics"
)

func TestSaveLoad(t *testing.T) {
	mgr := NewManager(t.TempDir())
	snap := Snapshot{
		CampaignID: "c1",
		BatchID:    "b1",
		Timestamp:  time.Now(),
		Leads:      map[string]LeadState{"a@example.com": {Attempts: 1, Done: true}},
		SMTPs:      map[string]SMTPState{"smtp-1": {ID: "smtp-1", Status: "healthy"}},
		Stats:      metrics.Snapshot{Sent: 1, Failed: 0},
	}
	if err := mgr.Save(snap); err != nil {
		t.Fatalf("save: %v", err)
	}
	loaded, err := mgr.Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if loaded.CampaignID != snap.CampaignID || len(loaded.Leads) != 1 {
		t.Fatalf("unexpected loaded: %+v", loaded)
	}
}
