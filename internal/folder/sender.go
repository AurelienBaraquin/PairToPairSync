package folder

import (
	"sync"
	"sync/atomic"

	"github.com/aurelienbaraquin/pairtopairsync/internal/model"
	"github.com/aurelienbaraquin/pairtopairsync/internal/node"
)

// sender delivers index entries to one peer strictly in the order they were
// queued. Sending each broadcast from its own goroutine would let batches overtake
// each other on the wire.
type sender struct {
	peer    *node.Peer
	sentMax atomic.Int64 // highest local sequence number actually handed to the connection

	mu    sync.Mutex
	queue [][]model.FileInfo
	wake  chan struct{}
}

func newSender(p *node.Peer) *sender {
	return &sender{peer: p, wake: make(chan struct{}, 1)}
}

func (s *sender) enqueue(entries []model.FileInfo) {
	if len(entries) == 0 {
		return
	}
	s.mu.Lock()
	s.queue = append(s.queue, entries)
	s.mu.Unlock()
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// run sends queued entries until the connection ends or the folder stops.
func (s *sender) run(f *Folder) {
	for {
		s.mu.Lock()
		batches := s.queue
		s.queue = nil
		s.mu.Unlock()
		for _, entries := range batches {
			if !f.sendTo(s.peer, entries, &s.sentMax) {
				return // the connection is gone; PeerUp offers everything again on reconnect
			}
		}
		if len(batches) > 0 {
			continue
		}
		select {
		case <-s.wake:
		case <-s.peer.Done():
			return
		case <-f.ctx.Done():
			return
		}
	}
}
