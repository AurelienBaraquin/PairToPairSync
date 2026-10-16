package node

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aurelienbaraquin/pairtopairsync/internal/model"
	"github.com/aurelienbaraquin/pairtopairsync/internal/protocol"
)

var ErrPeerClosed = errors.New("peer connection closed")

// Peer is one live, authenticated connection to a remote device.
type Peer struct {
	id       string
	name     string
	conn     *protocol.Conn
	outbound bool
	shared   map[string]bool // folder IDs shared with this peer
	since    time.Time

	mu      sync.Mutex
	waiters map[uint32]chan reply
	nextID  atomic.Uint32
	done    chan struct{}
	once    sync.Once
}

type reply struct {
	hdr  []byte
	body []byte
}

func newPeer(id, name string, conn *protocol.Conn, outbound bool) *Peer {
	return &Peer{id: id, name: name, conn: conn, outbound: outbound, since: time.Now(),
		shared: map[string]bool{}, waiters: map[uint32]chan reply{}, done: make(chan struct{})}
}

func (p *Peer) ID() string   { return p.id }
func (p *Peer) Name() string { return p.name }

// Done is closed when the connection goes away.
func (p *Peer) Done() <-chan struct{} { return p.done }

func (p *Peer) close() {
	p.once.Do(func() {
		close(p.done)
		_ = p.conn.Close()
	})
}

// SendIndex sends file metadata for a folder.
func (p *Peer) SendIndex(folder string, entries []model.FileInfo, maxSeq int64) error {
	return p.conn.Send(protocol.TypeIndex, protocol.Index{Folder: folder, Entries: entries, MaxSeq: maxSeq}, nil)
}

func (p *Peer) sendIndexAck(folder string, seq int64) error {
	return p.conn.Send(protocol.TypeIndexAck, protocol.IndexAck{Folder: folder, MaxSeq: seq}, nil)
}

func (p *Peer) call(ctx context.Context, t protocol.Type, build func(id uint32) any) (reply, error) {
	id := p.nextID.Add(1)
	ch := make(chan reply, 1)
	p.mu.Lock()
	p.waiters[id] = ch
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		delete(p.waiters, id)
		p.mu.Unlock()
	}()
	if err := p.conn.Send(t, build(id), nil); err != nil {
		p.close()
		return reply{}, err
	}
	select {
	case r := <-ch:
		return r, nil
	case <-ctx.Done():
		return reply{}, ctx.Err()
	case <-p.done:
		return reply{}, ErrPeerClosed
	}
}

// Request fetches one block from the peer and verifies its hash.
func (p *Peer) Request(ctx context.Context, folder, name string, off int64, size int, hash []byte) ([]byte, error) {
	r, err := p.call(ctx, protocol.TypeRequest, func(id uint32) any {
		return protocol.Request{ID: id, Folder: folder, Name: name, Offset: off, Size: size, Hash: hash}
	})
	if err != nil {
		return nil, err
	}
	var resp protocol.Response
	if err := json.Unmarshal(r.hdr, &resp); err != nil {
		return nil, err
	}
	if resp.Err != "" {
		return nil, fmt.Errorf("peer %s: %s", p.name, resp.Err)
	}
	if len(r.body) != size || string(model.HashBytes(r.body)) != string(hash) {
		return nil, fmt.Errorf("peer %s: block failed verification", p.name)
	}
	return r.body, nil
}

// Probe asks which of the given versions the peer already holds.
func (p *Peer) Probe(ctx context.Context, folder string, items []protocol.ProbeItem) ([]bool, error) {
	r, err := p.call(ctx, protocol.TypeProbe, func(id uint32) any {
		return protocol.Probe{ID: id, Folder: folder, Items: items}
	})
	if err != nil {
		return nil, err
	}
	var pr protocol.ProbeReply
	if err := json.Unmarshal(r.hdr, &pr); err != nil {
		return nil, err
	}
	if len(pr.Have) != len(items) {
		return nil, errors.New("malformed probe reply")
	}
	return pr.Have, nil
}

func (p *Peer) deliver(id uint32, r reply) {
	p.mu.Lock()
	ch := p.waiters[id]
	p.mu.Unlock()
	if ch != nil {
		select {
		case ch <- r:
		default:
		}
	}
}
