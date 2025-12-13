package main

import (
    "flag"
    "fmt"
    "math/rand"
    "os"
    "strings"
    "time"

    "zessen-go/internal/config"
    "zessen-go/internal/logging"
    "zessen-go/internal/metrics"
    "zessen-go/internal/state"
    "zessen-go/internal/worker"
)

func main() {
    rand.Seed(time.Now().UnixNano())
    if len(os.Args) < 2 {
        fmt.Println("usage: zessen-go <run|test-smtps> --config config.json")
        os.Exit(1)
    }
    cmd := os.Args[1]
    switch cmd {
    case "run":
        run()
    case "test-smtps":
        testSMTPs()
    default:
        fmt.Println("unknown command")
        os.Exit(1)
    }
}

func run() {
    fs := flag.NewFlagSet("run", flag.ExitOnError)
    cfgPath := fs.String("config", "config.json", "config file")
    dryRun := fs.Bool("dry-run", false, "simulate sending")
    resume := fs.Bool("resume", false, "resume from checkpoint")
    maxWorkers := fs.Int("max-workers", 0, "override max workers")
    domainsAllow := fs.String("domains-allowlist", "", "comma separated allowed domains")
    limitPerSMTP := fs.Int("limit-per-smtp", 0, "limit emails per smtp")
    durationLimit := fs.Duration("duration-limit", 0, "max duration")
    fs.Parse(os.Args[2:])

    cfg, err := config.Load(*cfgPath)
    if err != nil {
        panic(err)
    }
    if *maxWorkers > 0 {
        cfg.Concurrency.MaxWorkers = *maxWorkers
    }
    if *domainsAllow != "" {
        cfg.DomainsAllowlist = splitCSV(*domainsAllow)
    }

    accounts, err := config.ParseSMTPFile(cfg.Paths.SMTPSFile)
    if err != nil {
        panic(err)
    }

    logWriter, err := logging.NewWriter(cfg.Paths.LogsDir)
    if err != nil {
        panic(err)
    }
    logWriter.Start()
    defer logWriter.Stop()

    stateMgr := state.NewManager(cfg.Paths.StateDir)
    var snap state.Snapshot
    if *resume {
        snap, _ = stateMgr.Load()
    }

    runner := worker.NewRunner(cfg, accounts, logWriter, stateMgr)
    runner.DryRun = *dryRun
    runner.LimitPerSMTP = *limitPerSMTP
    runner.DurationLimit = *durationLimit

    leads, err := readLines(cfg.Paths.LeadsFile)
    if err != nil {
        panic(err)
    }
    runner.Enqueue(leads, snap)
    renderer := metrics.Renderer{Stats: runner.Stats}
    stopRender := make(chan struct{})
    renderer.Start(2*time.Second, stopRender)
    runner.Start(cfg.Concurrency.MaxWorkers)
    runner.Wait()
    close(stopRender)

    summary := map[string]int64{"sent": runner.Stats.Snapshot().Sent, "failed": runner.Stats.Snapshot().Failed, "pending": runner.Stats.Snapshot().Pending}
    logWriter.Publish("run_summary.json", logging.Event{Type: "summary", Message: "", Data: summary})
}

func testSMTPs() {
    fs := flag.NewFlagSet("test-smtps", flag.ExitOnError)
    cfgPath := fs.String("config", "config.json", "config file")
    fs.Parse(os.Args[2:])
    cfg, err := config.Load(*cfgPath)
    if err != nil {
        panic(err)
    }
    accounts, err := config.ParseSMTPFile(cfg.Paths.SMTPSFile)
    if err != nil {
        panic(err)
    }
    for _, acc := range accounts {
        err := worker.ValidateSMTP(acc, time.Duration(cfg.Timeouts.ConnectTimeoutSeconds)*time.Second)
        if err != nil {
            fmt.Printf("%s failed: %v\n", acc.ID, err)
        } else {
            fmt.Printf("%s ok\n", acc.ID)
        }
    }
}

func readLines(path string) ([]string, error) {
    data, err := os.ReadFile(path)
    if err != nil {
        return nil, err
    }
    return config.SplitLines(string(data)), nil
}

func splitCSV(v string) []string {
    var out []string
    for _, part := range config.SplitFields(strings.ReplaceAll(v, ",", "|")) {
        if part != "" {
            out = append(out, part)
        }
    }
    return out
}

