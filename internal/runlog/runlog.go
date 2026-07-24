// Package runlog is a durable, append-only JSONL log. It follows the durability
// model from clawd's workflows/scripts/runner.py: append + fsync per record, and
// resume-by-replay. Every session event, every inbound/outbound message, and
// every confirm decision is recorded here so a restart can replay cleanly.
package runlog

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Record is one line in the log. Type labels the record; Data is the payload.
type Record struct {
	TS   time.Time       `json:"ts"`
	Type string          `json:"type"`
	Data json.RawMessage `json:"data"`
}

// Log is a thread-safe append-only JSONL writer with fsync-per-record.
type Log struct {
	mu   sync.Mutex
	f    *os.File
	path string
}

// Open opens (creating parent dirs as needed) the JSONL file for appending.
func Open(path string) (*Log, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("runlog mkdir: %w", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("runlog open: %w", err)
	}
	return &Log{f: f, path: path}, nil
}

// Path returns the file the log appends to (so readers can Replay it).
func (l *Log) Path() string { return l.path }

// Append marshals payload, writes one JSON line, and fsyncs before returning.
// The fsync makes the record durable against a crash the instant it returns.
func (l *Log) Append(recordType string, payload any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("runlog marshal payload: %w", err)
	}
	rec := Record{TS: time.Now().UTC(), Type: recordType, Data: data}
	line, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("runlog marshal record: %w", err)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, err := l.f.Write(append(line, '\n')); err != nil {
		return fmt.Errorf("runlog write: %w", err)
	}
	if err := l.f.Sync(); err != nil {
		return fmt.Errorf("runlog fsync: %w", err)
	}
	return nil
}

// Close closes the underlying file.
func (l *Log) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.f.Close()
}

// Replay reads all records from a JSONL file in order. This is the resume path:
// a restart replays the log to rebuild state. A missing file yields no records.
func Replay(path string) ([]Record, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()

	var out []Record
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var rec Record
		if err := json.Unmarshal(line, &rec); err != nil {
			return out, fmt.Errorf("runlog replay parse: %w", err)
		}
		out = append(out, rec)
	}
	return out, sc.Err()
}
