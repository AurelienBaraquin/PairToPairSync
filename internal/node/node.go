// Package node owns the network side of a device: listening, dialling known
// peers, authenticating them, and routing messages to the folder engines.
package node

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sort"
	"sync"
	"time"

	"github.com/aurelienbaraquin/pairtopairsync/internal/config"
	"github.com/aurelienbaraquin/pairtopairsync/internal/identity"
	"github.com/aurelienbaraquin/pairtopairsync/internal/protocol"
)

// FolderHandler is implemented by the folder engines (full copy and relay).
// Callbacks other than PeerUp/PeerDown must return quickly.
type FolderHandler interface {
	ID() string
	Start(ctx context.Context) error
	Stop()
	PeerUp(p *Peer)
	PeerDown(p *Peer)
	HandleIndex(p *Peer, idx protocol.Index)
	HandleIndexAck(p *Peer, ack protocol.IndexAck)
	// HandleRequest returns the requested block or an error code from protocol.Err*.
	HandleRequest(p *Peer, r protocol.Request) ([]byte, string)
	HandleProbe(p *Peer, items []protocol.ProbeItem) []bool
	Status() any
}

// AddressSource lets discovery contribute dial candidates.
type AddressSource interface {
	Addresses(deviceID string) []string
}

type Options struct {
	Config   *config.Config
	Identity *identity.Identity
	Handlers []FolderHandler
	Logger   *slog.Logger
	// Addresses optionally supplies extra addresses (LAN discovery).
	Addresses AddressSource
	// RedialMax caps the reconnect back-off (default 60s).
	RedialMax time.Duration
}

type Node struct {
	cfg      *config.Config
	id       *identity.Identity
	log      *slog.Logger
	handlers map[string]FolderHandler
	addrs    AddressSource
	redial   time.Duration

	ln     net.Listener
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu    sync.Mutex
	peers map[string]*Peer
}

func New(o Options) *Node {
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.RedialMax == 0 {
		o.RedialMax = time.Minute
	}
	n := &Node{cfg: o.Config, id: o.Identity, log: o.Logger, addrs: o.Addresses, redial: o.RedialMax,
		handlers: map[string]FolderHandler{}, peers: map[string]*Peer{}}
	for _, h := range o.Handlers {
		n.handlers[h.ID()] = h
	}
	return n
}

// Start begins listening and dialling. It returns once the listener is up.
func (n *Node) Start(parent context.Context) error {
	n.ctx, n.cancel = context.WithCancel(parent)
	ln, err := net.Listen("tcp", n.cfg.ListenAddr)
	if err != nil {
		n.cancel()
		return fmt.Errorf("listen %s: %w", n.cfg.ListenAddr, err)
	}
	n.ln = ln
	for _, h := range n.handlers {
		if err := h.Start(n.ctx); err != nil {
			n.cancel()
			ln.Close()
			return fmt.Errorf("folder %s: %w", h.ID(), err)
		}
	}
	n.wg.Add(1)
	go n.acceptLoop()
	for _, d := range n.cfg.Devices {
		if d.ID == n.id.ID || !n.sharesAny(d.ID) {
			continue
		}
		n.wg.Add(1)
		go n.dialLoop(d)
	}
	return nil
}

func (n *Node) Stop() {
	if n.cancel == nil {
		return
	}
	n.cancel()
	n.ln.Close()
	n.mu.Lock()
	for _, p := range n.peers {
		p.close()
	}
	n.mu.Unlock()
	n.wg.Wait()
	for _, h := range n.handlers {
		h.Stop()
	}
}

func (n *Node) ListenAddr() net.Addr { return n.ln.Addr() }

// PeerInfo describes a connected peer for the status API.
type PeerInfo struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	Outbound  bool     `json:"outbound"`
	Folders   []string `json:"folders"`
	Connected string   `json:"connected_since"`
}

func (n *Node) Peers() []PeerInfo {
	n.mu.Lock()
	defer n.mu.Unlock()
	var out []PeerInfo
	for _, p := range n.peers {
		pi := PeerInfo{ID: p.id, Name: p.name, Outbound: p.outbound, Connected: p.since.Format(time.RFC3339)}
		for f := range p.shared {
			pi.Folders = append(pi.Folders, f)
		}
		sort.Strings(pi.Folders)
		out = append(out, pi)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (n *Node) Connected(id string) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	_, ok := n.peers[id]
	return ok
}

func (n *Node) sharesAny(id string) bool {
	for i := range n.cfg.Folders {
		if n.cfg.Folders[i].SharesWith(id) {
			return true
		}
	}
	return false
}

func (n *Node) acceptLoop() {
	defer n.wg.Done()
	tlsCfg := n.id.ServerConfig()
	for {
		c, err := n.ln.Accept()
		if err != nil {
			if n.ctx.Err() != nil {
				return
			}
			n.log.Warn("accept failed", "err", err)
			time.Sleep(time.Second)
			continue
		}
		n.wg.Add(1)
		go func() {
			defer n.wg.Done()
			n.session(tls.Server(c, tlsCfg), false, "")
		}()
	}
}

func (n *Node) dialLoop(d config.Device) {
	defer n.wg.Done()
	backoff := time.Second
	for n.ctx.Err() == nil {
		if n.Connected(d.ID) {
			if !n.sleep(3 * time.Second) {
				return
			}
			continue
		}
		cands := append([]string{}, d.Addresses...)
		if n.addrs != nil {
			cands = append(cands, n.addrs.Addresses(d.ID)...)
		}
		connected := false
		for _, a := range cands {
			dctx, cancel := context.WithTimeout(n.ctx, 8*time.Second)
			c, err := (&net.Dialer{KeepAlive: 20 * time.Second}).DialContext(dctx, "tcp", a)
			cancel()
			if err != nil {
				continue
			}
			start := time.Now()
			n.session(tls.Client(c, n.id.ClientConfig()), true, d.ID)
			connected = time.Since(start) > 5*time.Second
			break
		}
		if connected {
			backoff = time.Second
		} else if backoff *= 2; backoff > n.redial {
			backoff = n.redial
		}
		if !n.sleep(backoff) {
			return
		}
	}
}

func (n *Node) sleep(d time.Duration) bool {
	select {
	case <-time.After(d):
		return true
	case <-n.ctx.Done():
		return false
	}
}

// session runs one connection from handshake to close.
func (n *Node) session(tc *tls.Conn, outbound bool, expect string) {
	defer tc.Close()
	hctx, cancel := context.WithTimeout(n.ctx, 10*time.Second)
	err := tc.HandshakeContext(hctx)
	cancel()
	if err != nil {
		n.log.Debug("tls handshake failed", "err", err)
		return
	}
	pid, err := identity.PeerID(tc)
	if err != nil {
		return
	}
	if outbound && pid != expect {
		n.log.Warn("remote device has an unexpected identity; refusing", "expected", identity.Format(expect), "got", identity.Format(pid))
		return
	}
	dev := n.cfg.Device(pid)
	if dev == nil || pid == n.id.ID || !n.sharesAny(pid) {
		n.log.Warn("connection from unknown device refused; add it to the configuration to accept it", "device", identity.Format(pid))
		return
	}

	conn := protocol.NewConn(tc)
	var mine []string
	for i := range n.cfg.Folders {
		if n.cfg.Folders[i].SharesWith(pid) {
			mine = append(mine, n.cfg.Folders[i].ID)
		}
	}
	if err := conn.Send(protocol.TypeHello, protocol.Hello{Version: protocol.Version, Name: n.cfg.DeviceName, Folders: mine}, nil); err != nil {
		return
	}
	_ = conn.SetReadDeadline(time.Now().Add(15 * time.Second))
	t, hb, _, err := conn.Recv()
	if err != nil || t != protocol.TypeHello {
		return
	}
	var hello protocol.Hello
	if json.Unmarshal(hb, &hello) != nil || hello.Version != protocol.Version {
		n.log.Warn("incompatible peer", "device", dev.Name)
		return
	}
	name := hello.Name
	if dev.Name != "" {
		name = dev.Name
	}
	p := newPeer(pid, name, conn, outbound)
	theirs := map[string]bool{}
	for _, f := range hello.Folders {
		theirs[f] = true
	}
	for _, f := range mine {
		if theirs[f] && n.handlers[f] != nil {
			p.shared[f] = true
		}
	}
	if len(p.shared) == 0 {
		n.log.Info("no folder in common with peer", "peer", name)
		return
	}
	if !n.register(p) {
		return
	}
	n.log.Info("connected", "peer", name, "device", identity.Format(pid)[:7], "outbound", outbound)
	defer func() {
		n.unregister(p)
		for f := range p.shared {
			n.handlers[f].PeerDown(p)
		}
		n.log.Info("disconnected", "peer", name)
	}()

	go func() {
		for f := range p.shared {
			n.handlers[f].PeerUp(p)
		}
	}()
	go n.pinger(p)
	n.readLoop(p)
}

// register adds p to the peer table, resolving simultaneous dials: the
// connection initiated by the device with the lower ID survives on both sides.
func (n *Node) register(p *Peer) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	if old, ok := n.peers[p.id]; ok {
		oldWins := dialerID(old, n.id.ID) < dialerID(p, n.id.ID)
		if oldWins {
			return false
		}
		old.close()
	}
	n.peers[p.id] = p
	return true
}

func dialerID(p *Peer, self string) string {
	if p.outbound {
		return self
	}
	return p.id
}

func (n *Node) unregister(p *Peer) {
	p.close()
	n.mu.Lock()
	if n.peers[p.id] == p {
		delete(n.peers, p.id)
	}
	n.mu.Unlock()
}

func (n *Node) pinger(p *Peer) {
	t := time.NewTicker(15 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			if p.conn.Send(protocol.TypePing, struct{}{}, nil) != nil {
				p.close()
				return
			}
		case <-p.done:
			return
		}
	}
}

func (n *Node) readLoop(p *Peer) {
	sem := make(chan struct{}, 16)
	for {
		_ = p.conn.SetReadDeadline(time.Now().Add(60 * time.Second))
		t, hb, body, err := p.conn.Recv()
		if err != nil {
			if n.ctx.Err() == nil && !errors.Is(err, net.ErrClosed) {
				n.log.Debug("read failed", "peer", p.name, "err", err)
			}
			return
		}
		switch t {
		case protocol.TypePing:
		case protocol.TypeIndex:
			var idx protocol.Index
			if json.Unmarshal(hb, &idx) != nil {
				return
			}
			if h := n.handler(p, idx.Folder); h != nil {
				h.HandleIndex(p, idx)
				_ = p.sendIndexAck(idx.Folder, idx.MaxSeq)
			}
		case protocol.TypeIndexAck:
			var ack protocol.IndexAck
			if json.Unmarshal(hb, &ack) != nil {
				return
			}
			if h := n.handler(p, ack.Folder); h != nil {
				h.HandleIndexAck(p, ack)
			}
		case protocol.TypeResponse:
			var r protocol.Response
			if json.Unmarshal(hb, &r) != nil {
				return
			}
			p.deliver(r.ID, reply{hdr: hb, body: body})
		case protocol.TypeProbeReply:
			var r protocol.ProbeReply
			if json.Unmarshal(hb, &r) != nil {
				return
			}
			p.deliver(r.ID, reply{hdr: hb})
		case protocol.TypeRequest:
			var r protocol.Request
			if json.Unmarshal(hb, &r) != nil {
				return
			}
			h := n.handler(p, r.Folder)
			sem <- struct{}{}
			go func() {
				defer func() { <-sem }()
				var data []byte
				code := protocol.ErrNotFound
				if h != nil {
					data, code = h.HandleRequest(p, r)
				}
				if p.conn.Send(protocol.TypeResponse, protocol.Response{ID: r.ID, Err: code}, data) != nil {
					p.close()
				}
			}()
		case protocol.TypeProbe:
			var pr protocol.Probe
			if json.Unmarshal(hb, &pr) != nil {
				return
			}
			h := n.handler(p, pr.Folder)
			go func() {
				have := make([]bool, len(pr.Items))
				if h != nil {
					have = h.HandleProbe(p, pr.Items)
				}
				if p.conn.Send(protocol.TypeProbeReply, protocol.ProbeReply{ID: pr.ID, Have: have}, nil) != nil {
					p.close()
				}
			}()
		default:
			n.log.Debug("unknown message type", "type", t)
		}
	}
}

func (n *Node) handler(p *Peer, folder string) FolderHandler {
	if !p.shared[folder] {
		return nil
	}
	return n.handlers[folder]
}
