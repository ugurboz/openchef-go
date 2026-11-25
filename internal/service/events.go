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
// viewers) and fans it out to live subscribers.
type events struct {
	dir string // ~/.openchef/sessions: <dir>/<task>/events.jsonl

	mu     sync.Mutex
	files  map[task.ID]*os.File
	subs   map[task.ID]map[*Subscription]struct{}
	onFail func(error)
}

func newEvents(dir string, onFail func(error)) *events {
	return &events{dir: dir, files: map[task.ID]*os.File{}, subs: map[task.ID]map[*Subscription]struct{}{}, onFail: onFail}
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

// Publish implements engine.Sink. Writing the log line and fanning out happen under
// one lock, so a subscriber that replays the log and then receives live events sees
// every event exactly once.
func (e *events) Publish(ev engine.Event) {
	line, err := json.Marshal(ev)
	if err != nil {
		e.onFail(fmt.Errorf("service: encode event: %w", err))
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.appendLocked(ev.TaskID, line); err != nil {
		e.onFail(err)
	}
	for sub := range e.subs[ev.TaskID] {
		select {
		case sub.c <- ev:
		default:
			delete(e.subs[ev.TaskID], sub)
			sub.end(ErrLagged)
		}
	}
}

func (e *events) appendLocked(id task.ID, line []byte) error {
	f := e.files[id]
	if f == nil {
		p := e.path(id)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			return fmt.Errorf("service: create event log directory: %w", err)
		}
		//nolint:gosec // G304: the path is built from a validated task id.
		nf, err := os.OpenFile(p, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			return fmt.Errorf("service: open event log: %w", err)
		}
		e.files[id], f = nf, nf
	}
	if _, err := f.Write(append(line, '\n')); err != nil {
		return fmt.Errorf("service: write event log: %w", err)
	}
	return nil
}

// Subscribe returns the task's past events (if replay) and a subscription to new ones,
// atomically: no event is missed or delivered twice.
func (e *events) Subscribe(id task.ID, replay bool) ([]engine.Event, *Subscription, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	var past []engine.Event
	if replay {
		var err error
		if past, err = readLog(e.path(id)); err != nil {
			return nil, nil, err
		}
	}
	c := make(chan engine.Event, subscriberBuffer)
	sub := &Subscription{C: c, c: c}
	if e.subs[id] == nil {
		e.subs[id] = map[*Subscription]struct{}{}
	}
	e.subs[id][sub] = struct{}{}
	return past, sub, nil
}

// Unsubscribe ends sub.
func (e *events) Unsubscribe(id task.ID, sub *Subscription) {
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.subs[id], sub)
	sub.end(nil)
}

// closeTask releases a finished task's log file.
func (e *events) closeTask(id task.ID) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if f := e.files[id]; f != nil {
		if err := f.Close(); err != nil {
			e.onFail(fmt.Errorf("service: close event log: %w", err))
		}
		delete(e.files, id)
	}
}

// Close ends every subscription and closes every log.
func (e *events) Close() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	var errs []error
	for id, f := range e.files {
		if err := f.Close(); err != nil {
			errs = append(errs, fmt.Errorf("service: close event log of %s: %w", id, err))
		}
	}
	e.files = map[task.ID]*os.File{}
	for _, subs := range e.subs {
		for sub := range subs {
			sub.end(nil)
		}
	}
	e.subs = map[task.ID]map[*Subscription]struct{}{}
	return errors.Join(errs...)
}

func readLog(path string) ([]engine.Event, error) {
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
	for sc.Scan() {
		var ev engine.Event
		if err := json.Unmarshal(sc.Bytes(), &ev); err != nil {
			continue // a line cut short by a crash; the rest of the log is still useful
		}
		out = append(out, ev)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("service: read event log: %w", err)
	}
	return out, nil
}
