package api

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"syscall"

	"github.com/ugurboz/openchef/internal/runtime/linerpc"
)

// ErrNotRunning means no daemon is listening on the socket.
var ErrNotRunning = errors.New("api: the OpenChef daemon is not running (start it with: openchef daemon start)")

// Client is one connection to the daemon.
type Client struct {
	conn net.Conn
	peer *linerpc.Peer
	// Daemon describes the daemon, from the handshake.
	Daemon InitializeResult
}

// Dial connects to the daemon's socket and performs the version handshake.
func Dial(ctx context.Context, socket, client string) (*Client, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", socket)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ECONNREFUSED) || isRefused(err) {
			return nil, ErrNotRunning
		}
		return nil, fmt.Errorf("api: connect %s: %w", socket, err)
	}
	c := &Client{conn: conn, peer: linerpc.New(conn, conn, linerpc.Options{})}
	err = c.peer.Call(ctx, MethodInitialize, InitializeParams{Version: Version, Client: client}, &c.Daemon)
	if err != nil {
		var rpcErr *linerpc.Error
		if errors.As(err, &rpcErr) && rpcErr.Code == CodeVersionMismatch {
			err = fmt.Errorf("%w: %s", ErrVersionMismatch, rpcErr.Message)
		}
		return nil, errors.Join(fmt.Errorf("api: initialize: %w", err), c.Close())
	}
	return c, nil
}

// Call invokes method and decodes the result into out (which may be nil).
func (c *Client) Call(ctx context.Context, method string, params, out any) error {
	if err := c.peer.Call(ctx, method, params, out); err != nil {
		return fmt.Errorf("api: %s: %w", method, err)
	}
	return nil
}

// Status returns the daemon's status.
func (c *Client) Status(ctx context.Context) (Status, error) {
	var s Status
	err := c.Call(ctx, MethodStatus, nil, &s)
	return s, err
}

// Stop asks the daemon to shut down (§5.4). It returns once the daemon accepted the
// request; the daemon then stops its sessions and exits.
func (c *Client) Stop(ctx context.Context) error {
	err := c.Call(ctx, MethodStop, nil, nil)
	if errors.Is(err, linerpc.ErrClosed) {
		return nil // the daemon closed the connection while stopping
	}
	return err
}

// Notifications delivers server notifications in order; it closes with the connection.
func (c *Client) Notifications() <-chan linerpc.Notification { return c.peer.Notifications() }

// Done is closed when the daemon closes the connection.
func (c *Client) Done() <-chan struct{} { return c.peer.Done() }

// Close ends the connection.
func (c *Client) Close() error {
	if err := c.conn.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
		return fmt.Errorf("api: close: %w", err)
	}
	return nil
}
