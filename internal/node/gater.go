package node

import (
	"github.com/libp2p/go-libp2p/core/control"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	ma "github.com/multiformats/go-multiaddr"
)

// gater is the first line of defence: only devices present in the configuration
// may connect, in either direction. Anyone else is dropped during the secure
// handshake, so they cannot even open a protocol stream.
type gater struct {
	allowed func(peer.ID) bool
	// pairing reports whether an invitation is outstanding. Only then are
	// unknown devices let in, and they can do nothing but present a token.
	pairing func() bool
	banned  func(peer.ID) bool
}

func (g *gater) InterceptPeerDial(p peer.ID) bool { return g.allowed(p) }

func (g *gater) InterceptAddrDial(p peer.ID, _ ma.Multiaddr) bool { return g.allowed(p) }

// Raw inbound connections cannot be judged before the handshake reveals who is
// calling; InterceptSecured does it.
func (g *gater) InterceptAccept(network.ConnMultiaddrs) bool { return true }

func (g *gater) InterceptSecured(dir network.Direction, p peer.ID, _ network.ConnMultiaddrs) bool {
	if g.allowed(p) {
		return true
	}
	return dir == network.DirInbound && g.pairing != nil && g.pairing() && !(g.banned != nil && g.banned(p))
}

func (g *gater) InterceptUpgraded(network.Conn) (bool, control.DisconnectReason) { return true, 0 }
