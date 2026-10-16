// Package discovery finds other devices on the local network with UDP
// broadcasts, so that devices on the same Wi-Fi connect without configuring
// addresses. Announcements carry only the device ID and port; trust still
// comes from the TLS identity check.
package discovery

import (
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	Port     = 21027
	magic    = "P2PS1"
	interval = 30 * time.Second
	ttl      = 3 * interval
)

type found struct {
	addr string
	seen time.Time
}

type Discovery struct {
	self string
	port int
	log  *slog.Logger
	conn *net.UDPConn
	stop chan struct{}
	wg   sync.WaitGroup

	mu    sync.Mutex
	known map[string]found
}

// Start announces selfID (reachable on tcpPort) and listens for others.
func Start(selfID string, tcpPort int, log *slog.Logger) (*Discovery, error) {
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{Port: Port})
	if err != nil {
		return nil, err
	}
	d := &Discovery{self: selfID, port: tcpPort, log: log, conn: conn, stop: make(chan struct{}), known: map[string]found{}}
	d.wg.Add(2)
	go func() { defer d.wg.Done(); d.listen() }()
	go func() { defer d.wg.Done(); d.announce() }()
	return d, nil
}

func (d *Discovery) Stop() {
	close(d.stop)
	d.conn.Close()
	d.wg.Wait()
}

// Addresses implements node.AddressSource.
func (d *Discovery) Addresses(id string) []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	if f, ok := d.known[id]; ok && time.Since(f.seen) < ttl {
		return []string{f.addr}
	}
	return nil
}

func (d *Discovery) announce() {
	msg := []byte(fmt.Sprintf("%s %s %d", magic, d.self, d.port))
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		for _, dst := range broadcastTargets() {
			_, _ = d.conn.WriteToUDP(msg, dst)
		}
		select {
		case <-t.C:
		case <-d.stop:
			return
		}
	}
}

func broadcastTargets() []*net.UDPAddr {
	out := []*net.UDPAddr{{IP: net.IPv4bcast, Port: Port}}
	ifaces, _ := net.Interfaces()
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagBroadcast == 0 {
			continue
		}
		addrs, _ := ifc.Addrs()
		for _, a := range addrs {
			ipn, ok := a.(*net.IPNet)
			if !ok || ipn.IP.To4() == nil {
				continue
			}
			ip := ipn.IP.To4()
			b := make(net.IP, 4)
			for i := range b {
				b[i] = ip[i] | ^ipn.Mask[len(ipn.Mask)-4+i]
			}
			out = append(out, &net.UDPAddr{IP: b, Port: Port})
		}
	}
	return out
}

func (d *Discovery) listen() {
	buf := make([]byte, 512)
	for {
		n, from, err := d.conn.ReadFromUDP(buf)
		if err != nil {
			select {
			case <-d.stop:
				return
			default:
				time.Sleep(time.Second)
				continue
			}
		}
		parts := strings.Fields(string(buf[:n]))
		if len(parts) != 3 || parts[0] != magic || parts[1] == d.self {
			continue
		}
		port, err := strconv.Atoi(parts[2])
		if err != nil || port <= 0 || port > 65535 {
			continue
		}
		d.mu.Lock()
		d.known[parts[1]] = found{addr: net.JoinHostPort(from.IP.String(), strconv.Itoa(port)), seen: time.Now()}
		d.mu.Unlock()
	}
}
