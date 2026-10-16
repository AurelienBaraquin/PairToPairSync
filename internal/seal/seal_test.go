package seal

import (
	"bytes"
	"testing"

	"github.com/aurelienbaraquin/pairtopairsync/internal/model"
)

func newSealer(t *testing.T) *Sealer {
	t.Helper()
	sec, err := NewSecret()
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(sec)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestBlockRoundTripAndDeterminism(t *testing.T) {
	s := newSealer(t)
	plain := []byte("some file content")
	h := model.HashBytes(plain)
	c1, c2 := s.SealBlock(plain, h), s.SealBlock(plain, h)
	if !bytes.Equal(c1, c2) {
		t.Fatal("block sealing must be deterministic so relays can de-duplicate")
	}
	if bytes.Contains(c1, plain) {
		t.Fatal("ciphertext contains plaintext")
	}
	if len(c1) != len(plain)+16 {
		t.Fatalf("unexpected overhead: %d", len(c1)-len(plain))
	}
	got, err := s.OpenBlock(c1, h)
	if err != nil || !bytes.Equal(got, plain) {
		t.Fatalf("round trip failed: %v", err)
	}
}

func TestTamperingAndWrongKeyAreDetected(t *testing.T) {
	s, other := newSealer(t), newSealer(t)
	plain := []byte("payload")
	h := model.HashBytes(plain)
	c := s.SealBlock(plain, h)

	bad := append([]byte{}, c...)
	bad[0] ^= 1
	if _, err := s.OpenBlock(bad, h); err == nil {
		t.Fatal("modified ciphertext accepted")
	}
	if _, err := other.OpenBlock(c, h); err == nil {
		t.Fatal("block opened with another folder's secret")
	}
	// A relay swapping in a different (valid) block for this hash must fail.
	p2 := []byte("different")
	if _, err := s.OpenBlock(s.SealBlock(p2, model.HashBytes(p2)), h); err == nil {
		t.Fatal("substituted block accepted")
	}
}

func TestMetadataIsBoundToPathAndVersion(t *testing.T) {
	s := newSealer(t)
	meta := []byte(`{"name":"secret/plan.txt"}`)
	id := s.PathID("secret/plan.txt")
	sealed, err := s.SealMeta(meta, id, "A:3")
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(sealed, []byte("plan.txt")) {
		t.Fatal("metadata not encrypted")
	}
	if got, err := s.OpenMeta(sealed, id, "A:3"); err != nil || !bytes.Equal(got, meta) {
		t.Fatalf("open failed: %v", err)
	}
	if _, err := s.OpenMeta(sealed, id, "A:4"); err == nil {
		t.Fatal("relay could replay metadata under another version")
	}
	if _, err := s.OpenMeta(sealed, s.PathID("other.txt"), "A:3"); err == nil {
		t.Fatal("relay could move metadata to another path")
	}
}

func TestPathIDsAreOpaqueAndKeyed(t *testing.T) {
	a, b := newSealer(t), newSealer(t)
	if a.PathID("x") == a.PathID("y") || a.PathID("x") != a.PathID("x") {
		t.Fatal("path id must be stable and distinct per name")
	}
	if a.PathID("x") == b.PathID("x") {
		t.Fatal("path ids must depend on the secret (no dictionary attack from a relay)")
	}
}

func TestSecretValidation(t *testing.T) {
	if _, err := New("short"); err == nil {
		t.Fatal("invalid secret accepted")
	}
}
