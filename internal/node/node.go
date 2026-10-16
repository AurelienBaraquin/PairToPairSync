// Package node owns the network side of a device. It is built on libp2p:
//
//   - transports: QUIC (TLS 1.3, survives IP changes) with TCP+Noise as fallback;
//     every connection is mutually authenticated by the devices' Ed25519 keys;
//   - a connection gater that refuses any device that is not in the configuration
//     before a single byte of application data is exchanged;
//   - reachability without any third-party server: mDNS on the LAN, UPnP/NAT-PMP,
//     AutoNAT, hole punching, and circuit relaying through *your own* devices;
//   - automatic reconnection with jittered back-off, and independent streams for
//     control traffic and for each block transfer.
package node

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"net"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/libp2p/go-libp2p"
	dht "github.com/libp2p/go-libp2p-kad-dht"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/core/peerstore"
	lprotocol "github.com/libp2p/go-libp2p/core/protocol"
	"github.com/libp2p/go-libp2p/core/routing"
	"github.com/libp2p/go-libp2p/p2p/discovery/mdns"
	"github.com/libp2p/go-libp2p/p2p/host/autorelay"
	relayv2 "github.com/libp2p/go-libp2p/p2p/protocol/circuitv2/relay"
	ma "github.com/multiformats/go-multiaddr"

	"github.com/aurelienbaraquin/pairtopairsync/internal/config"
	"github.com/aurelienbaraquin/pairtopairsync/internal/identity"
	"github.com/aurelienbaraquin/pairtopairsync/internal/protocol"
)

type protocolID = lprotocol.ID

const (
	protoSession protocolID = "/p2psync/1.0/session"
	protoBlock   protocolID = "/p2psync/1.0/block"
	protoProbe   protocolID = "/p2psync/1.0/probe"
	protoPair    protocolID = protocol.PairProtocol
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

// PairHandler answers invitations (see package pairing).
type PairHandler interface {
	// Active reports whether any invitation is outstanding.
	Active() bool
	// Banned reports whether this device has failed too many times to be let in again.
	Banned(peer.ID) bool
	// Accept validates the request. remoteIP is the guest's IP when it connected
	// directly ("" if relayed). On success it returns commit, which the node runs
	// *after* the reply has been delivered (it changes the configuration, which
	// restarts the daemon).
	Accept(remote peer.ID, remoteIP string, req protocol.PairRequest) (resp protocol.PairResponse, commit func())
}

type Options struct {
	Config   *config.Config
	Identity *identity.Identity
	Handlers []FolderHandler
	Logger   *slog.Logger
	// RedialMax caps the reconnect back-off (default 60s).
	RedialMax time.Duration
	// Pairing, if set, lets a device that presents a valid invitation token join.
	Pairing PairHandler
	// Reachability forces "public" or "private" instead of auto-detecting
	// (testing, or a server that knows better).
	Reachability string
}

type Node struct {
	cfg      *config.Config
	id       *identity.Identity
	log      *slog.Logger
	handlers map[string]FolderHandler
	opts     Options
	redial   time.Duration
	known    map[peer.ID]config.Device

	host   host.Host
	dht    *dht.IpfsDHT
	mdns   mdns.Service
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu          sync.Mutex
	peers       map[string]*Peer
	opening     map[peer.ID]bool
	strangers   int
	strangerIPs map[string]int
	pairing     map[peer.ID]bool        // strangers with a pairing exchange in progress
	slots       map[network.Conn]func() // release functions of stranger connections
	kick        map[peer.ID]chan struct{}
	disco       map[peer.ID][]ma.Multiaddr // addresses learnt from configuration / mDNS (relay candidates)
}

func New(o Options) *Node {
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.RedialMax == 0 {
		o.RedialMax = 30 * time.Second
	}
	n := &Node{cfg: o.Config, id: o.Identity, log: o.Logger, opts: o, redial: o.RedialMax,
		handlers: map[string]FolderHandler{}, known: map[peer.ID]config.Device{},
		peers: map[string]*Peer{}, opening: map[peer.ID]bool{}, strangerIPs: map[string]int{}, pairing: map[peer.ID]bool{}, slots: map[network.Conn]func(){}, kick: map[peer.ID]chan struct{}{},
		disco: map[peer.ID][]ma.Multiaddr{}}
	for _, h := range o.Handlers {
		n.handlers[h.ID()] = h
	}
	for _, d := range o.Config.Devices {
		if pid, err := identity.Decode(d.ID); err == nil && d.ID != o.Identity.ID && n.sharesAny(d.ID) {
			n.known[pid] = d
			// Known before the host exists, so AutoRelay finds relay candidates at once.
			for _, a := range d.Addresses {
				n.disco[pid] = append(n.disco[pid], ParseAddr(a)...)
			}
		}
	}
	return n
}

func (n *Node) sharesAny(id string) bool {
	for i := range n.cfg.Folders {
		if n.cfg.Folders[i].SharesWith(id) {
			return true
		}
	}
	return false
}

// Start brings the node online. It returns once the host is listening.
func (n *Node) Start(parent context.Context) error {
	n.ctx, n.cancel = context.WithCancel(parent)
	opts := n.libp2pOptions()
	h, err := libp2p.New(opts...)
	if err != nil {
		n.cancel()
		return fmt.Errorf("start network: %w", err)
	}
	n.host = h
	h.SetStreamHandler(protoSession, func(s network.Stream) { n.wg.Add(1); defer n.wg.Done(); n.runSession(s, false) })
	h.SetStreamHandler(protoBlock, n.serveBlock)
	h.SetStreamHandler(protoProbe, n.serveProbe)
	h.SetStreamHandler(protoPair, n.servePair)
	h.Network().Notify(&notifee{n})

	// Static addresses from the configuration.
	for pid, d := range n.known {
		n.addDeviceAddrs(pid, d)
	}
	if n.dht != nil {
		_ = n.dht.Bootstrap(n.ctx)
	}
	if n.cfg.LANDiscover {
		n.mdns = mdns.NewMdnsService(h, "_p2psync._udp", &mdnsNotifee{n})
		if err := n.mdns.Start(); err != nil {
			n.log.Warn("LAN discovery unavailable", "err", err)
			n.mdns = nil
		}
	}
	for _, hd := range n.handlers {
		if err := hd.Start(n.ctx); err != nil {
			n.Stop()
			return fmt.Errorf("folder %s: %w", hd.ID(), err)
		}
	}
	for pid := range n.known {
		n.wg.Add(1)
		go n.dialLoop(pid)
	}
	return nil
}

func (n *Node) listenAddrs() []string {
	if len(n.cfg.ListenAddresses) > 0 {
		return n.cfg.ListenAddresses
	}
	p := strconv.Itoa(n.cfg.ListenPort)
	return []string{
		"/ip4/0.0.0.0/udp/" + p + "/quic-v1", "/ip4/0.0.0.0/tcp/" + p,
		"/ip6/::/udp/" + p + "/quic-v1", "/ip6/::/tcp/" + p,
	}
}

func (n *Node) libp2pOptions() []libp2p.Option {
	opts := []libp2p.Option{
		libp2p.Identity(n.id.Key),
		libp2p.ListenAddrStrings(n.listenAddrs()...),
		libp2p.ConnectionGater(&gater{allowed: n.isKnown, pairing: n.pairingActive, banned: n.pairingBanned}),
		libp2p.UserAgent("p2psync/0.2"),
		libp2p.EnableRelay(),
		libp2p.WithDialTimeout(12 * time.Second),
	}
	switch n.opts.Reachability {
	case "public":
		opts = append(opts, libp2p.ForceReachabilityPublic())
	case "private":
		opts = append(opts, libp2p.ForceReachabilityPrivate())
	}
	if n.cfg.NATTraversal {
		opts = append(opts,
			libp2p.NATPortMap(),         // UPnP / NAT-PMP / PCP on the user's own router
			libp2p.EnableAutoNATv2(),    // learn from our own devices whether we are reachable
			libp2p.EnableHolePunching(), // direct connection through NATs (DCUtR)
			libp2p.EnableAutoRelayWithPeerSource(n.relayCandidates,
				// With a handful of personal devices, do not wait for a crowd of candidates.
				autorelay.WithBootDelay(0), autorelay.WithMinCandidates(1), autorelay.WithMinInterval(5*time.Second)),
		)
	}
	if n.cfg.RelayService {
		// Our devices may relay each other without data caps; the ACL and the
		// connection gater restrict this to the configured devices only.
		opts = append(opts, libp2p.EnableRelayService(relayv2.WithInfiniteLimits(), relayv2.WithACL(&relayACL{n})))
	}
	if n.cfg.DHT {
		opts = append(opts, libp2p.Routing(func(h host.Host) (routing.PeerRouting, error) {
			d, err := dht.New(gatedHost{Host: h, allow: n.isKnown},
				dht.ProtocolPrefix("/p2psync"), // a private DHT: only our devices can join it
				dht.Mode(dht.ModeAutoServer), dht.DisableProviders(), dht.DisableValues())
			n.dht = d
			return d, err
		}))
	}
	return opts
}

func (n *Node) isKnown(p peer.ID) bool { _, ok := n.known[p]; return ok }

func (n *Node) pairingActive() bool { return n.opts.Pairing != nil && n.opts.Pairing.Active() }

func (n *Node) pairingBanned(p peer.ID) bool {
	return n.opts.Pairing != nil && n.opts.Pairing.Banned(p)
}

// gatedHost wraps the host handed to the DHT so that its inbound protocol is
// answered for configured devices only. Without this, a stranger admitted for
// pairing could query the DHT and learn the addresses of our other devices.
type gatedHost struct {
	host.Host
	allow func(peer.ID) bool
}

func (g gatedHost) gate(h network.StreamHandler) network.StreamHandler {
	return func(s network.Stream) {
		if !g.allow(s.Conn().RemotePeer()) {
			_ = s.Reset()
			return
		}
		h(s)
	}
}

func (g gatedHost) SetStreamHandler(pid protocolID, h network.StreamHandler) {
	g.Host.SetStreamHandler(pid, g.gate(h))
}

func (g gatedHost) SetStreamHandlerMatch(pid protocolID, m func(protocolID) bool, h network.StreamHandler) {
	g.Host.SetStreamHandlerMatch(pid, m, g.gate(h))
}

// relayACL lets only configured devices use this device as a circuit relay.
type relayACL struct{ n *Node }

func (a *relayACL) AllowReserve(p peer.ID, _ ma.Multiaddr) bool { return a.n.isKnown(p) }
func (a *relayACL) AllowConnect(src peer.ID, _ ma.Multiaddr, dst peer.ID) bool {
	return a.n.isKnown(src) && a.n.isKnown(dst)
}

// relayCandidates feeds AutoRelay with our own devices: those that are publicly
// reachable will accept a reservation, the others simply refuse.
func (n *Node) relayCandidates(ctx context.Context, num int) <-chan peer.AddrInfo {
	ch := make(chan peer.AddrInfo, max(num, 1))
	go func() {
		defer close(ch)
		n.mu.Lock()
		var infos []peer.AddrInfo
		for pid, addrs := range n.disco {
			if len(addrs) > 0 {
				infos = append(infos, peer.AddrInfo{ID: pid, Addrs: addrs})
			}
		}
		n.mu.Unlock()
		for _, ai := range infos {
			select {
			case ch <- ai:
			case <-ctx.Done():
				return
			}
		}
	}()
	return ch
}

// Stop shuts the node down. Every step that depends on third-party code is
// bounded in time: a library that fails to close must never keep the daemon
// alive (or block a configuration reload).
func (n *Node) Stop() {
	if n.cancel == nil {
		return
	}
	n.cancel()
	n.mu.Lock()
	for _, p := range n.peers {
		p.close()
	}
	n.mu.Unlock()
	if n.mdns != nil {
		n.bounded("LAN discovery", func() { _ = n.mdns.Close() })
	}
	if n.dht != nil {
		n.bounded("DHT", func() { _ = n.dht.Close() })
	}
	if n.host != nil {
		n.bounded("network host", func() { _ = n.host.Close() })
	}
	n.bounded("sessions", n.wg.Wait)
	for _, h := range n.handlers {
		h.Stop()
	}
}

func (n *Node) bounded(what string, fn func()) {
	done := make(chan struct{})
	go func() { defer close(done); fn() }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		n.log.Warn("did not stop in time, continuing", "component", what)
	}
}

// Addrs lists the multiaddrs (ending in /p2p/<ID>) at which this device can be
// reached; one of them is what you give to another device to pair with it.
func (n *Node) Addrs() []string {
	var out []string
	for _, a := range n.host.Addrs() {
		out = append(out, a.String()+"/p2p/"+n.id.ID)
	}
	sort.Strings(out)
	return out
}

// PeerInfo describes a connected peer for the status API.
type PeerInfo struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	Outbound  bool     `json:"outbound"`
	Relayed   bool     `json:"relayed"`
	Folders   []string `json:"folders"`
	Connected string   `json:"connected_since"`
}

func (n *Node) Peers() []PeerInfo {
	n.mu.Lock()
	defer n.mu.Unlock()
	var out []PeerInfo
	for _, p := range n.peers {
		pi := PeerInfo{ID: p.id, Name: p.name, Outbound: p.outbound, Relayed: p.viaRelay, Connected: p.since.Format(time.RFC3339)}
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

// ---- addresses ---------------------------------------------------------------

// ParseAddr turns "host:port" into the multiaddrs to try (QUIC first, then TCP),
// or accepts a full multiaddr.
func ParseAddr(s string) []ma.Multiaddr {
	if len(s) > 0 && s[0] == '/' {
		if m, err := ma.NewMultiaddr(s); err == nil {
			return []ma.Multiaddr{m}
		}
		return nil
	}
	host, port, err := net.SplitHostPort(s)
	if err != nil {
		return nil
	}
	proto := "dns"
	if ip := net.ParseIP(host); ip != nil {
		if ip.To4() != nil {
			proto = "ip4"
		} else {
			proto = "ip6"
		}
	}
	var out []ma.Multiaddr
	for _, f := range []string{"/%s/%s/udp/%s/quic-v1", "/%s/%s/tcp/%s"} {
		if m, err := ma.NewMultiaddr(fmt.Sprintf(f, proto, host, port)); err == nil {
			out = append(out, m)
		}
	}
	return out
}

func (n *Node) addDeviceAddrs(pid peer.ID, d config.Device) {
	var all []ma.Multiaddr
	for _, a := range d.Addresses {
		all = append(all, ParseAddr(a)...)
	}
	if len(all) == 0 {
		return
	}
	n.host.Peerstore().AddAddrs(pid, all, peerstore.PermanentAddrTTL)
}

type mdnsNotifee struct{ n *Node }

func (m *mdnsNotifee) HandlePeerFound(pi peer.AddrInfo) {
	if !m.n.isKnown(pi.ID) {
		return
	}
	m.n.host.Peerstore().AddAddrs(pi.ID, pi.Addrs, peerstore.TempAddrTTL)
	m.n.mu.Lock()
	m.n.disco[pi.ID] = append(m.n.disco[pi.ID], pi.Addrs...)
	m.n.mu.Unlock()
	m.n.wake(pi.ID)
}

func (n *Node) wake(pid peer.ID) {
	n.mu.Lock()
	ch := n.kick[pid]
	n.mu.Unlock()
	if ch != nil {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

// ---- connection supervision ---------------------------------------------------

// dialLoop keeps trying to reach one device, forever, with jittered exponential
// back-off. It resets quickly when the network comes back (mDNS discovery, an
// inbound connection or a network change wake it up).
func (n *Node) dialLoop(pid peer.ID) {
	defer n.wg.Done()
	wake := make(chan struct{}, 1)
	n.mu.Lock()
	n.kick[pid] = wake
	n.mu.Unlock()

	backoff := time.Second
	for n.ctx.Err() == nil {
		if n.Connected(pid.String()) {
			if !n.wait(2*time.Second, wake) {
				return
			}
			backoff = time.Second
			continue
		}
		cctx, cancel := context.WithTimeout(n.ctx, 20*time.Second)
		err := n.host.Connect(cctx, peer.AddrInfo{ID: pid})
		cancel()
		if err == nil {
			started := time.Now()
			// Only one side should open the session: the device with the lower ID does so at
			// once, the other lets it. Two sessions opened simultaneously would have one of
			// them torn down mid-transfer by the tie-break.
			n.ensureSession(pid, sessionDelay(n.id.ID, pid.String()))
			if time.Since(started) > 10*time.Second {
				backoff = time.Second
			}
		} else {
			n.log.Debug("cannot reach device yet", "device", pid.ShortString(), "err", err)
		}
		d := backoff + time.Duration(rand.Int63n(int64(backoff)/2+1)) // jitter
		if !n.wait(d, wake) {
			return
		}
		if backoff *= 2; backoff > n.redial {
			backoff = n.redial
		}
	}
}

func (n *Node) wait(d time.Duration, wake <-chan struct{}) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-wake:
		return true
	case <-n.ctx.Done():
		return false
	}
}

// sessionDelay is how long a device waits before opening the session itself: the
// lower ID goes first; the other only steps in if nothing happened.
func sessionDelay(self, other string) time.Duration {
	if self < other {
		return 0
	}
	return 3 * time.Second
}

// ensureSession opens the session stream to pid if none is running. It blocks
// for the lifetime of the session it creates.
func (n *Node) ensureSession(pid peer.ID, delay time.Duration) {
	if delay > 0 && !n.wait(delay, nil) {
		return
	}
	n.mu.Lock()
	if _, ok := n.peers[pid.String()]; ok || n.opening[pid] {
		n.mu.Unlock()
		return
	}
	n.opening[pid] = true
	n.mu.Unlock()
	defer func() {
		n.mu.Lock()
		delete(n.opening, pid)
		n.mu.Unlock()
	}()

	ctx, cancel := context.WithTimeout(n.ctx, 30*time.Second)
	s, err := n.openStream(ctx, pid, protoSession, true)
	cancel()
	if err != nil {
		n.log.Debug("cannot open session", "device", pid.ShortString(), "err", err)
		return
	}
	n.runSession(s, true)
}

// openStream opens a stream to pid. A direct connection is always preferred;
// when only a relayed, limited connection exists and patient is set, it first
// gives hole punching a few seconds to produce a direct one.
func (n *Node) openStream(ctx context.Context, pid peer.ID, proto protocolID, patient bool) (network.Stream, error) {
	s, err := n.host.NewStream(ctx, pid, proto)
	if err == nil {
		return s, nil
	}
	if patient {
		for i := 0; i < 5 && ctx.Err() == nil; i++ {
			if !n.wait(time.Second, nil) {
				break
			}
			if s, err = n.host.NewStream(ctx, pid, proto); err == nil {
				return s, nil
			}
		}
	}
	return n.host.NewStream(network.WithAllowLimitedConn(ctx, "p2psync"), pid, proto)
}

// ---- sessions ------------------------------------------------------------------

func (n *Node) runSession(s network.Stream, outbound bool) {
	defer s.Reset()
	pid := s.Conn().RemotePeer()
	dev, ok := n.known[pid]
	if !ok {
		return
	}
	conn := protocol.NewConn(s)
	var mine []string
	for i := range n.cfg.Folders {
		if n.cfg.Folders[i].SharesWith(pid.String()) {
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
	name := config.Clean(hello.Name) // shown in the UI: never trust it as markup
	if dev.Name != "" {
		name = dev.Name
	}
	p := newPeer(n, pid, name, conn, outbound, isRelayedConn(s.Conn()))
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
	n.log.Info("connected", "peer", name, "device", pid.ShortString(), "outbound", outbound, "relayed", p.viaRelay)
	// PeerUp runs on its own goroutine (it sends the index, which must not block the
	// reader), so it must be finished before PeerDown: otherwise a late PeerUp could
	// re-register a session that has already ended, and a handler would keep talking
	// to a dead connection forever.
	upDone := make(chan struct{})
	defer func() {
		n.unregister(p)
		<-upDone
		for f := range p.shared {
			n.handlers[f].PeerDown(p)
		}
		n.log.Info("disconnected", "peer", name)
		n.wake(pid)
	}()

	go func() {
		defer close(upDone)
		for f := range p.shared {
			select {
			case <-p.done: // replaced or closed before we got here
				return
			default:
			}
			n.handlers[f].PeerUp(p)
		}
	}()
	go n.pinger(p)
	n.readLoop(p)
}

// register adds p to the peer table, resolving simultaneous sessions: the one
// opened by the device with the lower ID survives on both sides.
func (n *Node) register(p *Peer) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	if old, ok := n.peers[p.id]; ok {
		if dialerID(old, n.id.ID) < dialerID(p, n.id.ID) {
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

// pinger detects dead links quickly (a vanished Wi-Fi does not close sockets).
func (n *Node) pinger(p *Peer) {
	t := time.NewTicker(8 * time.Second)
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
	for {
		_ = p.conn.SetReadDeadline(time.Now().Add(30 * time.Second))
		t, hb, _, err := p.conn.Recv()
		if err != nil {
			if n.ctx.Err() == nil && !errors.Is(err, net.ErrClosed) {
				n.log.Debug("session ended", "peer", p.name, "err", err)
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
		default:
			n.log.Debug("unexpected message on session stream", "type", t)
		}
	}
}

func (n *Node) handler(p *Peer, folder string) FolderHandler {
	if !p.shared[folder] {
		return nil
	}
	return n.handlers[folder]
}

func (n *Node) sessionPeer(s network.Stream) *Peer {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.peers[s.Conn().RemotePeer().String()]
}

// serveBlock answers one block request on its own stream.
func (n *Node) serveBlock(s network.Stream) {
	defer s.Close()
	p := n.sessionPeer(s)
	if p == nil {
		_ = s.Reset()
		return
	}
	n.wg.Add(1)
	defer n.wg.Done()
	_ = s.SetDeadline(time.Now().Add(3 * time.Minute))
	c := protocol.NewConn(s)
	t, hb, _, err := c.Recv()
	var r protocol.Request
	if err != nil || t != protocol.TypeRequest || json.Unmarshal(hb, &r) != nil {
		_ = s.Reset()
		return
	}
	var data []byte
	code := protocol.ErrNotFound
	if h := n.handler(p, r.Folder); h != nil {
		data, code = h.HandleRequest(p, r)
	}
	_ = c.Send(protocol.TypeResponse, protocol.Response{Err: code}, data)
}

func (n *Node) serveProbe(s network.Stream) {
	defer s.Close()
	p := n.sessionPeer(s)
	if p == nil {
		_ = s.Reset()
		return
	}
	n.wg.Add(1)
	defer n.wg.Done()
	_ = s.SetDeadline(time.Now().Add(time.Minute))
	c := protocol.NewConn(s)
	t, hb, _, err := c.Recv()
	var pr protocol.Probe
	if err != nil || t != protocol.TypeProbe || json.Unmarshal(hb, &pr) != nil {
		_ = s.Reset()
		return
	}
	have := make([]bool, len(pr.Items))
	if h := n.handler(p, pr.Folder); h != nil {
		have = h.HandleProbe(p, pr.Items)
	}
	_ = c.Send(protocol.TypeProbeReply, protocol.ProbeReply{Have: have}, nil)
}

// notifee reacts to connections appearing and disappearing.
type notifee struct{ n *Node }

func (nf *notifee) Connected(_ network.Network, c network.Conn) {
	n := nf.n
	pid := c.RemotePeer()
	if n.ctx.Err() != nil {
		return
	}
	if !n.isKnown(pid) {
		n.limitStranger(c)
		return
	}
	// A direct connection appeared while the session runs through a relay (hole
	// punching succeeded): move the session to the better path.
	if !isRelayedConn(c) {
		n.mu.Lock()
		p := n.peers[pid.String()]
		n.mu.Unlock()
		if p != nil && p.viaRelay {
			n.log.Info("direct connection established, leaving the relay", "peer", p.name)
			p.close()
		}
	}
	// The device with the lower ID opens the session right away; the other one
	// only does so if nothing happens, which avoids needless duplicate sessions.
	delay := sessionDelay(n.id.ID, pid.String())
	n.wg.Add(1)
	go func() { defer n.wg.Done(); n.ensureSession(pid, delay) }()
}

func (nf *notifee) Disconnected(_ network.Network, c network.Conn) {
	nf.n.wake(c.RemotePeer())
}
func (nf *notifee) Listen(network.Network, ma.Multiaddr)      {}
func (nf *notifee) ListenClose(network.Network, ma.Multiaddr) {}

// ---- pairing ---------------------------------------------------------------------

const (
	maxStrangers      = 6
	maxStrangersPerIP = 2
	strangerWindow    = 15 * time.Second
)

// limitStranger bounds what a device admitted only because an invitation is
// open can do: few at a time (and few per address), and each is dropped after a
// short window unless it has paired by then. The place it occupies is freed as
// soon as its pairing exchange ends (see releaseStranger), not only when the
// window elapses, so one guest never blocks the next.
func (n *Node) limitStranger(c network.Conn) {
	ip := remoteIP(c)
	n.mu.Lock()
	if n.strangers >= maxStrangers || (ip != "" && n.strangerIPs[ip] >= maxStrangersPerIP) {
		n.mu.Unlock()
		_ = c.Close()
		return
	}
	n.strangers++
	if ip != "" {
		n.strangerIPs[ip]++
	}
	var once sync.Once
	n.slots[c] = func() {
		once.Do(func() {
			n.mu.Lock()
			n.strangers--
			if ip != "" {
				if n.strangerIPs[ip]--; n.strangerIPs[ip] <= 0 {
					delete(n.strangerIPs, ip)
				}
			}
			n.mu.Unlock()
		})
	}
	n.mu.Unlock()
	time.AfterFunc(strangerWindow, func() {
		_ = c.Close()
		n.releaseStranger(c)
	})
}

// releaseStranger frees the place held by a stranger's connection.
func (n *Node) releaseStranger(c network.Conn) {
	n.mu.Lock()
	release := n.slots[c]
	delete(n.slots, c)
	n.mu.Unlock()
	if release != nil {
		release()
	}
}

func remoteIP(c network.Conn) string {
	if isRelayedConn(c) {
		return ""
	}
	first, _ := ma.SplitFirst(c.RemoteMultiaddr())
	if first == nil {
		return ""
	}
	switch first.Protocol().Code {
	case ma.P_IP4, ma.P_IP6:
		return first.Value()
	}
	return ""
}

// servePair handles the only conversation a stranger may have with us.
func (n *Node) servePair(s network.Stream) {
	defer s.Close()
	if !n.pairingActive() {
		_ = s.Reset()
		return
	}
	remote := s.Conn().RemotePeer()
	// One pairing exchange at a time per device: parallel streams would only
	// multiply memory use and token guesses.
	n.mu.Lock()
	if n.pairing[remote] {
		n.mu.Unlock()
		_ = s.Reset()
		return
	}
	n.pairing[remote] = true
	n.mu.Unlock()
	defer func() {
		n.mu.Lock()
		delete(n.pairing, remote)
		n.mu.Unlock()
	}()
	n.wg.Add(1)
	defer n.wg.Done()
	_ = s.SetDeadline(time.Now().Add(15 * time.Second))
	c := protocol.NewConn(s)
	t, hb, _, err := c.RecvMax(4096) // a request is a few dozen bytes
	var req protocol.PairRequest
	if err != nil || t != protocol.TypePair || json.Unmarshal(hb, &req) != nil {
		_ = s.Reset()
		return
	}
	conn := s.Conn()
	if !n.isKnown(remote) {
		// This exchange is all a stranger is allowed: once the reply is out, free
		// its place and drop the connection (after the guest had time to read).
		defer func() {
			n.releaseStranger(conn)
			time.AfterFunc(time.Second, func() { _ = conn.Close() })
		}()
	}
	resp, commit := n.opts.Pairing.Accept(remote, remoteIP(conn), req)
	if c.Send(protocol.TypePair, resp, nil) != nil {
		return
	}
	if resp.OK && commit != nil {
		// Let the guest read the reply before the configuration change restarts us.
		time.AfterFunc(500*time.Millisecond, commit)
	}
}
