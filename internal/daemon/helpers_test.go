package daemon

import (
	"context"
	"net"
	"runtime"
	"testing"

	"github.com/ugurboz/openchef/internal/runtime/linerpc"
)

func isWindows() bool { return runtime.GOOS == "windows" }

func rawPeer(t *testing.T, r *running) *linerpc.Peer {
	t.Helper()
	var d net.Dialer
	conn, err := d.DialContext(context.Background(), "unix", r.paths.Socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return linerpc.New(conn, conn, linerpc.Options{})
}
