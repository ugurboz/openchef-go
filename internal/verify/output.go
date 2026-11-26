package verify

import (
	"fmt"
	"strings"
	"sync"
)

// headTail keeps the first and last bytes of a stream within a fixed budget, so a
// verification command that prints gigabytes costs constant memory and the agent still
// sees how the output began (build errors) and ended (test summary).
type headTail struct {
	mu       sync.Mutex
	headCap  int
	tailCap  int
	head     []byte
	tail     []byte // ring buffer once full
	tailNext int    // next write position in tail when full
	total    int64
}

func newHeadTail(limit int) *headTail {
	headCap := limit / 2
	return &headTail{headCap: headCap, tailCap: limit - headCap}
}

// Write implements io.Writer. It never fails.
func (h *headTail) Write(p []byte) (int, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := len(p)
	h.total += int64(n)
	if room := h.headCap - len(h.head); room > 0 {
		take := min(room, len(p))
		h.head = append(h.head, p[:take]...)
		p = p[take:]
	}
	for len(p) > 0 && h.tailCap > 0 {
		if len(h.tail) < h.tailCap {
			take := min(h.tailCap-len(h.tail), len(p))
			h.tail = append(h.tail, p[:take]...)
			p = p[take:]
			continue
		}
		if len(p) >= h.tailCap {
			copy(h.tail, p[len(p)-h.tailCap:])
			h.tailNext = 0
			break
		}
		c := copy(h.tail[h.tailNext:], p)
		h.tailNext = (h.tailNext + c) % h.tailCap
		p = p[c:]
	}
	return n, nil
}

// result returns the kept output and whether anything was dropped. Cuts may split a
// multi-byte character; invalid UTF-8 at the seams is replaced.
func (h *headTail) result() (string, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	tail := append(append([]byte(nil), h.tail[h.tailNext:]...), h.tail[:h.tailNext]...)
	kept := int64(len(h.head) + len(tail))
	if kept == h.total {
		return strings.ToValidUTF8(string(h.head)+string(tail), "�"), false
	}
	var b strings.Builder
	b.WriteString(strings.ToValidUTF8(string(h.head), "�"))
	fmt.Fprintf(&b, "\n\n[… %d bytes omitted …]\n\n", h.total-kept)
	b.WriteString(strings.ToValidUTF8(string(tail), "�"))
	return b.String(), true
}
