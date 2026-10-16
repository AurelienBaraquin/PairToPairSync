package pairing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/libp2p/go-libp2p"
	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	ma "github.com/multiformats/go-multiaddr"

	"github.com/aurelienbaraquin/pairtopairsync/internal/protocol"
)

// Dial presents an invitation to its issuer and returns the reply. It uses a
// short-lived, listen-less libp2p host with the guest's own identity key, so
// the inviter learns (and trusts, from now on) the guest's real device ID, while
// the guest only talks to the exact device ID printed in the invitation: nobody
// can impersonate the inviter without its private key.
func Dial(ctx context.Context, key crypto.PrivKey, inv Invite, req protocol.PairRequest) (protocol.PairResponse, error) {
	pid, err := peer.Decode(inv.ID)
	if err != nil {
		return protocol.PairResponse{}, err
	}
	var addrs []ma.Multiaddr
	for _, a := range inv.Addrs {
		if m, err := ma.NewMultiaddr(a); err == nil {
			addrs = append(addrs, m)
		}
	}
	if len(addrs) == 0 {
		return protocol.PairResponse{}, errors.New("the invitation carries no usable address")
	}
	req.Token = inv.Token

	h, err := libp2p.New(libp2p.Identity(key), libp2p.NoListenAddrs, libp2p.EnableRelay())
	if err != nil {
		return protocol.PairResponse{}, err
	}
	defer func() {
		done := make(chan struct{})
		go func() { _ = h.Close(); close(done) }()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
		}
	}()

	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := h.Connect(cctx, peer.AddrInfo{ID: pid, Addrs: addrs}); err != nil {
		return protocol.PairResponse{}, fmt.Errorf("cannot reach %s (is the daemon running and the invitation still valid?): %w", name(inv), err)
	}
	s, err := h.NewStream(network.WithAllowLimitedConn(cctx, "pair"), pid, protocol.PairProtocol)
	if err != nil {
		return protocol.PairResponse{}, fmt.Errorf("the other device refused to pair (invitation expired or already used?): %w", err)
	}
	defer s.Close()
	wait := 20 * time.Second
	if inv.Approve {
		wait = MaxApprovalWait + 20*time.Second // a person has to say yes
	}
	_ = s.SetDeadline(time.Now().Add(wait))
	c := protocol.NewConn(s)
	if err := c.Send(protocol.TypePair, req, nil); err != nil {
		return protocol.PairResponse{}, err
	}
	t, hb, _, err := c.Recv()
	if err != nil || t != protocol.TypePair {
		return protocol.PairResponse{}, errors.New("no answer from the other device (invitation expired or already used?)")
	}
	var resp protocol.PairResponse
	if err := json.Unmarshal(hb, &resp); err != nil {
		return protocol.PairResponse{}, err
	}
	if !resp.OK {
		return resp, fmt.Errorf("pairing refused: %s", resp.Err)
	}
	return resp, nil
}

func name(inv Invite) string {
	if inv.Name != "" {
		return inv.Name
	}
	return "the other device"
}
