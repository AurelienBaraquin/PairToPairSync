package grant

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/peer"
)

type dev struct {
	key crypto.PrivKey
	id  string
}

func newDev(t *testing.T) dev {
	t.Helper()
	k, _, err := crypto.GenerateEd25519Key(nil)
	if err != nil {
		t.Fatal(err)
	}
	pid, _ := peer.IDFromPrivateKey(k)
	return dev{k, pid.String()}
}

var now = time.Unix(1_800_000_000, 0)

func issue(t *testing.T, by dev, sub dev, admin bool, seq int64, folders ...Share) Signed {
	t.Helper()
	sg, err := Sign(by.key, Grant{Subject: sub.id, Admin: admin, Folders: folders, Seq: seq}, now)
	if err != nil {
		t.Fatal(err)
	}
	return sg
}

func pin(ids ...dev) map[string]bool {
	m := map[string]bool{}
	for _, d := range ids {
		m[d.id] = true
	}
	return m
}

func TestSignVerifyAndTamper(t *testing.T) {
	a, n := newDev(t), newDev(t)
	sg := issue(t, a, n, false, 5, Share{ID: "docs"})
	g, err := Verify(sg)
	if err != nil || g.Subject != n.id || g.Issuer != a.id {
		t.Fatalf("verify: %v %+v", err, g)
	}
	// Any change to the body or the signature is detected.
	bad := Signed{Body: append([]byte(nil), sg.Body...), Sig: sg.Sig}
	bad.Body[len(bad.Body)/2] ^= 1
	if _, err := Verify(bad); err == nil {
		t.Fatal("tampered body accepted")
	}
	bad = Signed{Body: sg.Body, Sig: append([]byte(nil), sg.Sig...)}
	bad.Sig[0] ^= 1
	if _, err := Verify(bad); err == nil {
		t.Fatal("tampered signature accepted")
	}
	// A signature by someone else, under the admin's name, is refused.
	other := newDev(t)
	forged := Signed{Body: sg.Body}
	forged.Sig, _ = other.key.Sign(payload(sg.Body))
	if _, err := Verify(forged); err == nil {
		t.Fatal("grant signed by another key accepted")
	}
	// The signature is bound to this use: the same bytes signed without the domain prefix do not verify.
	raw := Signed{Body: sg.Body}
	raw.Sig, _ = a.key.Sign(sg.Body)
	if _, err := Verify(raw); err == nil {
		t.Fatal("signature without domain separation accepted")
	}
}

func TestMalformedGrantsRefused(t *testing.T) {
	a, n := newDev(t), newDev(t)
	cases := map[string]Grant{
		"bad folder":       {Subject: n.id, Folders: []Share{{ID: "../x"}}},
		"duplicate":        {Subject: n.id, Folders: []Share{{ID: "a"}, {ID: "a"}}},
		"bad subject":      {Subject: "nope"},
		"revoked + rights": {Subject: n.id, Revoked: true, Folders: []Share{{ID: "a"}}},
	}
	for name, g := range cases {
		if _, err := Sign(a.key, g, now); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := Verify(Signed{Body: []byte("{}"), Sig: []byte{1}}); err == nil {
		t.Error("empty grant accepted")
	}
	if _, err := Verify(Signed{Body: make([]byte, maxBody+1)}); err == nil {
		t.Error("oversized grant accepted")
	}
}

func TestOnlyTrustedIssuersAndNewestWins(t *testing.T) {
	a, b, n, stranger := newDev(t), newDev(t), newDev(t), newDev(t)
	s := Load(filepath.Join(t.TempDir(), "g.json"))
	trust := pin(a)

	if _, _, err := s.Offer(issue(t, stranger, n, false, 1, Share{ID: "docs"}), trust, now); err != ErrUntrusted {
		t.Fatalf("grant by an unknown device: %v", err)
	}
	if _, ch, err := s.Offer(issue(t, a, n, false, 10, Share{ID: "docs"}), trust, now); err != nil || !ch {
		t.Fatalf("first grant: %v %v", err, ch)
	}
	// An older grant (replay of a revoked right) does not come back.
	if _, ch, _ := s.Offer(issue(t, a, n, false, 9, Share{ID: "docs"}, Share{ID: "secret"}), trust, now); ch {
		t.Fatal("older grant replaced a newer one")
	}
	// Same grant again: nothing changes.
	if _, ch, _ := s.Offer(issue(t, a, n, false, 10, Share{ID: "docs"}), trust, now); ch {
		t.Fatal("identical grant reported as a change")
	}
	// A revocation is just a newer grant.
	rev, _ := Sign(a.key, Grant{Subject: n.id, Revoked: true, Seq: 11}, now)
	if _, ch, err := s.Offer(rev, trust, now); err != nil || !ch {
		t.Fatalf("revocation: %v %v", err, ch)
	}
	if g, _ := s.Get(n.id); !g.Revoked {
		t.Fatal("revocation not applied")
	}
	// ... and the revoked grant cannot be replayed.
	if _, ch, _ := s.Offer(issue(t, a, n, false, 10, Share{ID: "docs"}), trust, now); ch {
		t.Fatal("revoked grant came back")
	}
	_ = b
}

func TestAdminsCanPromoteButOnlyFromATrustedRoot(t *testing.T) {
	root, second, third, outsider, n := newDev(t), newDev(t), newDev(t), newDev(t), newDev(t)
	s := Load(filepath.Join(t.TempDir(), "g.json"))
	trust := pin(root)

	// The root promotes `second`; `second` may then vouch for devices and promote `third`.
	for _, sg := range []Signed{
		issue(t, root, second, true, 1, Share{ID: "docs"}),
		issue(t, second, third, true, 2, Share{ID: "docs"}),
		issue(t, third, n, false, 3, Share{ID: "docs"}),
	} {
		if _, _, err := s.Offer(sg, trust, now); err != nil {
			t.Fatalf("chain link refused: %v", err)
		}
	}
	got := s.Trusted(trust)
	for _, d := range []dev{root, second, third} {
		if !got[d.id] {
			t.Errorf("%s should be trusted", d.id[:8])
		}
	}
	if got[outsider.id] || got[n.id] {
		t.Error("untrusted device counted as admin")
	}
	// A promotion from a device that is not trusted does nothing.
	if _, _, err := s.Offer(issue(t, outsider, n, true, 4), trust, now); err != ErrUntrusted {
		t.Fatalf("promotion by outsider: %v", err)
	}
	// Taking the admin right away from `second` also cuts what depends on it.
	if _, _, err := s.Offer(issue(t, root, second, false, 5, Share{ID: "docs"}), trust, now); err != nil {
		t.Fatal(err)
	}
	got = s.Trusted(trust)
	if got[second.id] || got[third.id] {
		t.Error("demoted admin (and the admin it promoted) still trusted")
	}
}

func TestFutureDatedGrantRefused(t *testing.T) {
	a, n := newDev(t), newDev(t)
	s := Load(filepath.Join(t.TempDir(), "g.json"))
	far := now.Add(30 * 24 * time.Hour).UnixNano()
	if _, _, err := s.Offer(issue(t, a, n, false, far), pin(a), now); err == nil {
		t.Fatal("a grant dated a month ahead was accepted")
	}
}

func TestIssueAlwaysBeatsThePreviousGrant(t *testing.T) {
	a, n := newDev(t), newDev(t)
	s := Load(filepath.Join(t.TempDir(), "g.json"))
	_, g1, err := s.Issue(a.key, Grant{Subject: n.id, Folders: []Share{{ID: "x"}}}, now)
	if err != nil {
		t.Fatal(err)
	}
	// The clock went back: the new grant must still be newer.
	_, g2, err := s.Issue(a.key, Grant{Subject: n.id, Revoked: true}, now.Add(-time.Hour))
	if err != nil || g2.Seq <= g1.Seq {
		t.Fatalf("seq %d after %d (%v)", g2.Seq, g1.Seq, err)
	}
	if cur, _ := s.Get(n.id); !cur.Revoked {
		t.Fatal("the newer grant was not kept")
	}
}

func TestPersistenceAndDamagedFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "g.json")
	a, n := newDev(t), newDev(t)
	s := Load(path)
	if _, _, err := s.Issue(a.key, Grant{Subject: n.id, Folders: []Share{{ID: "x", Relay: true}}}, now); err != nil {
		t.Fatal(err)
	}
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	if g, ok := Load(path).Get(n.id); !ok || len(g.Folders) != 1 || !g.Folders[0].Relay {
		t.Fatalf("not reloaded: %+v", g)
	}
	// A damaged file is set aside, not fatal.
	if err := writeFile(path, []byte("{broken")); err != nil {
		t.Fatal(err)
	}
	if got := Load(path); len(got.All()) != 0 {
		t.Fatal("damaged file produced grants")
	}
	if m, _ := filepath.Glob(path + ".corrupt-*"); len(m) != 1 {
		t.Fatalf("damaged file not kept aside: %v", m)
	}
}

func TestSetIsBounded(t *testing.T) {
	a := newDev(t)
	s := Load(filepath.Join(t.TempDir(), "g.json"))
	var refused int
	for i := 0; i < maxSubjects+20; i++ {
		if _, _, err := s.Issue(a.key, Grant{Subject: newDev(t).id}, now); err != nil {
			refused++
		}
	}
	if n := len(s.All()); n > maxSubjects {
		t.Fatalf("%d grants kept, limit %d", n, maxSubjects)
	}
	if refused == 0 {
		t.Fatal("going over the limit was not reported")
	}
}
