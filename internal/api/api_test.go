package api

import (
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"
)

func startTest(t *testing.T) (*Server, *int) {
	t.Helper()
	calls := new(int)
	srv, err := Start("127.0.0.1:0", func() any { return map[string]string{"ok": "1"} },
		func(folder string, asRelay bool) (InviteResult, error) {
			*calls++
			return InviteResult{Code: "p2psync://pair/x", PNG: []byte("png"), Expires: time.Now().Add(time.Minute)}, nil
		}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Stop)
	return srv, calls
}

func do(t *testing.T, srv *Server, method, path, body string, hdr map[string]string) int {
	t.Helper()
	req, _ := http.NewRequest(method, "http://"+srv.Addr()+path, strings.NewReader(body))
	for k, v := range hdr {
		if k == "Host" {
			req.Host = v
		} else {
			req.Header.Set(k, v)
		}
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func TestInviteEndpointOnlyServesOurOwnPage(t *testing.T) {
	srv, calls := startTest(t)
	json := map[string]string{"Content-Type": "application/json"}
	body := `{"folder":"docs"}`

	if c := do(t, srv, "POST", "/api/invite", body, json); c != 200 {
		t.Fatalf("legitimate request refused: %d", c)
	}
	cases := map[string]struct {
		method string
		hdr    map[string]string
	}{
		"GET":                {"GET", json},
		"form post (CSRF)":   {"POST", map[string]string{"Content-Type": "application/x-www-form-urlencoded"}},
		"other origin":       {"POST", map[string]string{"Content-Type": "application/json", "Origin": "http://evil.example"}},
		"cross-site fetch":   {"POST", map[string]string{"Content-Type": "application/json", "Sec-Fetch-Site": "cross-site"}},
		"DNS rebinding host": {"POST", map[string]string{"Content-Type": "application/json", "Host": "evil.example:8384", "Origin": "http://evil.example:8384"}},
	}
	before := *calls
	for name, c := range cases {
		if code := do(t, srv, c.method, "/api/invite", body, c.hdr); code != http.StatusForbidden {
			t.Errorf("%s: got %d, want 403", name, code)
		}
	}
	if *calls != before {
		t.Fatal("an invitation was created by a forbidden request")
	}
}

func TestStatusIsAlsoProtectedFromRebinding(t *testing.T) {
	srv, _ := startTest(t)
	if c := do(t, srv, "GET", "/api/status", "", nil); c != 200 {
		t.Fatalf("status refused: %d", c)
	}
	if c := do(t, srv, "GET", "/api/status", "", map[string]string{"Host": "evil.example"}); c != http.StatusForbidden {
		t.Fatalf("rebinding not blocked: %d", c)
	}
}
