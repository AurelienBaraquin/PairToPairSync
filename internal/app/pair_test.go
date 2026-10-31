package app

import (
	"slices"
	"testing"

	"github.com/aurelienbaraquin/pairtopairsync/internal/pairing"
)

func TestSelectAddrs(t *testing.T) {
	const self = "12D3KooWSelf"
	quic := func(ip string) string { return "/ip4/" + ip + "/udp/22100/quic-v1" }
	withID := func(addrs ...string) []string {
		out := make([]string, len(addrs))
		for i, a := range addrs {
			out[i] = a + "/p2p/" + self
		}
		return out
	}
	circuit := "/ip4/203.0.113.9/udp/22100/quic-v1/p2p/12D3KooWRelay/p2p-circuit"

	tests := []struct {
		name string
		in   []string
		want []string
	}{
		{
			// A Windows PC with two VPNs, WSL and VirtualBox: the Wi-Fi address must not be
			// dropped because a VPN address happens to sort first.
			name: "every local address, home range first",
			in: withID(quic("10.100.0.2"), quic("10.5.0.2"), quic("127.0.0.1"), quic("169.254.6.62"),
				quic("172.21.64.1"), quic("192.168.1.45"), quic("192.168.56.1"),
				"/ip4/192.168.1.45/tcp/22100"),
			want: []string{quic("192.168.1.45"), quic("192.168.56.1"), quic("10.100.0.2"), quic("10.5.0.2"),
				quic("172.21.64.1")},
		},
		{
			name: "public, local and relayed",
			in:   append(withID(quic("192.168.1.45"), quic("198.51.100.7"), quic("198.51.100.8")), circuit),
			want: []string{quic("198.51.100.7"), quic("192.168.1.45"), circuit},
		},
		{
			name: "loopback only as a last resort",
			in:   withID(quic("127.0.0.1")),
			want: []string{quic("127.0.0.1")},
		},
		{
			name: "TCP when there is no QUIC address",
			in:   withID("/ip4/192.168.1.45/tcp/22100"),
			want: []string{"/ip4/192.168.1.45/tcp/22100"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := SelectAddrs(tt.in, self); !slices.Equal(got, tt.want) {
				t.Errorf("SelectAddrs() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestSelectAddrsFitsAnInvitation(t *testing.T) {
	var in []string
	for i := range 12 {
		in = append(in, "/ip4/10.0.0."+string(rune('1'+i%9))+"/udp/2210"+string(rune('0'+i%10))+"/quic-v1")
	}
	in = append(in, "/ip4/198.51.100.7/udp/22100/quic-v1",
		"/ip4/203.0.113.9/udp/22100/quic-v1/p2p/12D3KooWRelay/p2p-circuit")

	got := SelectAddrs(in, "12D3KooWSelf")
	if len(got) != pairing.MaxAddrs {
		t.Fatalf("got %d addresses, want %d: %v", len(got), pairing.MaxAddrs, got)
	}
	if !slices.Contains(got, in[len(in)-2]) || got[len(got)-1] != in[len(in)-1] {
		t.Errorf("the public and the relayed address must survive the cap: %v", got)
	}
}
