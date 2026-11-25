// Package turnq delivers the events of one turn to its consumer without ever blocking
// the producer (decision D-002): producers append to an unbounded queue, a dedicated
// goroutine hands events to the consumer's channel and closes it after the terminal
// event. A consumer that stops reading therefore cannot wedge notification routing,
// approval handling, Interrupt or Close.
package turnq

import (
	"sync"
	"time"

	"github.com/ugurboz/openchef/internal/runtime"
)

// Buffer is the capacity of the consumer channel.
const Buffer = 256

// Queue is the event stream of one turn.
type Queue struct {
	mu    sync.Mutex
	cond  *sync.Cond
	queue []runtime.Event
	done  bool
	ch    chan runtime.Event
}

// New starts a queue and its delivery goroutine.
func New() *Queue {
	q := &Queue{ch: make(chan runtime.Event, Buffer)}
	q.cond = sync.NewCond(&q.mu)
	go q.deliver()
	return q
}

// C is the consumer channel. It carries exactly one terminal event and is then closed.
// The consumer must drain it until it is closed.
func (q *Queue) C() <-chan runtime.Event { return q.ch }

// Emit queues a non-terminal event; it is dropped after the turn finished. It never
// blocks. A zero At is set to now.
func (q *Queue) Emit(ev runtime.Event) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.done {
		return
	}
	if ev.At.IsZero() {
		ev.At = time.Now()
	}
	q.queue = append(q.queue, ev)
	q.cond.Signal()
}

// Finish queues the terminal event once and reports whether this call ended the turn.
// It never blocks.
func (q *Queue) Finish(ev runtime.Event) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.done {
		return false
	}
	q.done = true
	if ev.At.IsZero() {
		ev.At = time.Now()
	}
	q.queue = append(q.queue, ev)
	q.cond.Signal()
	return true
}

// Done reports whether the terminal event has been queued.
func (q *Queue) Done() bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.done
}

// Discard drains the channel in the background; use it when the channel will never be
// handed to a consumer.
func (q *Queue) Discard() {
	go func() {
		for range q.ch { // drain: nobody else reads this channel
		}
	}()
}

func (q *Queue) deliver() {
	for {
		q.mu.Lock()
		for len(q.queue) == 0 {
			q.cond.Wait()
		}
		ev := q.queue[0]
		q.queue = q.queue[1:]
		q.mu.Unlock()
		q.ch <- ev
		if ev.Kind.Terminal() {
			close(q.ch)
			return
		}
	}
}
