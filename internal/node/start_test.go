package node

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"testing"
	"time"

	"github.com/aurelienbaraquin/pairtopairsync/internal/config"
	"github.com/aurelienbaraquin/pairtopairsync/internal/identity"
	"github.com/aurelienbaraquin/pairtopairsync/internal/protocol"
)

// probeHandler checks, while it is being started, whether the node already accepts
// connections on its port.
type probeHandler struct {
	addr      string
	reachable chan bool
}

func (h *probeHandler) ID() string { return "probe" }
func (h *probeHandler) Start(context.Context) error {
	c, err := net.DialTimeout("tcp", h.addr, time.Second)
	if err == nil {
		c.Close()
	}
	h.reachable <- err == nil
	return nil
}
func (h *probeHandler) Stop()                                                  {}
func (h *probeHandler) PeerUp(*Peer)                                           {}
func (h *probeHandler) PeerDown(*Peer)                                         {}
func (h *probeHandler) HandleIndex(*Peer, protocol.Index) bool                 { return true }
func (h *probeHandler) HandleIndexAck(*Peer, protocol.IndexAck)                {}
func (h *probeHandler) HandleRequest(*Peer, protocol.Request) ([]byte, string) { return nil, "" }
func (h *probeHandler) HandleProbe(*Peer, []protocol.ProbeItem) []bool         { return nil }
func (h *probeHandler) Status() any                                            { return nil }

// A device that connects while the node starts (a phone right after it was paired,
// when the daemon reloads) is handed to every folder at once: the folders must be
// running before the node accepts anyone, or they are used half-built.
func TestFoldersStartBeforeTheNodeAcceptsConnections(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()

	id, err := identity.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{ListenPort: port, APIAddr: "-",
		ListenAddresses: []string{fmt.Sprintf("/ip4/127.0.0.1/tcp/%d", port)}}
	h := &probeHandler{addr: l.Addr().String(), reachable: make(chan bool, 1)}
	n := New(Options{Config: cfg, Identity: id, Handlers: []FolderHandler{h},
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Reachability: "private"})
	if err := n.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer n.Stop()

	if <-h.reachable {
		t.Fatal("the node accepted connections before its folders were started")
	}
}
