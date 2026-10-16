package node

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"

	"github.com/aurelienbaraquin/pairtopairsync/internal/model"
	"github.com/aurelienbaraquin/pairtopairsync/internal/protocol"
)

var ErrPeerClosed = errors.New("peer connection closed")

// Peer is one live, authenticated session with a remote device. Its long-lived
// "session" stream carries index updates; every block or probe request uses its
// own short stream, so a slow transfer never delays control traffic.
type Peer struct {
	n        *Node
	pid      peer.ID
	id       string
	name     string
	conn     *protocol.Conn
	outbound bool
	viaRelay bool // the session runs over a relayed (not direct) connection
	shared   map[string]bool
	since    time.Time

	done chan struct{}
	once sync.Once
}

func newPeer(n *Node, pid peer.ID, name string, conn *protocol.Conn, outbound, viaRelay bool) *Peer {
	return &Peer{n: n, pid: pid, id: pid.String(), name: name, conn: conn, outbound: outbound,
		viaRelay: viaRelay, since: time.Now(), shared: map[string]bool{}, done: make(chan struct{})}
}

func (p *Peer) ID() string   { return p.id }
func (p *Peer) Name() string { return p.name }

// Done is closed when the session ends.
func (p *Peer) Done() <-chan struct{} { return p.done }

func (p *Peer) close() {
	p.once.Do(func() {
		close(p.done)
		_ = p.conn.Close()
	})
}

// SendIndex sends file metadata (plain between full devices, sealed to a relay).
func (p *Peer) SendIndex(idx protocol.Index) error {
	err := p.conn.Send(protocol.TypeIndex, idx, nil)
	if err != nil {
		p.close()
	}
	return err
}

func (p *Peer) sendIndexAck(folder string, seq int64) error {
	return p.conn.Send(protocol.TypeIndexAck, protocol.IndexAck{Folder: folder, MaxSeq: seq}, nil)
}

// exchange opens a fresh stream, sends one request frame and returns the reply.
func (p *Peer) exchange(ctx context.Context, proto protocolID, t protocol.Type, hdr any) ([]byte, []byte, error) {
	select {
	case <-p.done:
		return nil, nil, ErrPeerClosed
	default:
	}
	s, err := p.n.openStream(ctx, p.pid, proto, false)
	if err != nil {
		return nil, nil, err
	}
	defer s.Close()
	if dl, ok := ctx.Deadline(); ok {
		_ = s.SetDeadline(dl)
	}
	// Abort the stream if the caller gives up or the session dies.
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		select {
		case <-ctx.Done():
			_ = s.Reset()
		case <-p.done:
			_ = s.Reset()
		case <-stop:
		}
	}()
	c := protocol.NewConn(s)
	if err := c.Send(t, hdr, nil); err != nil {
		_ = s.Reset()
		return nil, nil, err
	}
	_, rh, rb, err := c.Recv()
	if err != nil {
		_ = s.Reset()
		if ctx.Err() != nil {
			return nil, nil, ctx.Err()
		}
		return nil, nil, err
	}
	return rh, rb, nil
}

// Request fetches one block. A plain block is verified here against its
// SHA-256. A sealed block (r.Sealed) is ciphertext whose authenticity is checked
// by the caller when it decrypts; only its length is checked here.
func (p *Peer) Request(ctx context.Context, r protocol.Request) ([]byte, error) {
	rh, body, err := p.exchange(ctx, protoBlock, protocol.TypeRequest, r)
	if err != nil {
		return nil, err
	}
	var resp protocol.Response
	if err := json.Unmarshal(rh, &resp); err != nil {
		return nil, err
	}
	if resp.Err != "" {
		return nil, fmt.Errorf("peer %s: %s", p.name, resp.Err)
	}
	if r.Sealed {
		if len(body) != r.Size+protocol.SealOverhead {
			return nil, fmt.Errorf("peer %s: sealed block has the wrong size", p.name)
		}
		return body, nil
	}
	if len(body) != r.Size || string(model.HashBytes(body)) != string(r.Hash) {
		return nil, fmt.Errorf("peer %s: block failed verification", p.name)
	}
	return body, nil
}

// Probe asks which of the given versions the peer already holds.
func (p *Peer) Probe(ctx context.Context, folder string, items []protocol.ProbeItem) ([]bool, error) {
	rh, _, err := p.exchange(ctx, protoProbe, protocol.TypeProbe, protocol.Probe{Folder: folder, Items: items})
	if err != nil {
		return nil, err
	}
	var pr protocol.ProbeReply
	if err := json.Unmarshal(rh, &pr); err != nil {
		return nil, err
	}
	if len(pr.Have) != len(items) {
		return nil, errors.New("malformed probe reply")
	}
	return pr.Have, nil
}

func isRelayedConn(c network.Conn) bool {
	return strings.Contains(c.RemoteMultiaddr().String(), "/p2p-circuit")
}
