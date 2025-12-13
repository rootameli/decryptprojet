package logging

import (
    "encoding/json"
    "fmt"
    "os"
    "path/filepath"
    "sync"
)

// Event represents a structured log entry.
type Event struct {
    Type    string      `json:"type"`
    Message string      `json:"message"`
    Data    interface{} `json:"data,omitempty"`
}

// Writer is a single writer consuming events asynchronously.
type Writer struct {
    dir    string
    files  map[string]*os.File
    mu     sync.Mutex
    events chan Event
    done   chan struct{}
}

// NewWriter constructs a writer for a directory.
func NewWriter(dir string) (*Writer, error) {
    if err := os.MkdirAll(dir, 0o755); err != nil {
        return nil, err
    }
    return &Writer{
        dir:    dir,
        files:  make(map[string]*os.File),
        events: make(chan Event, 1024),
        done:   make(chan struct{}),
    }, nil
}

// Start begins the background loop.
func (w *Writer) Start() {
    go func() {
        for ev := range w.events {
            w.writeEvent(ev)
        }
        close(w.done)
    }()
}

// Stop flushes and closes files.
func (w *Writer) Stop() {
    close(w.events)
    <-w.done
    w.mu.Lock()
    defer w.mu.Unlock()
    for _, f := range w.files {
        f.Sync()
        f.Close()
    }
}

// Publish enqueues an event.
func (w *Writer) Publish(filename string, ev Event) {
    if ev.Type == "secret" {
        // never log secret events
        return
    }
    w.events <- Event{Type: ev.Type, Message: ev.Message, Data: ev.Data}
    _ = filename
}

func (w *Writer) writeEvent(ev Event) {
    target := "smtp.log"
    switch ev.Type {
    case "sent":
        target = "sent.log"
    case "failed":
        target = "failed.log"
    case "smtp":
        target = "smtp.log"
    case "summary":
        target = "run_summary.json"
    }
    w.mu.Lock()
    defer w.mu.Unlock()
    if _, ok := w.files[target]; !ok {
        path := filepath.Join(w.dir, target)
        file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
        if err != nil {
            fmt.Fprintf(os.Stderr, "log open error: %v\n", err)
            return
        }
        w.files[target] = file
    }
    file := w.files[target]
    enc := json.NewEncoder(file)
    if target != "run_summary.json" {
        if err := enc.Encode(ev); err != nil {
            fmt.Fprintf(os.Stderr, "log encode error: %v\n", err)
        }
    } else {
        file.Truncate(0)
        file.Seek(0, 0)
        if err := enc.Encode(ev.Data); err != nil {
            fmt.Fprintf(os.Stderr, "summary encode error: %v\n", err)
        }
    }
}

