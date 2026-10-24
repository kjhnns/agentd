// Package runlog is a durable, append-only JSONL log. It follows the durability
// model from clawd's workflows/scripts/runner.py: append + fsync per record, and
// resume-by-replay. Every session event, every inbound/outbound message, and
// every confirm decision is recorded here so a restart can replay cleanly.
package runlog

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
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
// A torn final line (a crash in the middle of a write) is repaired first: its
// bytes are kept in <name>.torn-<ts> next to the log and the file is cut back
// to the last complete line, so the next record does not glue onto it.
func Open(path string) (*Log, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("runlog mkdir: %w", err)
	}
	if err := repairTail(path); err != nil {
		return nil, fmt.Errorf("runlog repair: %w", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("runlog open: %w", err)
	}
	return &Log{f: f, path: path}, nil
}

// tornName is where the bytes of a torn tail are kept. The base name drops a
// .jsonl suffix so the default log gives runlog.torn-<ts>.
func tornName(path string, now time.Time) string {
	base := filepath.Base(path)
	if ext := filepath.Ext(base); ext == ".jsonl" {
		base = base[:len(base)-len(ext)]
	}
	return filepath.Join(filepath.Dir(path), base+".torn-"+now.UTC().Format("20060102T150405.000000000Z"))
}

// repairTail cuts an incomplete final line off the log. A missing or empty
// file needs nothing.
func repairTail(path string) error {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || st.Size() == 0 {
		return err
	}
	size := st.Size()
	last := make([]byte, 1)
	if _, err := f.ReadAt(last, size-1); err != nil {
		return err
	}
	if last[0] == '\n' {
		return nil
	}
	// Walk back to the last newline in chunks.
	const chunk = 64 << 10
	cut := int64(0)
	for end := size; end > 0; {
		start := end - chunk
		if start < 0 {
			start = 0
		}
		buf := make([]byte, end-start)
		if _, err := f.ReadAt(buf, start); err != nil && err != io.EOF {
			return err
		}
		if i := bytes.LastIndexByte(buf, '\n'); i >= 0 {
			cut = start + int64(i) + 1
			break
		}
		end = start
	}
	torn := make([]byte, size-cut)
	if _, err := f.ReadAt(torn, cut); err != nil && err != io.EOF {
		return err
	}
	name := tornName(path, time.Now())
	if err := os.WriteFile(name, torn, 0o600); err != nil {
		return err
	}
	if err := f.Truncate(cut); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	log.Printf("runlog: %s ended in a torn record (%d bytes); kept them in %s and cut the tail", path, len(torn), name)
	return nil
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

// maxLineBytes bounds one record on replay. A longer line is skipped, never
// fatal.
const maxLineBytes = 16 << 20

// Replay reads all records from a JSONL file in order. This is the resume path:
// a restart replays the log to rebuild state. A missing file yields no records.
// A corrupt or oversized line in the middle is reported and skipped; it does
// not hide the records after it. A final line without a newline is a record
// still being written (or a torn tail Open has not repaired yet) and is
// skipped quietly.
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
	r := bufio.NewReaderSize(f, 64*1024)
	lineNo := 0
	for {
		line, complete, over, rerr := readLine(r)
		if len(line) > 0 || over {
			lineNo++
			switch {
			case !complete:
				// Partial final line: not a record yet.
			case over:
				log.Printf("runlog: %s line %d is over %d bytes; skipped", path, lineNo, maxLineBytes)
			default:
				var rec Record
				if err := json.Unmarshal(line, &rec); err != nil {
					log.Printf("runlog: %s line %d is corrupt (%v); skipped", path, lineNo, err)
				} else {
					out = append(out, rec)
				}
			}
		}
		if rerr == io.EOF {
			return out, nil
		}
		if rerr != nil {
			return out, rerr
		}
	}
}

// readLine reads one line of any length. complete is false for a final line
// with no newline; over is true when the line exceeded maxLineBytes (its
// bytes are dropped but the reader keeps its place).
func readLine(r *bufio.Reader) (line []byte, complete, over bool, err error) {
	for {
		chunk, rerr := r.ReadSlice('\n')
		if !over {
			line = append(line, chunk...)
			if len(line) > maxLineBytes {
				over = true
				line = nil
			}
		}
		if rerr == bufio.ErrBufferFull {
			continue
		}
		if rerr == nil {
			if !over {
				line = line[:len(line)-1]
			}
			return line, true, over, nil
		}
		return line, false, over, rerr
	}
}
