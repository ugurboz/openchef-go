package linerpc

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ugurboz/openchef/internal/runtime/rawlog"
)

// remote is the far side of a Peer: it reads what the peer writes and writes to it.
type remote struct {
	t     *testing.T
	in    *bufio.Scanner // peer -> remote
	out   io.WriteCloser // remote -> peer
	outMu sync.Mutex
}

func newPair(t *testing.T, opts Options) (*Peer, *remote) {
	t.Helper()
	toPeerR, toPeerW := io.Pipe()
	fromPeerR, fromPeerW := io.Pipe()
	p := New(toPeerR, fromPeerW, opts)
	sc := bufio.NewScanner(fromPeerR)
	sc.Buffer(make([]byte, 1<<20), 1<<24)
	r := &remote{t: t, in: sc, out: toPeerW}
	t.Cleanup(func() {
		_ = toPeerW.Close()
		_ = fromPeerR.Close()
	})
	return p, r
}

func (r *remote) read() message {
	r.t.Helper()
	ch := make(chan message, 1)
	go func() {
		if !r.in.Scan() {
			close(ch)
			return
		}
		var m message
		if err := json.Unmarshal(r.in.Bytes(), &m); err != nil {
			r.t.Errorf("peer wrote invalid JSON: %v", err)
		}
		ch <- m
	}()
	select {
	case m, ok := <-ch:
		if !ok {
			r.t.Fatal("peer closed its output")
		}
		return m
	case <-time.After(5 * time.Second):
		r.t.Fatal("timed out reading from peer")
		return message{}
	}
}

func (r *remote) write(s string) {
	r.t.Helper()
	r.outMu.Lock()
	defer r.outMu.Unlock()
	if _, err := io.WriteString(r.out, s+"\n"); err != nil {
		r.t.Errorf("write to peer: %v", err) // Errorf: write is also called from goroutines
	}
}

func ctx5(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func TestCallAndResult(t *testing.T) {
	t.Parallel()
	p, r := newPair(t, Options{})
	done := make(chan error, 1)
	var out struct {
		Thread string `json:"thread"`
	}
	go func() { done <- p.Call(ctx5(t), "thread/start", map[string]any{"cwd": "/x"}, &out) }()

	m := r.read()
	if m.JSONRPC != "2.0" || m.Method != "thread/start" || string(m.Params) != `{"cwd":"/x"}` {
		t.Fatalf("unexpected request %+v", m)
	}
	r.write(fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":{"thread":"t1"}}`, m.ID))
	if err := <-done; err != nil || out.Thread != "t1" {
		t.Fatalf("Call = %v, out=%+v", err, out)
	}
}

func TestCallErrorResponse(t *testing.T) {
	t.Parallel()
	p, r := newPair(t, Options{})
	done := make(chan error, 1)
	go func() { done <- p.Call(ctx5(t), "turn/start", nil, nil) }()
	m := r.read()
	if m.Params != nil {
		t.Errorf("nil params must be omitted, got %s", m.Params)
	}
	r.write(fmt.Sprintf(`{"id":%s,"error":{"code":-32602,"message":"bad params"}}`, m.ID))
	err := <-done
	var rpcErr *Error
	if !errors.As(err, &rpcErr) || rpcErr.Code != -32602 || !strings.Contains(err.Error(), "turn/start") {
		t.Fatalf("got %v", err)
	}
}

func TestIDFormatting(t *testing.T) {
	t.Parallel()
	for _, format := range []string{`%s`, `%s.0`} {
		p, r := newPair(t, Options{})
		done := make(chan error, 1)
		go func() { done <- p.Call(ctx5(t), "m", nil, nil) }()
		m := r.read()
		r.write(`{"id":` + fmt.Sprintf(format, m.ID) + `,"result":null}`)
		if err := <-done; err != nil {
			t.Fatalf("id format %q: %v", format, err)
		}
	}
}

func TestServerRequests(t *testing.T) {
	t.Parallel()
	handler := func(_ context.Context, method string, params json.RawMessage) (any, error) {
		switch method {
		case "approve":
			return map[string]string{"decision": "accept", "echo": string(params)}, nil
		case "typed":
			return nil, &Error{Code: 42, Message: "typed"}
		default:
			return nil, errors.New("plain failure")
		}
	}
	p, r := newPair(t, Options{Handler: handler})
	_ = p

	r.write(`{"jsonrpc":"2.0","id":"abc","method":"approve","params":{"cmd":"ls"}}`)
	m := r.read()
	if string(m.ID) != `"abc"` || m.Error != nil || !strings.Contains(string(m.Result), `"decision":"accept"`) {
		t.Fatalf("approve reply %+v (result %s)", m, m.Result)
	}

	r.write(`{"id":7,"method":"typed"}`)
	if m := r.read(); m.Error == nil || m.Error.Code != 42 || string(m.ID) != "7" {
		t.Fatalf("typed error reply %+v", m)
	}

	r.write(`{"id":8,"method":"other"}`)
	if m := r.read(); m.Error == nil || m.Error.Code != CodeServer || m.Error.Message != "plain failure" {
		t.Fatalf("plain error reply %+v", m)
	}
}

func TestNoHandler(t *testing.T) {
	t.Parallel()
	_, r := newPair(t, Options{})
	r.write(`{"id":1,"method":"approve"}`)
	if m := r.read(); m.Error == nil || m.Error.Code != CodeMethodNotFound {
		t.Fatalf("got %+v", m)
	}
}

func TestNotificationsAreOrderedAndNeverDropped(t *testing.T) {
	t.Parallel()
	p, r := newPair(t, Options{})
	const n = 10000
	go func() {
		var b strings.Builder
		for i := range n {
			fmt.Fprintf(&b, `{"method":"tick","params":{"i":%d}}`+"\n", i)
		}
		r.outMu.Lock()
		_, _ = io.WriteString(r.out, b.String())
		r.outMu.Unlock()
		_ = r.out.Close()
	}()
	// A slow consumer must not cause loss.
	got := 0
	for note := range p.Notifications() {
		var params struct {
			I int `json:"i"`
		}
		if err := json.Unmarshal(note.Params, &params); err != nil || note.Method != "tick" {
			t.Fatalf("bad notification %+v", note)
		}
		if params.I != got {
			t.Fatalf("out of order: got %d, want %d", params.I, got)
		}
		got++
		if got%2000 == 0 {
			time.Sleep(10 * time.Millisecond)
		}
	}
	if got != n {
		t.Fatalf("received %d notifications, want %d", got, n)
	}
	if err := p.Err(); err != nil {
		t.Fatalf("clean EOF should give nil Err, got %v", err)
	}
}

func TestCloseFailsPendingAndFutureCalls(t *testing.T) {
	t.Parallel()
	p, r := newPair(t, Options{})
	done := make(chan error, 1)
	go func() { done <- p.Call(ctx5(t), "slow", nil, nil) }()
	r.read()
	_ = r.out.Close()
	if err := <-done; !errors.Is(err, ErrClosed) {
		t.Fatalf("pending call: got %v, want ErrClosed", err)
	}
	<-p.Done()
	if err := p.Call(ctx5(t), "late", nil, nil); !errors.Is(err, ErrClosed) {
		t.Fatalf("call after close: got %v", err)
	}
	if _, ok := <-p.Notifications(); ok {
		t.Fatal("notifications channel should be closed")
	}
}

func TestCallContextCancelAndLateResponse(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var protoErrs []string
	p, r := newPair(t, Options{OnProtocolError: func(_ []byte, err error) {
		mu.Lock()
		protoErrs = append(protoErrs, err.Error())
		mu.Unlock()
	}})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- p.Call(ctx, "m", nil, nil) }()
	m := r.read()
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want context.Canceled", err)
	}
	r.write(fmt.Sprintf(`{"id":%s,"result":1}`, m.ID))
	// The peer must survive and keep working.
	done2 := make(chan error, 1)
	go func() { done2 <- p.Call(ctx5(t), "m2", nil, nil) }()
	m2 := r.read()
	r.write(fmt.Sprintf(`{"id":%s,"result":1}`, m2.ID))
	if err := <-done2; err != nil {
		t.Fatalf("follow-up call: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(protoErrs) != 1 || !strings.Contains(protoErrs[0], "unknown id") {
		t.Fatalf("protocol errors = %v", protoErrs)
	}
}

func TestMalformedLinesAndDuplicateResponses(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var reasons []string
	p, r := newPair(t, Options{OnProtocolError: func(_ []byte, err error) {
		mu.Lock()
		reasons = append(reasons, err.Error())
		mu.Unlock()
	}})
	done := make(chan error, 1)
	go func() { done <- p.Call(ctx5(t), "m", nil, nil) }()
	m := r.read()
	r.write(`not json`)
	r.write(`{"jsonrpc":"2.0"}`)
	r.write(fmt.Sprintf(`{"id":%s,"result":1}`, m.ID))
	r.write(fmt.Sprintf(`{"id":%s,"result":2}`, m.ID)) // duplicate must not deadlock
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	r.write(`{"method":"still-alive"}`)
	select {
	case note := <-p.Notifications():
		if note.Method != "still-alive" {
			t.Fatalf("got %+v", note)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("peer stopped after protocol errors")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(reasons) < 2 {
		t.Fatalf("expected protocol errors for bad lines, got %v", reasons)
	}
}

func TestOversizeMessageStopsPeer(t *testing.T) {
	t.Parallel()
	p, r := newPair(t, Options{MaxLine: 1024})
	go r.write(`{"method":"big","params":"` + strings.Repeat("x", 4096) + `"}`)
	select {
	case <-p.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("peer did not stop")
	}
	if !errors.Is(p.Err(), ErrLineTooLong) {
		t.Fatalf("Err = %v", p.Err())
	}
}

func TestConcurrentCalls(t *testing.T) {
	t.Parallel()
	p, r := newPair(t, Options{})
	const n = 100
	go func() {
		for range n {
			m := r.read()
			go r.write(fmt.Sprintf(`{"id":%s,"result":{"method":%q}}`, m.ID, m.Method))
		}
	}()
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var out struct {
				Method string `json:"method"`
			}
			method := fmt.Sprintf("m%d", i)
			if err := p.Call(ctx5(t), method, nil, &out); err != nil {
				errs <- err
				return
			}
			if out.Method != method {
				errs <- fmt.Errorf("response mixed up: %s got %s", method, out.Method)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

type nopCloser struct{ *bytes.Buffer }

func (nopCloser) Close() error { return nil }

func TestTrafficIsLogged(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	log := rawlog.New(nopCloser{&buf})
	p, r := newPair(t, Options{Log: log})
	done := make(chan error, 1)
	go func() { done <- p.Call(ctx5(t), "ping", nil, nil) }()
	m := r.read()
	r.write(fmt.Sprintf(`{"id":%s,"result":"pong"}`, m.ID))
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, `"dir":"send"`) || !strings.Contains(out, `"dir":"recv"`) || !strings.Contains(out, `"pong"`) {
		t.Fatalf("log missing traffic:\n%s", out)
	}
}
