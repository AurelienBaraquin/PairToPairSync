package node

import (
	"net"
	"slices"
	"strings"

	"github.com/aurelienbaraquin/pairtopairsync/internal/protocol"
)

// AllowLoopbackHints lets tests, whose devices all live on 127.0.0.1, exchange hints.
var AllowLoopbackHints bool

// CleanHints keeps the addresses worth trying for another device: literal public or
// local-network IP addresses with a port, never names (which would make this device
// query arbitrary resolvers), loopback or multicast. Hints are untrusted: a wrong one
// only wastes a connection attempt, since whoever answers is identified by its key.
func CleanHints(in []string) []string {
	var out []string
	for _, a := range in {
		if len(a) > 200 {
			continue
		}
		var ip net.IP
		if strings.HasPrefix(a, "/") {
			parts := strings.Split(a, "/")
			if len(parts) < 3 || (parts[1] != "ip4" && parts[1] != "ip6") {
				continue
			}
			ip = net.ParseIP(parts[2])
		} else if host, _, err := net.SplitHostPort(a); err == nil {
			ip = net.ParseIP(host)
		}
		if ip == nil || (ip.IsLoopback() && !AllowLoopbackHints) || ip.IsUnspecified() || ip.IsMulticast() || ip.IsLinkLocalUnicast() {
			continue
		}
		if !slices.Contains(out, a) {
			out = append(out, a)
		}
		if len(out) >= protocol.MaxHintAddrs {
			break
		}
	}
	return out
}
