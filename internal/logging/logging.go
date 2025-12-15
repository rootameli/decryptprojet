package logging

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Event represents a structured log entry.
type Event struct {
	Type    string      `json:"type"`
	Message string      `json:"message"`
	Data    interface{} `json:"data,omitempty"`
	Target  string      `json:"-"`
}

// Writer is a single writer consuming events asynchronously.
type Writer struct {
	dir       string
	files     map[string]*os.File
	mu        sync.Mutex
	events    chan Event
	done      chan struct{}
	flush     time.Duration
	batch     int
	closeOnce sync.Once
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
		flush:  200 * time.Millisecond,
		batch:  64,
	}, nil
}

// Start begins the background loop.
func (w *Writer) Start() {
	go func() {
		ticker := time.NewTicker(w.flush)
		defer ticker.Stop()
		buf := make([]Event, 0, w.batch)
		flush := func() {
			if len(buf) == 0 {
				return
			}
			w.writeBatch(buf)
			buf = buf[:0]
		}
		for {
			select {
			case ev, ok := <-w.events:
				if !ok {
					flush()
					close(w.done)
					return
				}
				buf = append(buf, ev)
				if len(buf) >= w.batch {
					flush()
				}
			case <-ticker.C:
				flush()
			}
		}
	}()
}

// Stop flushes and closes files.
func (w *Writer) Stop() {
	w.closeOnce.Do(func() {
		close(w.events)
	})
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
	ev.Target = filename
	w.events <- ev
}

func (w *Writer) writeBatch(events []Event) {
	byTarget := make(map[string][]Event)
	for _, ev := range events {
		target := ev.Target
		if target == "" {
			target = w.defaultTarget(ev.Type)
		}
		byTarget[target] = append(byTarget[target], ev)
	}
	for target, evs := range byTarget {
		if target == "run_summary.json" {
			w.writeSummary(evs[len(evs)-1])
			continue
		}
		w.mu.Lock()
		if _, ok := w.files[target]; !ok {
			path := filepath.Join(w.dir, target)
			file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
			if err != nil {
				w.mu.Unlock()
				fmt.Fprintf(os.Stderr, "log open error: %v\n", err)
				continue
			}
			w.files[target] = file
		}
		file := w.files[target]
		enc := json.NewEncoder(file)
		for _, ev := range evs {
			if err := enc.Encode(ev); err != nil {
				fmt.Fprintf(os.Stderr, "log encode error: %v\n", err)
			}
		}
		w.mu.Unlock()
	}
}

func (w *Writer) writeSummary(ev Event) {
	path := filepath.Join(w.dir, "run_summary.json")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		fmt.Fprintf(os.Stderr, "summary open error: %v\n", err)
		return
	}
	enc := json.NewEncoder(file)
	if err := enc.Encode(ev.Data); err != nil {
		fmt.Fprintf(os.Stderr, "summary encode error: %v\n", err)
	}
	file.Sync()
	file.Close()
}

func (w *Writer) defaultTarget(evType string) string {
	switch evType {
	case "sent":
		return "sent.log"
	case "failed":
		return "failed.log"
	case "smtp", "test":
		return "smtp.log"
	case "summary":
		return "run_summary.json"
	default:
		return "smtp.log"
	}
}
