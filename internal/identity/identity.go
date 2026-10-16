// Package identity manages the cryptographic identity of a device.
//
// A device owns an Ed25519 key pair. Its device ID is the libp2p peer ID derived
// from the public key, so the ID *is* the public key: when a connection is
// secured (QUIC/TLS 1.3 or Noise) the remote end has proven possession of the
// matching private key. There is no certificate authority and no server.
package identity

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/peer"
	ma "github.com/multiformats/go-multiaddr"
)

const keyFile = "identity.key"

type Identity struct {
	Key    crypto.PrivKey
	PeerID peer.ID
	ID     string // canonical device ID (== PeerID.String())
}

var keyMu sync.Mutex // the UI polls the ID while the first start creates the key

// LoadOrCreate reads the private key stored in dir, generating it on first use.
// The key file is written atomically: a crash can never leave a truncated key.
func LoadOrCreate(dir string) (*Identity, error) {
	keyMu.Lock()
	defer keyMu.Unlock()
	path := filepath.Join(dir, keyFile)
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		priv, _, gerr := crypto.GenerateEd25519Key(nil)
		if gerr != nil {
			return nil, gerr
		}
		if raw, err = crypto.MarshalPrivateKey(priv); err != nil {
			return nil, err
		}
		if err = os.MkdirAll(dir, 0o700); err != nil {
			return nil, err
		}
		tmp := path + ".tmp"
		if err = os.WriteFile(tmp, raw, 0o600); err != nil {
			return nil, err
		}
		if err = os.Rename(tmp, path); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}
	priv, err := crypto.UnmarshalPrivateKey(raw)
	if err != nil {
		return nil, fmt.Errorf("load identity: %w", err)
	}
	pid, err := peer.IDFromPrivateKey(priv)
	if err != nil {
		return nil, err
	}
	return &Identity{Key: priv, PeerID: pid, ID: pid.String()}, nil
}

// Normalize validates a device ID and returns its canonical form.
func Normalize(id string) (string, error) {
	pid, err := peer.Decode(strings.TrimSpace(id))
	if err != nil {
		return "", fmt.Errorf("invalid device ID %q", id)
	}
	// A real ID embeds an Ed25519 public key: a truncated or mistyped string that
	// merely parses as a hash is not a device.
	pub, err := pid.ExtractPublicKey()
	if err != nil || pub.Type() != crypto.Ed25519 {
		return "", fmt.Errorf("invalid device ID %q", id)
	}
	return pid.String(), nil
}

// Decode converts a canonical device ID to a peer ID.
func Decode(id string) (peer.ID, error) { return peer.Decode(id) }

// Format is the display form of an ID (identical to the canonical form).
func Format(id string) string { return id }

// Short is the compact, collision-resistant form used as a key in version vectors.
func Short(id string) string {
	sum := sha256.Sum256([]byte(id))
	return hex.EncodeToString(sum[:6])
}

// ParseRef accepts what a user can paste to designate a device: a bare device
// ID, or a full address such as /ip4/203.0.113.7/udp/22000/quic-v1/p2p/<ID>
// (which also carries a way to reach it). addr is empty for a bare ID.
func ParseRef(s string) (id string, addr string, err error) {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "/") {
		id, err = Normalize(s)
		return id, "", err
	}
	m, err := ma.NewMultiaddr(s)
	if err != nil {
		return "", "", err
	}
	info, err := peer.AddrInfoFromP2pAddr(m)
	if err != nil {
		return "", "", fmt.Errorf("address must end with /p2p/<device ID>: %w", err)
	}
	if len(info.Addrs) > 0 {
		addr = info.Addrs[0].String()
	}
	return info.ID.String(), addr, nil
}
