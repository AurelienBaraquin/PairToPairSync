package node

import (
	"context"
	"testing"
	"time"

	"github.com/libp2p/go-libp2p"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
)

// The DHT answers anyone who connects. Wrapped in gatedHost it must answer only
// configured devices, so a stranger let in for pairing learns nothing from it.
func TestGatedHostAnswersOnlyAllowedPeers(t *testing.T) {
	newHost := func() host.Host {
		h, err := libp2p.New(libp2p.ListenAddrStrings("/ip4/127.0.0.1/tcp/0"), libp2p.DisableRelay())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { h.Close() })
		return h
	}
	server, friend, stranger := newHost(), newHost(), newHost()
	called := make(chan peer.ID, 4)
	g := gatedHost{Host: server, allow: func(p peer.ID) bool { return p == friend.ID() }}
	g.SetStreamHandler("/test/dht", func(s network.Stream) { called <- s.Conn().RemotePeer(); s.Close() })

	try := func(c host.Host) error {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := c.Connect(ctx, peer.AddrInfo{ID: server.ID(), Addrs: server.Addrs()}); err != nil {
			return err
		}
		s, err := c.NewStream(ctx, server.ID(), "/test/dht")
		if err != nil {
			return err
		}
		defer s.Close()
		_, _ = s.Write([]byte("x"))
		buf := make([]byte, 1)
		_ = s.SetReadDeadline(time.Now().Add(time.Second))
		_, rerr := s.Read(buf) // reset by the gate, or EOF from the handler's Close
		_ = rerr
		return nil
	}
	if err := try(stranger); err != nil {
		t.Fatal(err)
	}
	select {
	case p := <-called:
		t.Fatalf("handler ran for %s, a stranger", p)
	case <-time.After(500 * time.Millisecond):
	}
	if err := try(friend); err != nil {
		t.Fatal(err)
	}
	select {
	case p := <-called:
		if p != friend.ID() {
			t.Fatalf("unexpected caller %s", p)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("handler never ran for the allowed peer")
	}
}
