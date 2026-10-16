package api

import (
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"
)

const testToken = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func startTest(t *testing.T) (*Server, *int) {
	t.Helper()
	calls := new(int)
	srv, err := Start("127.0.0.1:0", testToken, func() any { return map[string]string{"ok": "1"} },
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
	json := map[string]string{"Content-Type": "application/json", "Authorization": "Bearer " + testToken}
	body := `{"folder":"docs"}`

	if c := do(t, srv, "POST", "/api/invite", body, json); c != 200 {
		t.Fatalf("legitimate request refused: %d", c)
	}
	cases := map[string]struct {
		method string
		hdr    map[string]string
	}{
		"GET":                {"GET", json},
		"form post (CSRF)":   {"POST", map[string]string{"Content-Type": "application/x-www-form-urlencoded", "Authorization": "Bearer " + testToken}},
		"other origin":       {"POST", map[string]string{"Content-Type": "application/json", "Authorization": "Bearer " + testToken, "Origin": "http://evil.example"}},
		"cross-site fetch":   {"POST", map[string]string{"Content-Type": "application/json", "Authorization": "Bearer " + testToken, "Sec-Fetch-Site": "cross-site"}},
		"DNS rebinding host": {"POST", map[string]string{"Content-Type": "application/json", "Authorization": "Bearer " + testToken, "Host": "evil.example:8384", "Origin": "http://evil.example:8384"}},
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
	if c := do(t, srv, "GET", "/api/status", "", map[string]string{"Authorization": "Bearer " + testToken}); c != 200 {
		t.Fatalf("status refused: %d", c)
	}
	if c := do(t, srv, "GET", "/api/status", "", map[string]string{"Host": "evil.example", "Authorization": "Bearer " + testToken}); c != http.StatusForbidden {
		t.Fatalf("rebinding not blocked: %d", c)
	}
}

// Another account on the same machine can reach 127.0.0.1:8384 but does not
// have the token stored in the owner's config directory.
func TestAPIRequiresTheLocalToken(t *testing.T) {
	srv, calls := startTest(t)
	for name, hdr := range map[string]map[string]string{
		"no token":    nil,
		"wrong token": {"Authorization": "Bearer nope"},
		"empty":       {"Authorization": "Bearer "},
	} {
		if c := do(t, srv, "GET", "/api/status", "", hdr); c != http.StatusUnauthorized {
			t.Errorf("status with %s: %d, want 401", name, c)
		}
		h := map[string]string{"Content-Type": "application/json"}
		for k, v := range hdr {
			h[k] = v
		}
		if c := do(t, srv, "POST", "/api/invite", `{"folder":"docs"}`, h); c != http.StatusUnauthorized {
			t.Errorf("invite with %s: %d, want 401", name, c)
		}
	}
	if *calls != 0 {
		t.Fatal("an invitation was created without the token")
	}
}

// Names and labels can come from other devices. The page must never turn them
// into markup, and the browser must be told to refuse inline script anyway.
func TestPageCannotBeUsedForScriptInjection(t *testing.T) {
	for _, bad := range []string{"innerHTML", "outerHTML", "insertAdjacentHTML", "document.write", "eval("} {
		if strings.Contains(appJS, bad) {
			t.Errorf("app.js uses %s", bad)
		}
	}
	if strings.Contains(page, "<script>") || strings.Contains(strings.ReplaceAll(page, `<script src="/app.js"></script>`, ""), "<script") {
		t.Error("the page contains an inline script")
	}
	srv, _ := startTest(t)
	resp, err := http.Get("http://" + srv.Addr() + "/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	csp := resp.Header.Get("Content-Security-Policy")
	if !strings.Contains(csp, "default-src 'none'") || strings.Contains(csp, "script-src 'unsafe-inline'") {
		t.Errorf("weak CSP: %q", csp)
	}
	if resp.Header.Get("X-Frame-Options") != "DENY" || resp.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Error("missing hardening headers")
	}
}
