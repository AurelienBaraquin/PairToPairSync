package node

import (
	"testing"

	"github.com/aurelienbaraquin/pairtopairsync/internal/config"
	ma "github.com/multiformats/go-multiaddr"
)

func TestWithLocalIPs(t *testing.T) {
	n := &Node{cfg: &config.Config{ListenPort: 22100}}
	n.opts.LocalIPs = func() []string {
		return []string{"192.168.1.20", "192.168.1.20", "127.0.0.1", "0.0.0.0", "fe80::1", "fd00::5", "junk", ""}
	}
	have, _ := ma.NewMultiaddr("/ip4/192.168.1.20/tcp/22100")
	out := n.withLocalIPs([]ma.Multiaddr{have})
	want := map[string]int{
		"/ip4/192.168.1.20/tcp/22100":         1, // no duplicate of what the host knew
		"/ip4/192.168.1.20/udp/22100/quic-v1": 1,
		"/ip6/fd00::5/tcp/22100":              1,
		"/ip6/fd00::5/udp/22100/quic-v1":      1,
	}
	got := map[string]int{}
	for _, a := range out {
		got[a.String()]++
	}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s: %d, want %d (all: %v)", k, got[k], v, got)
		}
	}
}
