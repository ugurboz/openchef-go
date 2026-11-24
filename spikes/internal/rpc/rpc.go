// Package rpc is a small newline-delimited JSON-RPC 2.0 client for spikes. It talks to a
// child process over stdio, answers server-initiated requests through a handler and
// records every line to a scrubbed transcript.
//
// Spike code: correctness over polish, but no silent failures.
package rpc

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"time"
)

// Message is one JSON-RPC message.
type Message struct {
	JSONRPC string          `json:"jsonrpc,omitempty"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
}

// RPCError is a JSON-RPC error object.
type RPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *RPCError) Error() string { return fmt.Sprintf("rpc error %d: %s", e.Code, e.Message) }

// Handler answers a server-initiated request. Returning an error sends a JSON-RPC error.
type Handler func(method string, params json.RawMessage) (any, error)

// Client is a JSON-RPC peer running as a child process.
type Client struct {
	Cmd *exec.Cmd

	stdin  io.WriteCloser
	tr     *Transcript
	next   atomic.Int64
	mu     sync.Mutex
	wait   map[string]chan Message
	notify chan Message
	handle Handler
	done   chan struct{}
	exited chan struct{}
	start  time.Time
}

// Start launches path with args. Notifications are delivered on Notifications().
func Start(path string, args []string, dir string, tr *Transcript, handle Handler) (*Client, error) {
	cmd := exec.Command(path, args...)
	cmd.Dir = dir
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("stderr pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start %s: %w", path, err)
	}
	c := &Client{
		Cmd: cmd, stdin: stdin, tr: tr,
		wait:   map[string]chan Message{},
		notify: make(chan Message, 4096),
		handle: handle,
		done:   make(chan struct{}),
		exited: make(chan struct{}),
		start:  time.Now(),
	}
	go c.readLoop(stdout)
	go func() {
		sc := bufio.NewScanner(stderr)
		sc.Buffer(make([]byte, 1<<20), 1<<24)
		for sc.Scan() {
			tr.Note("stderr", sc.Text())
		}
	}()
	go func() {
		_ = cmd.Wait()
		close(c.exited)
	}()
	return c, nil
}

// Notifications returns the channel of server notifications.
func (c *Client) Notifications() <-chan Message { return c.notify }

// Exited is closed when the child process has exited.
func (c *Client) Exited() <-chan struct{} { return c.exited }

// Call sends a request and waits for its response.
func (c *Client) Call(ctx context.Context, method string, params any, out any) error {
	id := c.next.Add(1)
	idRaw := json.RawMessage(fmt.Sprintf("%d", id))
	ch := make(chan Message, 1)
	c.mu.Lock()
	c.wait[string(idRaw)] = ch
	c.mu.Unlock()
	if err := c.send(Message{ID: idRaw, Method: method, Params: mustJSON(params)}); err != nil {
		return err
	}
	select {
	case m := <-ch:
		if m.Error != nil {
			return m.Error
		}
		if out != nil && len(m.Result) > 0 {
			if err := json.Unmarshal(m.Result, out); err != nil {
				return fmt.Errorf("decode %s result: %w", method, err)
			}
		}
		return nil
	case <-c.done:
		return fmt.Errorf("%s: connection closed", method)
	case <-ctx.Done():
		return fmt.Errorf("%s: %w", method, ctx.Err())
	}
}

// Notify sends a notification.
func (c *Client) Notify(method string, params any) error {
	return c.send(Message{Method: method, Params: mustJSON(params)})
}

// CloseStdin closes the child's stdin (EOF).
func (c *Client) CloseStdin() error { return c.stdin.Close() }

// Kill sends SIGKILL.
func (c *Client) Kill() error { return c.Cmd.Process.Kill() }

func (c *Client) send(m Message) error {
	m.JSONRPC = "2.0"
	raw, err := json.Marshal(m)
	if err != nil {
		return fmt.Errorf("encode: %w", err)
	}
	c.tr.Line("send", raw)
	if _, err := c.stdin.Write(append(raw, '\n')); err != nil {
		return fmt.Errorf("write: %w", err)
	}
	return nil
}

func (c *Client) readLoop(r io.Reader) {
	defer close(c.done)
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<20), 1<<26)
	for sc.Scan() {
		line := append([]byte(nil), sc.Bytes()...)
		c.tr.Line("recv", line)
		var m Message
		if err := json.Unmarshal(line, &m); err != nil {
			c.tr.Note("parse-error", err.Error())
			continue
		}
		switch {
		case m.Method != "" && len(m.ID) > 0: // server request
			go c.answer(m)
		case m.Method != "": // notification
			select {
			case c.notify <- m:
			default:
				c.tr.Note("dropped-notification", m.Method)
			}
		default: // response
			c.mu.Lock()
			ch := c.wait[string(m.ID)]
			delete(c.wait, string(m.ID))
			c.mu.Unlock()
			if ch != nil {
				ch <- m
			}
		}
	}
	if err := sc.Err(); err != nil {
		c.tr.Note("read-error", err.Error())
	}
}

func (c *Client) answer(m Message) {
	if c.handle == nil {
		_ = c.send(Message{ID: m.ID, Error: &RPCError{Code: -32601, Message: "no handler"}})
		return
	}
	res, err := c.handle(m.Method, m.Params)
	if err != nil {
		_ = c.send(Message{ID: m.ID, Error: &RPCError{Code: -32000, Message: err.Error()}})
		return
	}
	_ = c.send(Message{ID: m.ID, Result: mustJSON(res)})
}

func mustJSON(v any) json.RawMessage {
	if v == nil {
		return nil
	}
	raw, err := json.Marshal(v)
	if err != nil {
		panic(fmt.Sprintf("marshal: %v", err))
	}
	return raw
}

// WaitExit waits up to d for the process to exit and reports how long it took.
func (c *Client) WaitExit(d time.Duration) (time.Duration, bool) {
	t0 := time.Now()
	select {
	case <-c.exited:
		return time.Since(t0), true
	case <-time.After(d):
		return d, false
	}
}

// ErrTimeout is returned by Await when no matching notification arrives in time.
var ErrTimeout = errors.New("timed out waiting for notification")

// Await consumes notifications until match returns true, the process exits or d passes.
func (c *Client) Await(d time.Duration, match func(Message) bool) (Message, error) {
	timer := time.NewTimer(d)
	defer timer.Stop()
	for {
		select {
		case m := <-c.notify:
			if match(m) {
				return m, nil
			}
		case <-c.exited:
			return Message{}, errors.New("process exited")
		case <-timer.C:
			return Message{}, ErrTimeout
		}
	}
}

// Pid returns the child PID.
func (c *Client) Pid() int { return c.Cmd.Process.Pid }

// Stderr is a helper for printing progress.
func Logf(format string, args ...any) { fmt.Fprintf(os.Stderr, format+"\n", args...) }
