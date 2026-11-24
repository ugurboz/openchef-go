// Package rawlog records every protocol line exchanged with an agent CLI as JSONL, in the
// same format as the fixtures in testdata/, so a live session can be diagnosed and turned
// into a replay fixture.
//
// Record format: {"ms": <since start>, "dir": "send"|"recv"|"stderr"|"note", "msg": <JSON>}
// or, for lines that are not JSON, {"ms": ..., "dir": ..., "text": "..."}.
package rawlog

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Directions.
const (
	Send   = "send"
	Recv   = "recv"
	Stderr = "stderr"
	Note   = "note"
)

// Log is an append-only protocol log. A nil *Log discards everything, so adapters can
// log unconditionally.
type Log struct {
	mu    sync.Mutex
	w     io.WriteCloser
	start time.Time
	err   error
}

// Open creates (or appends to) path with owner-only permissions.
func Open(path string) (*Log, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("rawlog: create directory: %w", err)
	}
	//nolint:gosec // G304: path is OpenChef's own session directory.
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("rawlog: open %s: %w", path, err)
	}
	return &Log{w: f, start: time.Now()}, nil
}

// New writes to w (tests).
func New(w io.WriteCloser) *Log { return &Log{w: w, start: time.Now()} }

// Line records one protocol line. Valid JSON is embedded as-is; anything else is
// recorded as text.
func (l *Log) Line(dir string, line []byte) {
	if l == nil {
		return
	}
	rec := struct {
		Ms   int64           `json:"ms"`
		Dir  string          `json:"dir"`
		Msg  json.RawMessage `json:"msg,omitempty"`
		Text string          `json:"text,omitempty"`
	}{Ms: time.Since(l.start).Milliseconds(), Dir: dir}
	if json.Valid(line) {
		rec.Msg = line
	} else {
		rec.Text = string(line)
	}
	l.write(rec)
}

// Notef records a free-text note (stderr line, observation).
func (l *Log) Notef(dir, format string, args ...any) {
	if l == nil {
		return
	}
	l.write(struct {
		Ms   int64  `json:"ms"`
		Dir  string `json:"dir"`
		Text string `json:"text"`
	}{time.Since(l.start).Milliseconds(), dir, fmt.Sprintf(format, args...)})
}

func (l *Log) write(rec any) {
	raw, err := json.Marshal(rec)
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.err != nil {
		return
	}
	if err != nil {
		l.err = fmt.Errorf("rawlog: encode: %w", err)
		return
	}
	if _, err := l.w.Write(append(raw, '\n')); err != nil {
		l.err = fmt.Errorf("rawlog: write: %w", err)
	}
}

// Err returns the first write error, if any. Logging never interrupts a session; the
// adapter reports this when the session closes.
func (l *Log) Err() error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.err
}

// Close closes the underlying file.
func (l *Log) Close() error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.w.Close(); err != nil {
		return fmt.Errorf("rawlog: close: %w", err)
	}
	return l.err
}

// Record is one decoded log entry.
type Record struct {
	Ms   int64           `json:"ms"`
	Dir  string          `json:"dir"`
	Msg  json.RawMessage `json:"msg,omitempty"`
	Text string          `json:"text,omitempty"`
}

// ReadFile decodes a log (or a testdata fixture).
func ReadFile(path string) ([]Record, error) {
	//nolint:gosec // G304: reading a log or fixture chosen by the caller.
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("rawlog: open %s: %w", path, err)
	}
	defer func() { _ = f.Close() }() // read-only
	var out []Record
	r := bufio.NewReader(f)
	for n := 1; ; n++ {
		line, err := r.ReadBytes('\n')
		if len(line) > 0 {
			var rec Record
			if jerr := json.Unmarshal(line, &rec); jerr != nil {
				return nil, fmt.Errorf("rawlog: %s line %d: %w", path, n, jerr)
			}
			out = append(out, rec)
		}
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return nil, fmt.Errorf("rawlog: read %s: %w", path, err)
		}
	}
}
