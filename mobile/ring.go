package mobile

import (
	"strings"
	"sync"
)

// ring keeps the last N log lines in memory (diagnostics screen), so nothing
// about the user's files or devices is ever written to disk or the system log.
type ring struct {
	mu    sync.Mutex
	lines []string
	max   int
	part  string
}

func newRing(max int) *ring { return &ring{max: max} }

func (r *ring) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.part += string(p)
	for {
		i := strings.IndexByte(r.part, '\n')
		if i < 0 {
			break
		}
		r.lines = append(r.lines, r.part[:i])
		r.part = r.part[i+1:]
	}
	if over := len(r.lines) - r.max; over > 0 {
		r.lines = r.lines[over:]
	}
	return len(p), nil
}

func (r *ring) String() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return strings.Join(r.lines, "\n")
}
