package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newInjectServer(t *testing.T) *Server {
	t.Helper()
	st := NewStore(t.TempDir())
	st.NewThread()
	return &Server{store: st, hub: NewHub(), injectToken: "0123456789abcdef0123456789abcdef"}
}

func injectPost(t *testing.T, srv *Server, body string, headers map[string]string, remote string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/inject", strings.NewReader(body))
	req.RemoteAddr = remote
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	srv.injectGuard(srv.handleInject)(rec, req)
	return rec
}

func TestInjectDeliversToThread(t *testing.T) {
	srv := newInjectServer(t)
	thread := srv.store.Snapshot().Threads[0].ID

	rec := injectPost(t, srv,
		`{"thread":"`+thread+`","text":"disk 71% used","title":"Disk digest"}`,
		map[string]string{injectHeader: srv.injectToken}, "127.0.0.1:45678")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var out struct {
		OK      bool   `json:"ok"`
		Thread  string `json:"thread"`
		Message int64  `json:"msgId"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("bad response JSON: %v", err)
	}
	if !out.OK || out.Thread != thread || out.Message == 0 {
		t.Fatalf("unexpected response: %+v", out)
	}

	snap := srv.store.Snapshot()
	if len(snap.Messages) != 1 {
		t.Fatalf("expected 1 message, got %d", len(snap.Messages))
	}
	m := snap.Messages[0]
	if m.Role != "assistant" || m.Text != "disk 71% used" {
		t.Fatalf("unexpected message: %+v", m)
	}
	if snap.Threads[0].Title != "Disk digest" {
		t.Fatalf("title from delivery not applied: %q", snap.Threads[0].Title)
	}
}

func TestInjectRoleAndErrors(t *testing.T) {
	srv := newInjectServer(t)
	thread := srv.store.Snapshot().Threads[0].ID
	ok := map[string]string{injectHeader: srv.injectToken}

	if rec := injectPost(t, srv, `{"thread":"`+thread+`","text":"boom","role":"error"}`, ok, "127.0.0.1:1"); rec.Code != http.StatusOK {
		t.Fatalf("error-role delivery failed: %d", rec.Code)
	}
	if rec := injectPost(t, srv, `{"thread":"nope","text":"x"}`, ok, "127.0.0.1:1"); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown thread should 404, got %d", rec.Code)
	}
	if rec := injectPost(t, srv, `{"thread":"`+thread+`","text":"  "}`, ok, "127.0.0.1:1"); rec.Code != http.StatusBadRequest {
		t.Fatalf("empty text should 400, got %d", rec.Code)
	}
	if rec := injectPost(t, srv, `{"thread":"`+thread+`","text":"x","role":"user"}`, ok, "127.0.0.1:1"); rec.Code != http.StatusBadRequest {
		t.Fatalf("role user should 400, got %d", rec.Code)
	}
}

func TestInjectGuard(t *testing.T) {
	srv := newInjectServer(t)
	body := `{"text":"hi"}`
	good := map[string]string{injectHeader: srv.injectToken}

	cases := []struct {
		name    string
		headers map[string]string
		remote  string
		want    int
	}{
		{"loopback + token", good, "127.0.0.1:9999", http.StatusOK},
		{"ipv6 loopback + token", good, "::1", http.StatusOK},
		{"missing token", nil, "127.0.0.1:9999", http.StatusUnauthorized},
		{"wrong token", map[string]string{injectHeader: "wrong"}, "127.0.0.1:9999", http.StatusUnauthorized},
		{"remote host", good, "192.168.1.9:5000", http.StatusForbidden},
		{"through cloudflare", map[string]string{injectHeader: srv.injectToken, "Cf-Connecting-Ip": "203.0.113.7"}, "127.0.0.1:9999", http.StatusForbidden},
		{"through proxy", map[string]string{injectHeader: srv.injectToken, "X-Forwarded-For": "203.0.113.7"}, "127.0.0.1:9999", http.StatusForbidden},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := injectPost(t, srv, body, c.headers, c.remote)
			if rec.Code != c.want {
				t.Fatalf("want %d, got %d (%s)", c.want, rec.Code, rec.Body.String())
			}
		})
	}

	srv.injectToken = ""
	if rec := injectPost(t, srv, body, good, "127.0.0.1:1"); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("disabled endpoint should 503, got %d", rec.Code)
	}
}

func TestInjectGuardRejectsGarbageHeader(t *testing.T) {
	srv := newInjectServer(t)
	rec := injectPost(t, srv, `{"text":"x"}`, map[string]string{injectHeader: srv.injectToken + "x"}, "127.0.0.1:1")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for a token superset, got %d", rec.Code)
	}
}

func TestLoadInjectTokenPersists(t *testing.T) {
	path := t.TempDir() + "/inject.token"
	t.Setenv("SENTINEL_TOKEN_FILE", path)
	t.Setenv("SENTINEL_INJECT_TOKEN", "")

	first, err := loadInjectToken()
	if err != nil || len(first) < 32 {
		t.Fatalf("generated token unusable: %q %v", first, err)
	}
	second, err := loadInjectToken()
	if err != nil || second != first {
		t.Fatalf("token not stable across loads: %q vs %q (%v)", second, first, err)
	}
	if b := bytes.TrimSpace([]byte(first)); len(b) != 64 {
		t.Fatalf("expected a 64-char hex token, got %d chars", len(b))
	}

	t.Setenv("SENTINEL_INJECT_TOKEN", "explicit-env-token")
	if got, _ := loadInjectToken(); got != "explicit-env-token" {
		t.Fatalf("env override not honoured: %q", got)
	}
}
