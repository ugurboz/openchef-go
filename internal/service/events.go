package service

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"

	"github.com/ugurboz/openchef/internal/engine"
	"github.com/ugurboz/openchef/internal/task"
)

// subscriberBuffer bounds how far a slow viewer may fall behind before it is cut off;
// the engine never waits for viewers (engine.Sink).
const subscriberBuffer = 1024

// ErrLagged is the reason a subscription ends when its viewer could not keep up. The
// viewer can re-watch from the start: the task's event log has everything.
var ErrLagged = errors.New("service: viewer fell behind; watch again to catch up")

// events persists every engine event to the task's events.jsonl (for `logs` and late
// viewers) and fans it out to live subscribers. Each task has its own lock, so one
// task's viewers never slow another task's engine.
type events struct {
	dir    string // ~/.openchef/sessions: <dir>/<task>/events.jsonl
	onFail func(error)

	mu    sync.Mutex
	tasks map[task.ID]*taskLog
}

// taskLog is one task's log file and subscribers.
type taskLog struct {
	mu    sync.Mutex
	file  *os.File
	lines int // lines in the file, counted once on open and kept up to date
	subs  map[*Subscription]struct{}
}

func newEvents(dir string, onFail func(error)) *events {
	return &events{dir: dir, onFail: onFail, tasks: map[task.ID]*taskLog{}}
}

// Subscription is one viewer of a task's events.
type Subscription struct {
	// C delivers events in order. It is closed when the subscription ends.
	C <-chan engine.Event

	c      chan engine.Event
	once   sync.Once
	mu     sync.Mutex
	reason error
}

// Err is why the subscription ended: nil if the viewer unsubscribed, ErrLagged if it
// fell behind.
func (s *Subscription) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reason
}

func (s *Subscription) end(reason error) {
	s.once.Do(func() {
		s.mu.Lock()
		s.reason = reason
		s.mu.Unlock()
		close(s.c)
	})
}

func (e *events) path(id task.ID) string {
	return filepath.Join(e.dir, string(id), "events.jsonl")
}

func (e *events) log(id task.ID) *taskLog {
	e.mu.Lock()
	defer e.mu.Unlock()
	l := e.tasks[id]
	if l == nil {
		l = &taskLog{lines: -1, subs: map[*Subscription]struct{}{}}
		e.tasks[id] = l
	}
	return l
}

// Publish implements engine.Sink: append, then fan out, under the task's lock.
func (e *events) Publish(ev engine.Event) {
	line, err := json.Marshal(ev)
	if err != nil {
		e.onFail(fmt.Errorf("service: encode event: %w", err))
		return
	}
	l := e.log(ev.TaskID)
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := e.appendLocked(ev.TaskID, l, line); err != nil {
		e.onFail(err)
	}
	for sub := range l.subs {
		select {
		case sub.c <- ev:
		default:
			delete(l.subs, sub)
			sub.end(ErrLagged)
		}
	}
}

func (e *events) appendLocked(id task.ID, l *taskLog, line []byte) error {
	if l.file == nil {
		if err := e.openLocked(id, l); err != nil {
			return err
		}
	}
	if _, err := l.file.Write(append(line, '\n')); err != nil {
		return fmt.Errorf("service: write event log: %w", err)
	}
	l.lines++
	return nil
}

func (e *events) openLocked(id task.ID, l *taskLog) error {
	p := e.path(id)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return fmt.Errorf("service: create event log directory: %w", err)
	}
	if l.lines < 0 {
		n, err := countLines(p)
		if err != nil {
			return err
		}
		l.lines = n
	}
	//nolint:gosec // G304: the path is built from a validated task id.
	f, err := os.OpenFile(p, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("service: open event log: %w", err)
	}
	if err := terminateLastLine(f, p); err != nil {
		_ = f.Close() // the write error is the one to report
		return err
	}
	l.file = f
	return nil
}

// terminateLastLine ends a line a crash cut short, so the next event starts its own
// line instead of merging into the broken one (which readLog skips but counts).
func terminateLastLine(f *os.File, path string) error {
	info, err := f.Stat()
	if err != nil {
		return fmt.Errorf("service: stat event log: %w", err)
	}
	if info.Size() == 0 {
		return nil
	}
	//nolint:gosec // G304: the path is built from a validated task id.
	r, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("service: open event log: %w", err)
	}
	defer func() { _ = r.Close() }() // read-only
	last := make([]byte, 1)
	if _, err := r.ReadAt(last, info.Size()-1); err != nil {
		return fmt.Errorf("service: read event log: %w", err)
	}
	if last[0] == '\n' {
		return nil
	}
	if _, err := f.Write([]byte{'\n'}); err != nil {
		return fmt.Errorf("service: write event log: %w", err)
	}
	return nil
}

// Subscribe returns the task's past events (if replay) and a subscription to new ones:
// no event is missed or delivered twice. The subscription is registered under the
// task's lock together with the current line count; the log is then read outside the
// lock, up to that count, so a long replay never holds up the engine.
func (e *events) Subscribe(id task.ID, replay bool) ([]engine.Event, *Subscription, error) {
	l := e.log(id)
	c := make(chan engine.Event, subscriberBuffer)
	sub := &Subscription{C: c, c: c}
	l.mu.Lock()
	if l.lines < 0 {
		n, err := countLines(e.path(id))
		if err != nil {
			l.mu.Unlock()
			return nil, nil, err
		}
		l.lines = n
	}
	upTo := l.lines
	l.subs[sub] = struct{}{}
	l.mu.Unlock()
	if !replay {
		return nil, sub, nil
	}
	past, err := readLog(e.path(id), upTo)
	if err != nil {
		e.Unsubscribe(id, sub)
		return nil, nil, err
	}
	return past, sub, nil
}

// watched reports whether anyone is watching the task live.
func (e *events) watched(id task.ID) bool {
	l := e.log(id)
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.subs) > 0
}

// Unsubscribe ends sub.
func (e *events) Unsubscribe(id task.ID, sub *Subscription) {
	l := e.log(id)
	l.mu.Lock()
	delete(l.subs, sub)
	l.mu.Unlock()
	sub.end(nil)
}

// closeTask releases a finished task's log file; a later event reopens it.
func (e *events) closeTask(id task.ID) {
	l := e.log(id)
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file != nil {
		if err := l.file.Close(); err != nil {
			e.onFail(fmt.Errorf("service: close event log: %w", err))
		}
		l.file = nil
	}
}

// Close ends every subscription and closes every log.
func (e *events) Close() error {
	e.mu.Lock()
	logs := e.tasks
	e.tasks = map[task.ID]*taskLog{}
	e.mu.Unlock()
	var errs []error
	for id, l := range logs {
		l.mu.Lock()
		if l.file != nil {
			if err := l.file.Close(); err != nil {
				errs = append(errs, fmt.Errorf("service: close event log of %s: %w", id, err))
			}
			l.file = nil
		}
		for sub := range l.subs {
			sub.end(nil)
		}
		l.subs = map[*Subscription]struct{}{}
		l.mu.Unlock()
	}
	return errors.Join(errs...)
}

func countLines(path string) (int, error) {
	//nolint:gosec // G304: the path is built from a validated task id.
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("service: open event log: %w", err)
	}
	defer func() { _ = f.Close() }() // read-only
	n := 0
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 16<<20)
	for sc.Scan() {
		n++
	}
	if err := sc.Err(); err != nil {
		return 0, fmt.Errorf("service: read event log: %w", err)
	}
	return n, nil
}

// readLog returns the events on the first upTo lines; lines that do not decode (cut
// short by a crash) are skipped but counted.
func readLog(path string, upTo int) ([]engine.Event, error) {
	//nolint:gosec // G304: the path is built from a validated task id.
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("service: open event log: %w", err)
	}
	defer func() { _ = f.Close() }() // read-only
	var out []engine.Event
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 16<<20)
	for n := 0; n < upTo && sc.Scan(); n++ {
		var ev engine.Event
		if err := json.Unmarshal(sc.Bytes(), &ev); err != nil {
			continue
		}
		out = append(out, ev)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("service: read event log: %w", err)
	}
	return out, nil
}
