// Package identity manages the cryptographic identity of a device. A device is
// identified by the SHA-256 of its self-signed certificate; peers authenticate
// each other by pinning that value (mutual TLS 1.3, no certificate authority).
package identity

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base32"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ALPN protocol identifier.
const alpn = "p2psync/1"

type Identity struct {
	Cert tls.Certificate
	ID   string // canonical device ID (no dashes)
}

var b32 = base32.StdEncoding.WithPadding(base32.NoPadding)

// IDFromCertDER computes the canonical device ID of a DER encoded certificate.
func IDFromCertDER(der []byte) string {
	sum := sha256.Sum256(der)
	return b32.EncodeToString(sum[:])
}

// Normalize converts a user supplied ID (dashes, spaces, lowercase) to the
// canonical form. It returns an error if the result is not a valid ID.
func Normalize(id string) (string, error) {
	id = strings.ToUpper(strings.NewReplacer("-", "", " ", "").Replace(strings.TrimSpace(id)))
	raw, err := b32.DecodeString(id)
	if err != nil || len(raw) != sha256.Size {
		return "", fmt.Errorf("invalid device ID %q", id)
	}
	return id, nil
}

// Format renders an ID in groups of seven characters for display.
func Format(id string) string {
	var parts []string
	for len(id) > 7 {
		parts = append(parts, id[:7])
		id = id[7:]
	}
	parts = append(parts, id)
	return strings.Join(parts, "-")
}

// Short is the compact form used as a key in version vectors.
func Short(id string) string {
	if len(id) < 8 {
		return id
	}
	return id[:8]
}

// LoadOrCreate reads the identity stored in dir, generating it on first use.
func LoadOrCreate(dir string) (*Identity, error) {
	certPath, keyPath := filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	if _, err := os.Stat(certPath); errors.Is(err, os.ErrNotExist) {
		if err := generate(dir, certPath, keyPath); err != nil {
			return nil, err
		}
	}
	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return nil, fmt.Errorf("load identity: %w", err)
	}
	return &Identity{Cert: cert, ID: IDFromCertDER(cert.Certificate[0])}, nil
}

func generate(dir, certPath, keyPath string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	tpl := &x509.Certificate{
		SerialNumber:          big.NewInt(time.Now().UnixNano()),
		Subject:               pkix.Name{CommonName: "p2psync"},
		NotBefore:             time.Now().Add(-24 * time.Hour),
		NotAfter:              time.Now().AddDate(30, 0, 0),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, pub, priv)
	if err != nil {
		return err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return err
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		return err
	}
	return os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644)
}

func (i *Identity) base() *tls.Config {
	return &tls.Config{
		Certificates: []tls.Certificate{i.Cert},
		MinVersion:   tls.VersionTLS13,
		NextProtos:   []string{alpn},
		// Trust is established by pinning the device ID after the handshake,
		// not by a certificate chain.
		InsecureSkipVerify: true,
	}
}

// ServerConfig is used for accepted connections; the client must present a certificate.
func (i *Identity) ServerConfig() *tls.Config {
	c := i.base()
	c.ClientAuth = tls.RequireAnyClientCert
	return c
}

// ClientConfig is used for outgoing connections.
func (i *Identity) ClientConfig() *tls.Config { return i.base() }

// PeerID returns the device ID of the remote end of an established TLS connection.
func PeerID(c *tls.Conn) (string, error) {
	certs := c.ConnectionState().PeerCertificates
	if len(certs) == 0 {
		return "", errors.New("peer presented no certificate")
	}
	return IDFromCertDER(certs[0].Raw), nil
}
