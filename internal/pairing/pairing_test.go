package pairing

import (
	"strings"
	"testing"
	"time"
)

const id = "12D3KooWDAKocJYhkhxMyDZ4SL1SRTvkUagXCVpapAsz788yQNqX"

func TestInviteRoundTrip(t *testing.T) {
	in := Invite{V: 1, ID: id, Name: "pc", Addrs: []string{"/ip4/192.168.1.2/udp/22000/quic-v1"}, Token: "tok",
		Folder: "docs", InviteeRelay: true, Exp: time.Now().Add(time.Minute).Unix()}
	code := in.Encode()
	if !strings.HasPrefix(code, Scheme) {
		t.Fatal("missing scheme")
	}
	for _, form := range []string{code, strings.TrimPrefix(code, Scheme), "  " + code + "\n"} {
		out, err := Decode(form)
		if err != nil || out.ID != id || out.Folder != "docs" || !out.InviteeRelay || out.Token != "tok" {
			t.Fatalf("decode %q: %+v %v", form, out, err)
		}
	}
}

func TestDecodeRejectsGarbage(t *testing.T) {
	bad := []string{"", "hello", Scheme + "!!!", Scheme + "e30" /* {} */}
	for _, b := range bad {
		if _, err := Decode(b); err == nil {
			t.Errorf("%q accepted", b)
		}
	}
	both := Invite{V: 1, ID: id, Token: "t", Folder: "f", InviterRelay: true, InviteeRelay: true}
	if _, err := Decode(both.Encode()); err == nil {
		t.Error("relay inviting a relay accepted")
	}
	wrongVersion := Invite{V: 2, ID: id, Token: "t", Folder: "f"}
	if _, err := Decode(wrongVersion.Encode()); err == nil {
		t.Error("unknown version accepted")
	}
}

func TestTokenIsSingleUseAndExpires(t *testing.T) {
	s := NewStore(t.TempDir())
	now := time.Now()
	if s.Active(now) {
		t.Fatal("empty store reports active invitations")
	}
	tok, _ := NewToken()
	if err := s.Add(tok, Pending{Folder: "docs", Exp: now.Add(time.Minute).Unix()}); err != nil {
		t.Fatal(err)
	}
	if !s.Active(now) {
		t.Fatal("invitation not active")
	}
	if _, ok := s.Consume("wrong-token", now); ok {
		t.Fatal("wrong token accepted")
	}
	if p, ok := s.Consume(tok, now); !ok || p.Folder != "docs" {
		t.Fatal("valid token refused")
	}
	if _, ok := s.Consume(tok, now); ok {
		t.Fatal("token reused")
	}
	if s.Active(now) {
		t.Fatal("consumed invitation still active")
	}

	old, _ := NewToken()
	_ = s.Add(old, Pending{Folder: "docs", Exp: now.Add(-time.Second).Unix()})
	if _, ok := s.Consume(old, now); ok {
		t.Fatal("expired token accepted")
	}
}

func TestOnlyOneConcurrentConsumerWins(t *testing.T) {
	s := NewStore(t.TempDir())
	tok, _ := NewToken()
	_ = s.Add(tok, Pending{Folder: "f", Exp: time.Now().Add(time.Minute).Unix()})
	wins := make(chan bool, 20)
	for i := 0; i < 20; i++ {
		go func() { _, ok := s.Consume(tok, time.Now()); wins <- ok }()
	}
	n := 0
	for i := 0; i < 20; i++ {
		if <-wins {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("%d consumers succeeded, want exactly 1", n)
	}
}

func TestPurgeCancelsEverything(t *testing.T) {
	s := NewStore(t.TempDir())
	for i := 0; i < 3; i++ {
		tok, _ := NewToken()
		_ = s.Add(tok, Pending{Folder: "f", Exp: time.Now().Add(time.Minute).Unix()})
	}
	s.Purge()
	if s.Active(time.Now()) {
		t.Fatal("invitations survived purge")
	}
}

func TestQRRendering(t *testing.T) {
	code := Invite{V: 1, ID: id, Token: "t", Folder: "f", Addrs: []string{"/ip4/1.2.3.4/udp/22000/quic-v1"}}.Encode()
	txt, err := QRText(code)
	if err != nil || len(txt) < 200 {
		t.Fatalf("terminal QR: %v", err)
	}
	png, err := QRPNG(code, 256)
	if err != nil || len(png) < 100 || string(png[1:4]) != "PNG" {
		t.Fatalf("png QR: %v", err)
	}
}
