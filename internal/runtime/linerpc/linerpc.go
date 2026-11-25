// Package linerpc is a JSON-RPC 2.0 peer over newline-delimited JSON, the framing used by
// `codex app-server` and `agy acp`. It matches responses to calls, delivers
// notifications in order without ever dropping one, and answers requests the agent
// sends to us (approvals, questions) through a Handler.
package linerpc

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"sync"
	"sync/atomic"

	"github.com/ugurboz/openchef/internal/runtime/rawlog"
)

// DefaultMaxLine bounds a single message (64 MiB): large enough for diffs and file
// contents, small enough to stop a runaway stream.
const DefaultMaxLine = 64 << 20

// Errors.
var (
	// ErrClosed means the peer's input ended; pending and future calls fail with it.
	ErrClosed = errors.New("linerpc: connection closed")
	// ErrLineTooLong means a message exceeded the configured limit.
	ErrLineTooLong = errors.New("linerpc: message exceeds size limit")
)

// Error is a JSON-RPC error object, returned by Call and sent by Handlers.
type Error struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *Error) Error() string { return fmt.Sprintf("json-rpc error %d: %s", e.Code, e.Message) }

// Standard error codes.
const (
	CodeMethodNotFound = -32601
	CodeInternal       = -32603
	CodeServer         = -32000
)

// Notification is a message without an id.
type Notification struct {
	Method string
	Params json.RawMessage

	fence chan struct{} // set on the marker queued by Barrier
}

// Release reports whether n is a Barrier marker rather than a notification, and if so
// releases the waiting Barrier. Consumers of a peer that uses Barrier must call it for
// every notification and skip markers; it is false for every real notification.
func (n Notification) Release() bool {
	if n.fence == nil {
		return false
	}
	close(n.fence)
	return true
}

// Handler answers a request initiated by the remote side. A returned *Error is sent as
// is; any other error becomes CodeServer. ctx is cancelled when the peer closes.
type Handler func(ctx context.Context, method string, params json.RawMessage) (any, error)

// Options configure a Peer.
type Options struct {
	Log     *rawlog.Log
	Handler Handler
	// MaxLine overrides DefaultMaxLine.
	MaxLine int
	// OnProtocolError is called for lines that are not valid JSON-RPC. The peer keeps
	// running; the adapter decides whether that is fatal.
	OnProtocolError func(line []byte, err error)
}

type message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *Error          `json:"error,omitempty"`
}

// Peer is one side of a JSON-RPC connection.
type Peer struct {
	opts Options

	wmu sync.Mutex
	w   io.Writer

	nextID  atomic.Int64
	pmu     sync.Mutex
	pending map[string]chan message

	notifyIn  chan Notification
	notifyOut chan Notification
	inMu      sync.Mutex // orders Barrier against finish closing notifyIn
	inClosed  bool

	ctx      context.Context
	cancel   context.CancelFunc
	done     chan struct{}
	pumpDone chan struct{} // closed after the last notification was delivered
	err      error         // set before done is closed
}

// New starts reading r and writes to w. The peer runs until r ends.
func New(r io.Reader, w io.Writer, opts Options) *Peer {
	if opts.MaxLine <= 0 {
		opts.MaxLine = DefaultMaxLine
	}
	ctx, cancel := context.WithCancel(context.Background())
	p := &Peer{
		opts:      opts,
		w:         w,
		pending:   map[string]chan message{},
		notifyIn:  make(chan Notification),
		notifyOut: make(chan Notification),
		ctx:       ctx,
		cancel:    cancel,
		done:      make(chan struct{}),
		pumpDone:  make(chan struct{}),
	}
	go p.pumpNotifications()
	go p.readLoop(r)
	return p
}

// Done is closed when the input has ended (EOF, read error or oversize message).
func (p *Peer) Done() <-chan struct{} { return p.done }

// Err returns why the peer stopped (nil for a clean EOF). Valid after Done.
func (p *Peer) Err() error {
	<-p.done
	return p.err
}

// Notifications delivers notifications in arrival order. It is closed after Done once
// every received notification has been delivered.
func (p *Peer) Notifications() <-chan Notification { return p.notifyOut }

// Call sends a request and decodes the result into out (which may be nil).
func (p *Peer) Call(ctx context.Context, method string, params, out any) error {
	id := strconv.FormatInt(p.nextID.Add(1), 10)
	ch := make(chan message, 1)
	p.pmu.Lock()
	select {
	case <-p.done:
		p.pmu.Unlock()
		return fmt.Errorf("%s: %w", method, ErrClosed)
	default:
	}
	p.pending[id] = ch
	p.pmu.Unlock()
	defer func() {
		p.pmu.Lock()
		delete(p.pending, id)
		p.pmu.Unlock()
	}()

	raw, err := marshalParams(params)
	if err != nil {
		return fmt.Errorf("%s: encode params: %w", method, err)
	}
	if err := p.send(message{ID: json.RawMessage(id), Method: method, Params: raw}); err != nil {
		return fmt.Errorf("%s: %w", method, err)
	}
	select {
	case m, ok := <-ch:
		if !ok {
			return fmt.Errorf("%s: %w", method, ErrClosed)
		}
		if m.Error != nil {
			return fmt.Errorf("%s: %w", method, m.Error)
		}
		if out != nil && len(m.Result) > 0 && string(m.Result) != "null" {
			if err := json.Unmarshal(m.Result, out); err != nil {
				return fmt.Errorf("%s: decode result: %w", method, err)
			}
		}
		return nil
	case <-ctx.Done():
		return fmt.Errorf("%s: %w", method, ctx.Err())
	}
}

// Notify sends a notification.
func (p *Peer) Notify(method string, params any) error {
	raw, err := marshalParams(params)
	if err != nil {
		return fmt.Errorf("%s: encode params: %w", method, err)
	}
	if err := p.send(message{Method: method, Params: raw}); err != nil {
		return fmt.Errorf("%s: %w", method, err)
	}
	return nil
}

func marshalParams(params any) (json.RawMessage, error) {
	if params == nil {
		return nil, nil
	}
	if raw, ok := params.(json.RawMessage); ok {
		return raw, nil
	}
	raw, err := json.Marshal(params)
	if err != nil {
		return nil, fmt.Errorf("marshal: %w", err)
	}
	return raw, nil
}

func (p *Peer) send(m message) error {
	m.JSONRPC = "2.0"
	raw, err := json.Marshal(m)
	if err != nil {
		return fmt.Errorf("encode: %w", err)
	}
	p.wmu.Lock()
	defer p.wmu.Unlock()
	p.opts.Log.Line(rawlog.Send, raw)
	if _, err := p.w.Write(append(raw, '\n')); err != nil {
		return fmt.Errorf("write: %w", err)
	}
	return nil
}

func (p *Peer) readLoop(r io.Reader) {
	br := bufio.NewReaderSize(r, 64<<10)
	var stopErr error
	for {
		line, err := readLine(br, p.opts.MaxLine)
		if len(line) > 0 {
			p.dispatch(line)
		}
		if err != nil {
			if !errors.Is(err, io.EOF) {
				stopErr = err
			}
			break
		}
	}
	p.finish(stopErr)
}

func readLine(br *bufio.Reader, maxLine int) ([]byte, error) {
	var buf []byte
	for {
		chunk, err := br.ReadSlice('\n')
		buf = append(buf, chunk...)
		if len(buf) > maxLine {
			return nil, ErrLineTooLong
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if len(buf) > 0 && buf[len(buf)-1] == '\n' {
			buf = buf[:len(buf)-1]
		}
		if len(buf) > 0 && buf[len(buf)-1] == '\r' {
			buf = buf[:len(buf)-1]
		}
		if err != nil {
			return buf, fmt.Errorf("read: %w", err)
		}
		return buf, nil
	}
}

func (p *Peer) dispatch(line []byte) {
	p.opts.Log.Line(rawlog.Recv, line)
	var m message
	if err := json.Unmarshal(line, &m); err != nil {
		p.protocolError(line, fmt.Errorf("invalid JSON: %w", err))
		return
	}
	switch {
	case m.Method != "" && len(m.ID) > 0:
		go p.answer(m)
	case m.Method != "":
		p.notifyIn <- Notification{Method: m.Method, Params: m.Params} // never blocks long: the pump buffers
	case len(m.ID) > 0:
		key := canonicalID(m.ID)
		p.pmu.Lock()
		ch := p.pending[key]
		p.pmu.Unlock()
		if ch == nil {
			p.protocolError(line, fmt.Errorf("response to unknown id %s", m.ID))
			return
		}
		select {
		case ch <- m:
		default: // a second response for the same id must not block the reader
			p.protocolError(line, fmt.Errorf("duplicate response for id %s", m.ID))
		}
	default:
		p.protocolError(line, errors.New("message has neither method nor id"))
	}
}

// canonicalID makes numeric ids comparable regardless of formatting ("7" vs 7.0) and
// leaves string ids as they are.
func canonicalID(raw json.RawMessage) string {
	var n json.Number
	if err := json.Unmarshal(raw, &n); err == nil {
		if i, err := n.Int64(); err == nil {
			return strconv.FormatInt(i, 10)
		}
		if f, err := n.Float64(); err == nil && f == float64(int64(f)) {
			return strconv.FormatInt(int64(f), 10)
		}
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return "s:" + s
	}
	return string(raw)
}

func (p *Peer) answer(m message) {
	if p.opts.Handler == nil {
		p.reply(m.ID, nil, &Error{Code: CodeMethodNotFound, Message: "no handler for " + m.Method})
		return
	}
	res, err := p.opts.Handler(p.ctx, m.Method, m.Params)
	if err != nil {
		var rpcErr *Error
		if !errors.As(err, &rpcErr) {
			rpcErr = &Error{Code: CodeServer, Message: err.Error()}
		}
		p.reply(m.ID, nil, rpcErr)
		return
	}
	raw, err := json.Marshal(res)
	if err != nil {
		p.reply(m.ID, nil, &Error{Code: CodeInternal, Message: "encode result: " + err.Error()})
		return
	}
	p.reply(m.ID, raw, nil)
}

func (p *Peer) reply(id, result json.RawMessage, rpcErr *Error) {
	if result == nil && rpcErr == nil {
		result = json.RawMessage("null")
	}
	if err := p.send(message{ID: id, Result: result, Error: rpcErr}); err != nil {
		p.opts.Log.Notef(rawlog.Note, "linerpc: reply to %s failed: %v", id, err)
	}
}

func (p *Peer) protocolError(line []byte, err error) {
	if p.opts.OnProtocolError != nil {
		p.opts.OnProtocolError(line, err)
	}
}

func (p *Peer) finish(err error) {
	p.err = err
	p.cancel() // in-flight handlers learn the connection is gone
	p.pmu.Lock()
	close(p.done)
	for id, ch := range p.pending {
		close(ch)
		delete(p.pending, id)
	}
	p.pmu.Unlock()
	p.inMu.Lock()
	p.inClosed = true
	close(p.notifyIn)
	p.inMu.Unlock()
}

// Barrier returns once the consumer has taken every notification that was read before
// the call, plus the marker it queues (see Notification.Release). Protocols that end a
// turn with a response rather than a notification (ACP's session/prompt) use it so no
// update that preceded the response is lost or credited to the next turn.
func (p *Peer) Barrier(ctx context.Context) error {
	fence := make(chan struct{})
	p.inMu.Lock()
	if p.inClosed {
		p.inMu.Unlock()
		return p.drained(ctx)
	}
	p.notifyIn <- Notification{fence: fence} // the pump always receives while running
	p.inMu.Unlock()
	select {
	case <-fence:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("linerpc: barrier: %w", ctx.Err())
	}
}

// drained waits until every queued notification has been delivered after the input
// ended; there is nothing left that could precede a later response.
func (p *Peer) drained(ctx context.Context) error {
	select {
	case <-p.pumpDone:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("linerpc: barrier: %w", ctx.Err())
	}
}

// pumpNotifications decouples the reader from a slow consumer with an unbounded queue,
// so a consumer that is busy (e.g. waiting on a human approval) never causes the reader
// to block or a notification to be lost.
//
// The consumer must drain Notifications until it is closed.
func (p *Peer) pumpNotifications() {
	defer close(p.pumpDone)
	defer close(p.notifyOut)
	var queue []Notification
	in := p.notifyIn
	for in != nil || len(queue) > 0 {
		var out chan Notification
		var head Notification
		if len(queue) > 0 {
			out = p.notifyOut
			head = queue[0]
		}
		select {
		case n, ok := <-in:
			if !ok {
				in = nil
				continue
			}
			queue = append(queue, n)
		case out <- head:
			queue = queue[1:]
		}
	}
}
