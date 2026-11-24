package rawlog

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestRoundTrip(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "s", "raw.jsonl")
	log, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	log.Line(Send, []byte(`{"id":1,"method":"initialize"}`))
	log.Line(Recv, []byte(`not json at all`))
	log.Notef(Stderr, "warning %d", 3)
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}

	recs, err := ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 3 {
		t.Fatalf("got %d records", len(recs))
	}
	if recs[0].Dir != Send || string(recs[0].Msg) != `{"id":1,"method":"initialize"}` {
		t.Errorf("JSON line must be embedded verbatim: %+v", recs[0])
	}
	if recs[1].Msg != nil || recs[1].Text != "not json at all" {
		t.Errorf("non-JSON line must become text: %+v", recs[1])
	}
	if recs[2].Dir != Stderr || recs[2].Text != "warning 3" {
		t.Errorf("note: %+v", recs[2])
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Errorf("mode = %v, %v; want 0600", info.Mode().Perm(), err)
		}
	}
}

func TestNilLogIsSafe(t *testing.T) {
	t.Parallel()
	var log *Log
	log.Line(Send, []byte("{}"))
	log.Notef(Note, "x")
	if err := log.Err(); err != nil {
		t.Fatal(err)
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("disk full") }
func (failingWriter) Close() error              { return nil }

func TestWriteErrorIsRememberedNotFatal(t *testing.T) {
	t.Parallel()
	log := New(failingWriter{})
	log.Line(Send, []byte("{}"))
	log.Line(Send, []byte("{}"))
	if err := log.Err(); err == nil {
		t.Fatal("expected the write error to be reported")
	}
	if err := log.Close(); err == nil {
		t.Fatal("Close should return the first write error")
	}
}

func TestReadFileErrors(t *testing.T) {
	t.Parallel()
	if _, err := ReadFile(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Error("missing file: expected error")
	}
	bad := filepath.Join(t.TempDir(), "bad.jsonl")
	if err := os.WriteFile(bad, []byte("{\"ms\":1}\nnope\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadFile(bad); err == nil {
		t.Error("invalid record: expected error")
	}
}

// Every committed fixture must stay readable: adapters' replay tests depend on it.
func TestFixturesAreReadable(t *testing.T) {
	t.Parallel()
	files, err := filepath.Glob(filepath.Join("..", "..", "..", "testdata", "*", "*.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no fixtures found under testdata/")
	}
	for _, f := range files {
		recs, err := ReadFile(f)
		if err != nil {
			t.Errorf("%s: %v", f, err)
			continue
		}
		recv := 0
		for _, r := range recs {
			if r.Dir == Recv && r.Msg != nil {
				recv++
			}
		}
		if recv == 0 {
			t.Errorf("%s: no received protocol messages", f)
		}
	}
}

type bufCloser struct{ bytes.Buffer }

func (*bufCloser) Close() error { return nil }

func TestLineWriter(t *testing.T) {
	t.Parallel()
	var buf bufCloser
	log := New(&buf)
	w := log.LineWriter(Stderr)
	for _, chunk := range []string{"first li", "ne\nsecond\r\nthird"} {
		if _, err := w.Write([]byte(chunk)); err != nil {
			t.Fatal(err)
		}
	}
	out := buf.String()
	if !strings.Contains(out, `"text":"first line"`) || !strings.Contains(out, `"text":"second"`) {
		t.Fatalf("lines not split correctly:\n%s", out)
	}
	if strings.Contains(out, "third") {
		t.Fatalf("unterminated line must wait for its newline:\n%s", out)
	}
}
