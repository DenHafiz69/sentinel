package main

// Loopback delivery endpoint: the Hermes gateway pushes cron / notification
// output into a Sentinel thread as a real chat message.
//
// Transport contract (consumed by ~/.hermes/plugins/sentinel/adapter.py):
//
//	POST /api/inject
//	X-Sentinel-Token: <shared secret>
//	{"thread": "t3", "text": "...", "role": "assistant|error", "title": "..."}
//	-> {"ok": true, "thread": "t3", "msgId": 41}
//
// The endpoint deliberately sits OUTSIDE the Cloudflare Access middleware: the
// gateway is a local process with no Access JWT. It is guarded instead by
// (a) a loopback-only RemoteAddr, (b) refusal of any request carrying
// Cloudflare / proxy headers, so the public hostname cannot reach it even
// though cloudflared dials 127.0.0.1, and (c) a 32-byte shared secret from
// ~/.config/sentinel/inject.token (auto-generated on first run, 0600).

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

const injectHeader = "X-Sentinel-Token"

// injectTokenPath resolves the shared-secret file. SENTINEL_TOKEN_FILE exists
// so a test instance can run against a scratch data dir without touching the
// live secret.
func injectTokenPath() string {
	if p := strings.TrimSpace(os.Getenv("SENTINEL_TOKEN_FILE")); p != "" {
		return p
	}
	return filepath.Join(homeDir, ".config", "sentinel", "inject.token")
}

// loadInjectToken returns the shared secret: SENTINEL_INJECT_TOKEN wins, else
// the token file, which is created on first run so nobody has to handle a
// secret by hand. An empty result disables the endpoint.
func loadInjectToken() (string, error) {
	if v := strings.TrimSpace(os.Getenv("SENTINEL_INJECT_TOKEN")); v != "" {
		return v, nil
	}
	p := injectTokenPath()
	// #nosec G304 — the path is either our own env override or a fixed
	// ~/.config/sentinel path; no request data reaches this read.
	if b, err := os.ReadFile(p); err == nil {
		if tok := strings.TrimSpace(string(b)); len(tok) >= 32 {
			return tok, nil
		}
	}
	if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
		return "", err
	}
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	tok := hex.EncodeToString(buf)
	if err := os.WriteFile(p, []byte(tok+"\n"), 0600); err != nil {
		return "", err
	}
	log.Printf("sentinel: generated inject token at %s", p)
	return tok, nil
}

// injectGuard admits only local, untunneled callers holding the shared secret.
func (s *Server) injectGuard(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil {
			host = r.RemoteAddr
		}
		if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
			http.Error(w, "forbidden: loopback only", http.StatusForbidden)
			return
		}
		// cloudflared dials 127.0.0.1, so RemoteAddr alone would admit the
		// public hostname. Anything that crossed the edge carries these.
		for _, h := range []string{"Cf-Connecting-Ip", "Cf-Ray", "Cf-Ipcountry", "X-Forwarded-For", "X-Real-Ip"} {
			if r.Header.Get(h) != "" {
				http.Error(w, "forbidden: inject is not reachable through the tunnel", http.StatusForbidden)
				return
			}
		}
		if s.injectToken == "" {
			http.Error(w, "inject disabled: no token configured", http.StatusServiceUnavailable)
			return
		}
		tok := r.Header.Get(injectHeader)
		if subtle.ConstantTimeCompare([]byte(tok), []byte(s.injectToken)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

type injectRequest struct {
	Thread string `json:"thread"`
	Text   string `json:"text"`
	Role   string `json:"role"`
	Title  string `json:"title"`
}

// handleInject appends a gateway-delivered message to a thread and pushes it to
// every open client. It never queues an agent run: the content is final.
func (s *Server) handleInject(w http.ResponseWriter, r *http.Request) {
	var req injectRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	text := strings.TrimSpace(req.Text)
	if text == "" {
		http.Error(w, "empty text", http.StatusBadRequest)
		return
	}
	role := "assistant"
	switch req.Role {
	case "error":
		role = "error"
	case "", "assistant":
	default:
		http.Error(w, "role must be assistant or error", http.StatusBadRequest)
		return
	}
	title := strings.TrimSpace(req.Title)
	if len(title) > 80 {
		title = title[:80]
	}
	m, threadID, ok := s.store.AddDelivered(req.Thread, role, text, title)
	if !ok {
		http.Error(w, "no such thread: "+req.Thread, http.StatusNotFound)
		return
	}
	s.broadcast("msg", map[string]any{"thread": threadID, "message": m})
	// A snapshot as well, so an idle client re-sorts its sidebar and a client
	// parked on another thread still notices the delivery.
	s.broadcast("snapshot", s.snapshotPayload())
	log.Printf("sentinel: delivered message %d to thread %s", m.ID, sanitizeLog(threadID))
	writeJSON(w, map[string]any{"ok": true, "thread": threadID, "msgId": m.ID})
}
