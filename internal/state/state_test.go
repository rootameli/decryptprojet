package state

import (
    "testing"
    "time"
)

func TestSaveLoad(t *testing.T) {
    mgr := NewManager(t.TempDir())
    snap := Snapshot{
        CampaignID: "c1",
        BatchID:    "b1",
        Timestamp:  time.Now(),
        Leads:      []LeadState{{Email: "a@example.com", Attempts: 1, Done: true}},
        SMTPs:      []SMTPState{{ID: "smtp-1", Status: "healthy"}},
        Stats:      map[string]int64{"sent": 1, "failed": 0},
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

