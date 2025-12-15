package config

import (
    "os"
    "testing"
)

func TestLoadExample(t *testing.T) {
    cfg, err := Load("../../config.example.json")
    if err != nil {
        t.Fatalf("load: %v", err)
    }
    if cfg.CampaignID == "" {
        t.Fatal("campaign id missing")
    }
    if len(cfg.Profiles) != 3 {
        t.Fatal("profiles expected")
    }
}

func TestSelectProfileForBatch(t *testing.T) {
    if SelectProfileForBatch(0) != ProfileA || SelectProfileForBatch(1) != ProfileB || SelectProfileForBatch(2) != ProfileC || SelectProfileForBatch(3) != ProfileA {
        t.Fatalf("rotation incorrect")
    }
}

func TestParseSMTP(t *testing.T) {
    content := "smtp.example.com|587|user|pass|from@example.com\n"
    path := t.TempDir() + "/smtps.txt"
    if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
        t.Fatalf("write: %v", err)
    }
    accounts, err := ParseSMTPFile(path)
    if err != nil {
        t.Fatalf("parse: %v", err)
    }
    if len(accounts) != 1 || accounts[0].Port != 587 {
        t.Fatalf("unexpected accounts: %+v", accounts)
    }
}

