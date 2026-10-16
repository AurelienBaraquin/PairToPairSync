// Package seal implements the end-to-end encryption that keeps relays blind.
//
// All full devices of a folder share a 256-bit secret. A relay never gets it, so
// everything stored on a relay (a phone that can be lost or stolen) is
// ciphertext it cannot read:
//
//   - file names  -> opaque identifiers, HMAC-SHA256(name) under a derived key;
//   - metadata    -> XChaCha20-Poly1305 with a random nonce, bound to the
//     identifier and the version vector (a relay cannot splice them);
//   - data blocks -> XChaCha20-Poly1305 with a nonce derived from the block's own
//     hash. Encryption is therefore deterministic: whichever device seals a block
//     produces the same bytes, so the relay can verify and de-duplicate by the
//     hash of the ciphertext. What the relay can learn: file sizes, timing, the
//     version vectors, and that two blocks of the folder are identical.
package seal

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"

	"golang.org/x/crypto/chacha20poly1305"
	"golang.org/x/crypto/hkdf"
)

const secretLen = 32

// NewSecret generates a fresh folder secret in its textual form.
func NewSecret() (string, error) {
	b := make([]byte, secretLen)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawStdEncoding.EncodeToString(b), nil
}

// ParseSecret validates the textual form of a secret.
func ParseSecret(s string) ([]byte, error) {
	b, err := base64.RawStdEncoding.DecodeString(s)
	if err != nil || len(b) != secretLen {
		return nil, errors.New("invalid folder secret (expected 32 bytes, base64)")
	}
	return b, nil
}

// Sealer holds the keys derived from a folder secret.
type Sealer struct {
	nameKey, metaKey, blockKey []byte
	meta                       cipherAEAD
	block                      cipherAEAD
}

type cipherAEAD interface {
	Seal(dst, nonce, plaintext, additionalData []byte) []byte
	Open(dst, nonce, ciphertext, additionalData []byte) ([]byte, error)
	NonceSize() int
}

func derive(secret []byte, label string) []byte {
	out := make([]byte, 32)
	if _, err := io.ReadFull(hkdf.New(sha256.New, secret, nil, []byte("p2psync/"+label)), out); err != nil {
		panic(err)
	}
	return out
}

// New derives the working keys from the textual secret.
func New(secret string) (*Sealer, error) {
	raw, err := ParseSecret(secret)
	if err != nil {
		return nil, err
	}
	s := &Sealer{nameKey: derive(raw, "name"), metaKey: derive(raw, "meta"), blockKey: derive(raw, "block")}
	if s.meta, err = chacha20poly1305.NewX(s.metaKey); err != nil {
		return nil, err
	}
	if s.block, err = chacha20poly1305.NewX(s.blockKey); err != nil {
		return nil, err
	}
	return s, nil
}

// PathID is the opaque identifier a relay uses instead of a file name.
func (s *Sealer) PathID(name string) string {
	m := hmac.New(sha256.New, s.nameKey)
	m.Write([]byte(name))
	return hex.EncodeToString(m.Sum(nil)[:16])
}

// SealMeta encrypts serialized metadata, bound to its public context.
func (s *Sealer) SealMeta(plain []byte, pathID, version string) ([]byte, error) {
	nonce := make([]byte, s.meta.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return s.meta.Seal(nonce, nonce, plain, aad(pathID, version)), nil
}

// OpenMeta decrypts metadata; it fails if it was altered or moved to another path/version.
func (s *Sealer) OpenMeta(sealed []byte, pathID, version string) ([]byte, error) {
	ns := s.meta.NonceSize()
	if len(sealed) < ns {
		return nil, errors.New("sealed metadata too short")
	}
	return s.meta.Open(nil, sealed[:ns], sealed[ns:], aad(pathID, version))
}

func aad(pathID, version string) []byte { return []byte(pathID + "|" + version) }

func (s *Sealer) blockNonce(plainHash []byte) []byte {
	m := hmac.New(sha256.New, s.blockKey)
	m.Write(plainHash)
	return m.Sum(nil)[:s.block.NonceSize()]
}

// SealBlock encrypts a data block deterministically (see package comment).
func (s *Sealer) SealBlock(plain, plainHash []byte) []byte {
	return s.block.Seal(nil, s.blockNonce(plainHash), plain, plainHash)
}

// OpenBlock decrypts a block whose plaintext hash is known from the metadata and
// checks that the result really has that hash.
func (s *Sealer) OpenBlock(cipher, plainHash []byte) ([]byte, error) {
	p, err := s.block.Open(nil, s.blockNonce(plainHash), cipher, plainHash)
	if err != nil {
		return nil, fmt.Errorf("block authentication failed: %w", err)
	}
	h := sha256.Sum256(p)
	if !hmac.Equal(h[:], plainHash) {
		return nil, errors.New("block does not match its hash")
	}
	return p, nil
}
