package main

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

//go:embed web
var webFS embed.FS

var (
	homeDir string
	homeBin string
	dataDir string
)

func init() {
	hd, err := os.UserHomeDir()
	if err != nil {
		log.Fatalf("no home dir: %v", err)
	}
	homeDir = hd
	homeBin = filepath.Join(homeDir, ".local", "bin")
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

type Job struct {
	Thread string
	MsgID  int64
	Query  string
}

type ThreadRun struct {
	MsgID      int64
	Text       strings.Builder
	Activities []Activity
	Cancel     context.CancelFunc
	LastFlush  time.Time // last time a cumulative "delta" event was broadcast
}

type Server struct {
	store         *Store
	hub           *Hub
	auth          *AccessAuth
	allowedEmails map[string]bool
	dev           bool
	uploadDir     string

	injectToken string // shared secret for POST /api/inject ("" disables it)

	notifier *notifier // telegram alerts for failed runs; nil disables them

	jobs chan Job // legacy fallback if initialized directly in tests

	mu            sync.Mutex
	running       bool
	runningThread string
	runningMsgID  int64
	runningText   strings.Builder
	activities    []Activity
	cancel        context.CancelFunc

	threadQueues map[string]chan Job
	activeRuns   map[string]*ThreadRun

	// shutdownCh is closed by hs.RegisterOnShutdown so the SSE stream
	// handlers return and http.Server.Shutdown does not stall on them.
	// nil in tests: a nil channel never fires, so handlers behave as before.
	shutdownCh <-chan struct{}
}

type ctxKey int

const emailKey ctxKey = 1

func emailFrom(r *http.Request) string {
	if v, ok := r.Context().Value(emailKey).(string); ok {
		return v
	}
	return ""
}

// ---------- SSE hub ----------

type Hub struct {
	mu      sync.Mutex
	clients map[chan []byte]struct{}
}

func NewHub() *Hub { return &Hub{clients: map[chan []byte]struct{}{}} }

func (h *Hub) Add() chan []byte {
	ch := make(chan []byte, 128)
	h.mu.Lock()
	h.clients[ch] = struct{}{}
	h.mu.Unlock()
	return ch
}

func (h *Hub) Remove(ch chan []byte) {
	h.mu.Lock()
	delete(h.clients, ch)
	h.mu.Unlock()
}

func (h *Hub) Send(b []byte) {
	h.mu.Lock()
	for c := range h.clients {
		select {
		case c <- b:
		default: // slow client drops an event; the next coalesced delta
			// or the run-end snapshot carries the full text again
		}
	}
	h.mu.Unlock()
}

// ---------- helpers ----------

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("webchat: json encode failed: %v", err)
	}
}

// sanitizeLog strips control characters so nothing can forge log lines.
func sanitizeLog(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 32 || r == 127 {
			return -1
		}
		return r
	}, s)
}

func (s *Server) broadcast(kind string, data any) {
	b, err := json.Marshal(data)
	if err != nil {
		return
	}
	s.hub.Send([]byte(fmt.Sprintf("event: %s\ndata: %s\n\n", kind, b)))
}

func (s *Server) runningState() (bool, string, int64, string, []Activity, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.activeRuns) > 0 {
		for tid, ar := range s.activeRuns {
			qLen := 0
			if ch, ok := s.threadQueues[tid]; ok {
				qLen = len(ch)
			}
			return true, tid, ar.MsgID, ar.Text.String(), cloneActivities(ar.Activities), qLen
		}
	}
	return s.running, s.runningThread, s.runningMsgID, s.runningText.String(), cloneActivities(s.activities), len(s.jobs)
}

func (s *Server) snapshotPayload() map[string]any {
	return s.snapshotPayloadFor("")
}

func (s *Server) snapshotPayloadFor(threadID string) map[string]any {
	snap := s.store.SnapshotFor(threadID)
	s.mu.Lock()
	var runningThreads []string
	for tid := range s.activeRuns {
		runningThreads = append(runningThreads, tid)
	}
	if s.running && s.runningThread != "" {
		found := false
		for _, tid := range runningThreads {
			if tid == s.runningThread {
				found = true
				break
			}
		}
		if !found {
			runningThreads = append(runningThreads, s.runningThread)
		}
	}
	var (
		r   bool
		t   string
		m   int64
		txt string
		act []Activity
		q   int
	)
	if ar, ok := s.activeRuns[snap.Active]; ok {
		r, t, m, txt, act = true, snap.Active, ar.MsgID, ar.Text.String(), cloneActivities(ar.Activities)
		if ch, ok := s.threadQueues[snap.Active]; ok {
			q = len(ch)
		}
	} else if s.running && s.runningThread == snap.Active {
		r, t, m, txt, act, q = true, s.runningThread, s.runningMsgID, s.runningText.String(), cloneActivities(s.activities), len(s.jobs)
	}
	s.mu.Unlock()
	if runningThreads == nil {
		runningThreads = []string{}
	}
	return map[string]any{
		"threads":        snap.Threads,
		"active":         snap.Active,
		"messages":       snap.Messages,
		"running":        r,
		"runningThread":  t,
		"runningThreads": runningThreads,
		"runningMsgId":   m,
		"runningText":    txt,
		"activities":     act,
		"queued":         q,
	}
}

// ---------- auth middleware ----------

func parseAllowedEmails(raw string) map[string]bool {
	out := map[string]bool{}
	for _, part := range strings.Split(raw, ",") {
		if e := strings.ToLower(strings.TrimSpace(part)); e != "" {
			out[e] = true
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func (s *Server) isEmailAllowed(email string) bool {
	if len(s.allowedEmails) == 0 {
		return true
	}
	return s.allowedEmails[strings.ToLower(strings.TrimSpace(email))]
}

func (s *Server) authed(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.dev {
			next(w, r.WithContext(context.WithValue(r.Context(), emailKey, "dev@local")))
			return
		}
		tok := r.Header.Get("Cf-Access-Jwt-Assertion")
		if tok == "" {
			if c, err := r.Cookie("CF_Authorization"); err == nil {
				tok = c.Value
			}
		}
		if tok == "" {
			tok = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		}
		if !strings.Contains(tok, ".") {
			s.deny(w, r)
			return
		}
		email, err := s.auth.Validate(tok)
		if err != nil {
			// #nosec G706 — control characters are stripped by sanitizeLog
			// and %q quotes the result, so no log forging is possible.
			log.Printf("access denied (%q): %v", sanitizeLog(r.URL.Path), err)
			s.deny(w, r)
			return
		}
		if !s.isEmailAllowed(email) {
			// #nosec G706 — control characters are stripped by sanitizeLog
			// and %q quotes the result, so no log forging is possible.
			log.Printf("access denied (%q): email %q not in ALLOWED_EMAILS", sanitizeLog(r.URL.Path), sanitizeLog(email))
			s.deny(w, r)
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), emailKey, email)))
	}
}

func (s *Server) deny(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/api/") {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"unauthorized: sign in via Cloudflare Access first"}`))
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusUnauthorized)
	fmt.Fprint(w, "<!doctype html><meta charset=utf-8><title>Sentinel</title><p style='font:15px system-ui;padding:40px'>401 — sign in via Cloudflare Access first.</p>")
}

// ---------- agent worker ----------

// How often a running reply streams a cumulative "delta" event. Bounded so a
// fast model does not flood slow clients, small enough to feel live.
const streamFlushInterval = 250 * time.Millisecond

func (s *Server) threadQueue(threadID string) chan Job {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.threadQueues == nil {
		s.threadQueues = map[string]chan Job{}
	}
	ch, ok := s.threadQueues[threadID]
	if !ok {
		ch = make(chan Job, 16)
		s.threadQueues[threadID] = ch
		go s.threadWorker(ch)
	}
	return ch
}

func (s *Server) threadWorker(ch <-chan Job) {
	for job := range ch {
		s.runJob(job)
	}
}

func (s *Server) cancelAllRuns() {
	s.mu.Lock()
	if s.cancel != nil {
		s.cancel()
	}
	for _, ar := range s.activeRuns {
		if ar.Cancel != nil {
			ar.Cancel()
		}
	}
	s.mu.Unlock()
}

func (s *Server) runJob(job Job) {
	ctx, cancel := context.WithCancel(context.Background())
	ar := &ThreadRun{MsgID: job.MsgID, Cancel: cancel}
	s.mu.Lock()
	if s.activeRuns == nil {
		s.activeRuns = map[string]*ThreadRun{}
	}
	s.activeRuns[job.Thread] = ar
	qLen := 0
	if ch, ok := s.threadQueues[job.Thread]; ok {
		qLen = len(ch)
	}
	s.mu.Unlock()
	s.broadcast("status", map[string]any{"running": true, "thread": job.Thread, "msgId": job.MsgID, "queued": qLen})

	session := s.store.SessionName(job.Thread)
	model := s.store.Model(job.Thread)
	res := runHermes(ctx, session, model, job.Query, func(delta string) {
		s.mu.Lock()
		ar.Text.WriteString(delta)
		text := ar.Text.String()
		flush := time.Since(ar.LastFlush) >= streamFlushInterval
		if flush {
			ar.LastFlush = time.Now()
		}
		s.mu.Unlock()
		// Coalesced: at most one event per interval, and each event carries
		// the full text so far, so a client that misses one is healed by the
		// next instead of silently losing a chunk until the run ends.
		if flush {
			s.broadcast("delta", map[string]any{"thread": job.Thread, "id": job.MsgID, "text": text})
		}
	}, func(acts []Activity) {
		s.mu.Lock()
		ar.Activities = acts
		s.mu.Unlock()
		s.broadcast("activity", map[string]any{"thread": job.Thread, "msgId": job.MsgID, "activities": acts})
	})

	cancel()
	s.mu.Lock()
	delete(s.activeRuns, job.Thread)
	qLen = 0
	if ch, ok := s.threadQueues[job.Thread]; ok {
		qLen = len(ch)
	}
	s.mu.Unlock()

	switch {
	case res.Err != nil:
		// A run that died partway through may still have produced usable text.
		// Keep it as the reply instead of throwing the work away, and say so.
		partial := strings.TrimSpace(res.Text) != ""
		if partial {
			note := "\n\n_[run ended early: " + res.Err.Error() + "]_"
			pm := s.store.FinalizeAssistant(job.Thread, job.MsgID, strings.TrimRight(res.Text, "\n")+note, extractAttachments(res.Text), res.Activities, res.SessionID)
			s.broadcast("msg", map[string]any{"thread": job.Thread, "message": pm})
		}
		msg := "Agent run failed: " + res.Err.Error()
		if res.TimedOut {
			if partial {
				msg += "\nThe partial reply was kept above."
			}
			msg += fmt.Sprintf("\nThe limit is %s; raise SENTINEL_RUN_TIMEOUT (or run long jobs from the CLI) if this needs longer.", fmtLimit(runLimit()))
		}
		if tail := strings.TrimSpace(res.StderrTail); tail != "" {
			msg += "\n" + shorten(tail)
		}
		log.Printf("webchat job error (thread %s): %v", job.Thread, res.Err)
		if s.alertRunFailure(job.Thread, session, res.Err, res.Text) {
			msg += "\n\n_Alert sent to Telegram._"
		}
		m := s.store.AddError(job.Thread, msg)
		s.broadcast("msg", map[string]any{"thread": job.Thread, "message": m})
	case res.Stopped:
		text := res.Text + "\n\n_[stopped]_"
		m := s.store.FinalizeAssistant(job.Thread, job.MsgID, text, extractAttachments(text), res.Activities, res.SessionID)
		s.broadcast("msg", map[string]any{"thread": job.Thread, "message": m})
	default:
		m := s.store.FinalizeAssistant(job.Thread, job.MsgID, res.Text, extractAttachments(res.Text), res.Activities, res.SessionID)
		s.broadcast("msg", map[string]any{"thread": job.Thread, "message": m})
	}
	s.broadcast("status", map[string]any{"running": false, "thread": job.Thread, "msgId": 0, "queued": qLen})
}

// ---------- handlers ----------

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	b, err := webFS.ReadFile("web/index.html")
	if err != nil {
		http.Error(w, "missing UI build", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; img-src 'self' blob: data:; style-src 'unsafe-inline'; script-src 'unsafe-inline'; connect-src 'self'")
	_, _ = w.Write(b)
}

// serveWebAsset serves the embedded PWA files (manifest, service worker, icons).
func (s *Server) serveWebAsset(w http.ResponseWriter, r *http.Request) {
	var (
		b     []byte
		ctype string
		err   error
	)
	switch strings.TrimPrefix(r.URL.Path, "/") {
	case "manifest.webmanifest":
		b, err = webFS.ReadFile("web/manifest.webmanifest")
		ctype = "application/manifest+json"
	case "sw.js":
		b, err = webFS.ReadFile("web/sw.js")
		ctype = "text/javascript; charset=utf-8"
	case "icon-192.png":
		b, err = webFS.ReadFile("web/icon-192.png")
		ctype = "image/png"
	case "icon-512.png":
		b, err = webFS.ReadFile("web/icon-512.png")
		ctype = "image/png"
	case "icon-512-maskable.png":
		b, err = webFS.ReadFile("web/icon-512-maskable.png")
		ctype = "image/png"
	case "apple-touch-icon.png":
		b, err = webFS.ReadFile("web/apple-touch-icon.png")
		ctype = "image/png"
	default:
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", ctype)
	if strings.TrimPrefix(r.URL.Path, "/") == "sw.js" {
		w.Header().Set("Cache-Control", "no-cache") // SW updates must revalidate
	} else {
		w.Header().Set("Cache-Control", "public, max-age=86400")
	}
	_, _ = w.Write(b)
}

func (s *Server) handleState(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, s.snapshotPayloadFor(r.URL.Query().Get("thread")))
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{"email": emailFrom(r), "dev": s.dev, "version": Version})
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	ch := s.hub.Add()
	defer s.hub.Remove(ch)
	if b, err := json.Marshal(s.snapshotPayload()); err == nil {
		fmt.Fprintf(w, "event: snapshot\ndata: %s\n\n", b)
		fl.Flush()
	}
	tick := time.NewTicker(20 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-s.shutdownCh:
			// Server is draining: release the connection so Shutdown
			// can finish; the EventSource client reconnects on its own.
			return
		case <-tick.C:
			fmt.Fprint(w, ": ping\n\n")
			fl.Flush()
		case b := <-ch:
			_, _ = w.Write(b)
			fl.Flush()
		}
	}
}

func (s *Server) handleNew(w http.ResponseWriter, r *http.Request) {
	s.store.NewThread()
	s.broadcast("snapshot", s.snapshotPayload())
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) handleActive(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req); err != nil || !s.store.SetActive(req.ID) {
		http.Error(w, "no such thread", http.StatusBadRequest)
		return
	}
	s.broadcast("snapshot", s.snapshotPayload())
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("q")
	hits := s.store.Search(q, 50)
	if hits == nil {
		hits = []SearchHit{}
	}
	writeJSON(w, map[string]any{"hits": hits})
}

func (s *Server) handleThreadRename(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID    string `json:"id"`
		Title string `json:"title"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req); err != nil || !s.store.RenameThread(req.ID, req.Title) {
		http.Error(w, "invalid thread or title", http.StatusBadRequest)
		return
	}
	s.broadcast("snapshot", s.snapshotPayload())
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) handleThreadPin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID     string `json:"id"`
		Pinned bool   `json:"pinned"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req); err != nil || !s.store.SetPinned(req.ID, req.Pinned) {
		http.Error(w, "no such thread", http.StatusBadRequest)
		return
	}
	s.broadcast("snapshot", s.snapshotPayload())
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) handleThreadArchive(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID       string `json:"id"`
		Archived bool   `json:"archived"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req); err != nil || !s.store.SetArchived(req.ID, req.Archived) {
		http.Error(w, "no such thread", http.StatusBadRequest)
		return
	}
	s.broadcast("snapshot", s.snapshotPayload())
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) stopAndCleanThread(threadID string) {
	s.mu.Lock()
	var cancel context.CancelFunc
	if ar, ok := s.activeRuns[threadID]; ok {
		cancel = ar.Cancel
	}
	if ch, ok := s.threadQueues[threadID]; ok {
		delete(s.threadQueues, threadID)
		close(ch)
	}
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (s *Server) handleThreadDelete(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req); err != nil {
		http.Error(w, "no such thread", http.StatusBadRequest)
		return
	}
	id := req.ID
	if id == "" {
		id = s.store.Snapshot().Active
	}
	if !s.store.DeleteThread(id) {
		http.Error(w, "no such thread", http.StatusBadRequest)
		return
	}
	s.stopAndCleanThread(id)
	s.broadcast("snapshot", s.snapshotPayload())
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) handleThreadClear(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req); err != nil || !s.store.ClearThread(req.ID) {
		http.Error(w, "no such thread", http.StatusBadRequest)
		return
	}
	s.broadcast("snapshot", s.snapshotPayload())
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) handleThreadModel(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID    string `json:"id"`
		Model string `json:"model"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req); err != nil || !s.store.SetModel(req.ID, req.Model) {
		http.Error(w, "invalid thread or model", http.StatusBadRequest)
		return
	}
	s.broadcast("snapshot", s.snapshotPayload())
	writeJSON(w, map[string]any{"ok": true, "model": s.store.Model(req.ID)})
}

func discoverHermesModels() []string {
	var out []string
	if raw := os.Getenv("SENTINEL_MODELS"); raw != "" {
		for _, part := range strings.Split(raw, ",") {
			if m := strings.TrimSpace(part); m != "" && validModelName(m) {
				out = append(out, m)
			}
		}
	}
	cfgPath := filepath.Join(homeDir, ".hermes", "config.yaml")
	// #nosec G304 — cfgPath is fixed to ~/.hermes/config.yaml under homeDir.
	if b, err := os.ReadFile(cfgPath); err == nil {
		inModels := false
		for _, line := range strings.Split(string(b), "\n") {
			trimmed := strings.TrimSpace(line)
			if trimmed == "" || strings.HasPrefix(trimmed, "#") {
				continue
			}
			if strings.HasPrefix(trimmed, "model:") {
				v := strings.TrimSpace(strings.TrimPrefix(trimmed, "model:"))
				v = strings.Trim(v, `"'`)
				if v != "" && validModelName(v) {
					out = append(out, v)
				}
				inModels = false
				continue
			}
			if trimmed == "models:" {
				inModels = true
				continue
			}
			if inModels {
				if strings.HasPrefix(trimmed, "- ") {
					v := strings.TrimSpace(strings.TrimPrefix(trimmed, "- "))
					v = strings.Trim(v, `"'`)
					if v != "" && validModelName(v) {
						out = append(out, v)
					}
				} else if !strings.HasPrefix(line, " ") && !strings.HasPrefix(line, "\t") {
					inModels = false
				}
			}
		}
	}
	return out
}

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	seen := map[string]bool{}
	var models []string
	add := func(m string) {
		m = strings.TrimSpace(m)
		if m != "" && !seen[m] {
			seen[m] = true
			models = append(models, m)
		}
	}
	for _, m := range discoverHermesModels() {
		add(m)
	}
	for _, m := range s.store.UsedModels() {
		add(m)
	}
	if models == nil {
		models = []string{}
	}
	snap := s.store.Snapshot()
	writeJSON(w, map[string]any{
		"current": s.store.Model(snap.Active),
		"models":  models,
	})
}

// sentinelNote tells the agent, on every message, how this chat handles long
// work: the run is bounded, anything longer goes to the background, and results
// come back through the inject delivery path as real chat messages.
func sentinelNote(thread string) string {
	return fmt.Sprintf(
		"[Sentinel house rules: this thread is %s and this run is capped at %s. "+
			"If the task needs more than about 8 minutes of work, do not grind through it in this reply: "+
			"start it in the background (terminal background=true, or a scheduled one-shot cron job) "+
			"and finish the reply saying what is running and when it should be done. "+
			"To post results back into this chat, schedule a one-shot cron whose deliver is sentinel:%s. "+
			"On each new message, first report any background work that has finished, briefly.]",
		thread, fmtLimit(runLimit()), thread)
}

// composeQuery shapes the message Hermes receives. Images follow the upstream
// gateway convention (tui_gateway/session_history.py): reference by absolute
// path and point the agent at vision_analyze, since one-shot mode has no
// native image attach. Plain files are listed as paths the agent can open.
func composeQuery(thread, text string, files []Attachment) string {
	note := sentinelNote(thread)
	if len(files) == 0 {
		return text + "\n\n" + note
	}
	var lines []string
	var plain []Attachment
	for _, f := range files {
		if f.Kind == "image" {
			lines = append(lines,
				fmt.Sprintf("[The user attached an image: %s]", f.Name),
				fmt.Sprintf("[Examine it with the vision_analyze tool using image_url: %s]", f.Path))
		} else {
			plain = append(plain, f)
		}
	}
	if len(plain) > 0 {
		lines = append(lines, "Hafiz attached file(s) via webchat (open them from these paths):")
		for _, f := range plain {
			lines = append(lines, fmt.Sprintf("- %s (%s)", f.Path, f.Name))
		}
	}
	suffix := strings.Join(lines, "\n")
	if text != "" {
		return text + "\n\n" + suffix + "\n\n" + note
	}
	return suffix + "\n\n" + note
}

func (s *Server) handleSend(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Thread string `json:"thread"`
		Text   string `json:"text"`
		Files  []struct {
			Path string `json:"path"`
			Name string `json:"name"`
		} `json:"files"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4<<20)).Decode(&req); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	text := strings.TrimSpace(req.Text)
	var files []Attachment
	for _, f := range req.Files {
		// only files that were actually uploaded through this server are
		// attachable; anything else is ignored.
		p := filepath.Clean(f.Path)
		if !strings.HasPrefix(p, s.uploadDir+string(os.PathSeparator)) {
			continue
		}
		fi, err := os.Stat(p)
		if err != nil || !fi.Mode().IsRegular() {
			continue
		}
		att := Attachment{Path: p, Name: filepath.Base(p), Size: fi.Size(), Kind: kindForFile(p)}
		if att.Name == "" {
			att.Name = filepath.Base(p)
		}
		files = append(files, att)
	}
	if text == "" && len(files) == 0 {
		http.Error(w, "empty message", http.StatusBadRequest)
		return
	}

	targetThread := strings.TrimSpace(req.Thread)
	if targetThread == "" {
		targetThread = s.store.Snapshot().Active
	}
	q := s.threadQueue(targetThread)
	if len(q) >= cap(q) {
		http.Error(w, "queue full, try again shortly", http.StatusServiceUnavailable)
		return
	}
	tl, msg := s.store.AddUserTo(targetThread, text, files)
	msgID := s.store.ReserveMsgID()
	s.broadcast("msg", map[string]any{"thread": tl.ID, "title": tl.Title, "message": msg})
	select {
	case q <- Job{Thread: tl.ID, MsgID: msgID, Query: composeQuery(tl.ID, text, files)}:
	default:
		http.Error(w, "queue full, try again shortly", http.StatusServiceUnavailable)
		return
	}
	writeJSON(w, map[string]any{"ok": true, "thread": tl.ID})
}

func (s *Server) handleStop(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Thread string `json:"thread"`
	}
	_ = json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req)

	s.mu.Lock()
	var c context.CancelFunc
	if req.Thread != "" {
		if ar, ok := s.activeRuns[req.Thread]; ok {
			c = ar.Cancel
		}
	} else {
		for _, ar := range s.activeRuns {
			c = ar.Cancel
			break
		}
		if c == nil {
			c = s.cancel
		}
	}
	s.mu.Unlock()
	if c != nil {
		c()
		writeJSON(w, map[string]any{"stopped": true})
		return
	}
	writeJSON(w, map[string]any{"stopped": false})
}

func sanitizeName(name string) string {
	name = filepath.Base(name)
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	out := strings.Trim(b.String(), ".")
	if len(out) > 80 {
		out = out[len(out)-80:]
	}
	if out == "" {
		out = "file"
	}
	return out
}

func (s *Server) handleUpload(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 60<<20)
	mr, err := r.MultipartReader()
	if err != nil {
		http.Error(w, "expected multipart form", http.StatusBadRequest)
		return
	}
	var out []Attachment
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if part.FormName() != "file" {
			continue
		}
		name := sanitizeName(part.FileName())
		dst := filepath.Join(s.uploadDir, fmt.Sprintf("%d_%s", time.Now().UnixNano(), name))
		// #nosec G304 — dst is uploadDir + a timestamp + sanitizeName(), which
		// strips everything outside [A-Za-z0-9._-] and trims edge dots, so no
		// separators or traversal sequences can appear in the file name.
		f, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY, 0600)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		n, err := io.Copy(f, io.LimitReader(part, 50<<20))
		_ = f.Close()
		if err != nil {
			_ = os.Remove(dst)
			http.Error(w, "upload failed: "+err.Error(), http.StatusInternalServerError)
			return
		}
		if n >= 50<<20 {
			_ = os.Remove(dst)
			http.Error(w, "file too large (50MB limit)", http.StatusRequestEntityTooLarge)
			return
		}
		out = append(out, Attachment{Path: dst, Name: name, Size: n, Kind: kindForFile(dst)})
	}
	writeJSON(w, map[string]any{"files": out})
}

func serveRootDirs() []string {
	return []string{homeDir, "/mnt/data", "/tmp"}
}

func (s *Server) isAllowedFile(cleanPath string) bool {
	if s.uploadDir != "" && strings.HasPrefix(cleanPath, filepath.Clean(s.uploadDir)+string(os.PathSeparator)) {
		return true
	}
	return s.store.HasAttachment(cleanPath)
}

func (s *Server) reapUploadsOnce(maxAge time.Duration) int {
	if s.uploadDir == "" {
		return 0
	}
	entries, err := os.ReadDir(s.uploadDir)
	if err != nil {
		return 0
	}
	refs := s.store.ReferencedPaths()
	cutoff := time.Now().Add(-maxAge)
	removed := 0
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		p := filepath.Join(s.uploadDir, e.Name())
		if refs[filepath.Clean(p)] {
			continue
		}
		fi, err := e.Info()
		if err != nil || !fi.ModTime().Before(cutoff) {
			continue
		}
		if os.Remove(p) == nil {
			removed++
		}
	}
	return removed
}

func (s *Server) reapUploads(ctx context.Context) {
	t := time.NewTicker(1 * time.Hour)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if n := s.reapUploadsOnce(24 * time.Hour); n > 0 {
				log.Printf("sentinel: reaped %d orphaned upload(s)", n)
			}
		}
	}
}

func (s *Server) handleExport(w http.ResponseWriter, r *http.Request) {
	tid := r.URL.Query().Get("thread")
	format := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("format")))
	if format == "" {
		format = "md"
	}
	thr, ok := s.store.ThreadByID(tid)
	if !ok {
		http.Error(w, "no such thread", http.StatusNotFound)
		return
	}
	base := sanitizeName(thr.Title)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	switch format {
	case "json":
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": base + ".json"}))
		_ = json.NewEncoder(w).Encode(thr)
	case "txt":
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": base + ".txt"}))
		var b strings.Builder
		fmt.Fprintf(&b, "%s\n%s\n\n", thr.Title, strings.Repeat("=", len(thr.Title)))
		for _, m := range thr.Messages {
			ts := time.Unix(m.Time, 0).Format("2006-01-02 15:04")
			fmt.Fprintf(&b, "[%s] %s:\n%s\n", ts, strings.ToUpper(m.Role), m.Text)
			for _, f := range m.Files {
				fmt.Fprintf(&b, "  [attachment: %s]\n", f.Name)
			}
			b.WriteByte('\n')
		}
		_, _ = io.WriteString(w, b.String())
	case "md":
		w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
		w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": base + ".md"}))
		var b strings.Builder
		fmt.Fprintf(&b, "# %s\n\n", thr.Title)
		for _, m := range thr.Messages {
			ts := time.Unix(m.Time, 0).Format("2006-01-02 15:04")
			fmt.Fprintf(&b, "### %s (%s)\n\n%s\n\n", strings.ToUpper(m.Role), ts, m.Text)
			for _, f := range m.Files {
				fmt.Fprintf(&b, "- Attachment: `%s`\n", f.Name)
			}
			if len(m.Files) > 0 {
				b.WriteByte('\n')
			}
		}
		_, _ = io.WriteString(w, b.String())
	default:
		http.Error(w, "unsupported format (use md, json, or txt)", http.StatusBadRequest)
	}
}

func (s *Server) handleFile(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Query().Get("path")
	if p == "" {
		http.Error(w, "missing path", http.StatusBadRequest)
		return
	}
	if strings.HasPrefix(p, "~/") {
		p = filepath.Join(homeDir, p[2:])
	}
	p = filepath.Clean(p)
	if !s.isAllowedFile(p) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	// Serve only from fixed roots. os.Root enforces containment at the
	// kernel level: neither ".." nor symlinks inside the root can escape it.
	for _, rootDir := range serveRootDirs() {
		if !strings.HasPrefix(p, rootDir+string(os.PathSeparator)) {
			continue
		}
		rel := p[len(rootDir)+1:]
		root, err := os.OpenRoot(rootDir)
		if err != nil {
			continue
		}
		fi, err := root.Stat(rel)
		if err != nil || !fi.Mode().IsRegular() {
			_ = root.Close()
			continue
		}
		if fi.Size() > 500<<20 {
			_ = root.Close()
			http.Error(w, "file too large to serve", http.StatusRequestEntityTooLarge)
			return
		}
		f, err := root.Open(rel)
		if err != nil {
			_ = root.Close()
			continue
		}
		ct := mime.TypeByExtension(strings.ToLower(filepath.Ext(p)))
		if ct == "" {
			buf := make([]byte, 512)
			n, _ := f.Read(buf)
			_, _ = f.Seek(0, 0)
			ct = http.DetectContentType(buf[:n])
		}
		disp := "attachment"
		switch {
		case strings.HasPrefix(ct, "image/"), strings.HasPrefix(ct, "video/"), strings.HasPrefix(ct, "audio/"), ct == "application/pdf":
			disp = "inline"
		}
		w.Header().Set("Content-Type", ct)
		w.Header().Set("Content-Disposition", mime.FormatMediaType(disp, map[string]string{"filename": filepath.Base(p)}))
		w.Header().Set("X-Content-Type-Options", "nosniff")
		http.ServeContent(w, r, filepath.Base(p), fi.ModTime(), f)
		_ = f.Close()
		_ = root.Close()
		return
	}
	http.Error(w, "not found", http.StatusNotFound)
}

// ---------- main ----------

func main() {
	addr := envOr("WEBCCHAT_ADDR", "127.0.0.1") + ":" + envOr("WEBCCHAT_PORT", "9121")
	dataDir = envOr("WEBCCHAT_DATA", filepath.Join(homeDir, "projects", "hermes-webchat", "data"))
	team := strings.TrimSpace(os.Getenv("CF_ACCESS_TEAM"))
	aud := strings.TrimSpace(os.Getenv("CF_AUD"))
	dev := os.Getenv("WEBCCHAT_DEV") == "1"
	if !dev && (team == "" || aud == "" || aud == "-") {
		log.Fatal("sentinel: CF_ACCESS_TEAM and CF_AUD must be set (or set WEBCCHAT_DEV=1 for local dev)")
	}
	allowedEmails := parseAllowedEmails(os.Getenv("ALLOWED_EMAILS"))

	uploadDir := filepath.Join(dataDir, "uploads")
	for _, d := range []string{dataDir, uploadDir} {
		if err := os.MkdirAll(d, 0700); err != nil {
			log.Fatal(err)
		}
	}
	store := NewStore(dataDir)
	if len(store.Snapshot().Threads) == 0 {
		store.NewThread()
	}

	injectToken, err := loadInjectToken()
	if err != nil {
		// Keep serving chat; only gateway delivery is lost.
		log.Printf("sentinel: inject token unavailable, POST /api/inject disabled: %v", err)
		injectToken = ""
	}

	shutdownCh := make(chan struct{})
	srv := &Server{
		store:         store,
		hub:           NewHub(),
		auth:          NewAccessAuth(team, aud),
		allowedEmails: allowedEmails,
		dev:           dev,
		uploadDir:     uploadDir,
		injectToken:   injectToken,
		notifier:      newNotifier(),
		jobs:          make(chan Job, 16),
		threadQueues:  map[string]chan Job{},
		activeRuns:    map[string]*ThreadRun{},
		shutdownCh:    shutdownCh,
	}
	sigCtx, stopSig := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSig()
	go srv.reapUploads(sigCtx)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
	mux.HandleFunc("GET /{$}", srv.authed(srv.handleIndex))
	mux.HandleFunc("GET /manifest.webmanifest", srv.serveWebAsset)
	mux.HandleFunc("GET /sw.js", srv.serveWebAsset)
	mux.HandleFunc("GET /icon-192.png", srv.serveWebAsset)
	mux.HandleFunc("GET /icon-512.png", srv.serveWebAsset)
	mux.HandleFunc("GET /icon-512-maskable.png", srv.serveWebAsset)
	mux.HandleFunc("GET /apple-touch-icon.png", srv.serveWebAsset)
	mux.HandleFunc("GET /api/state", srv.authed(srv.handleState))
	mux.HandleFunc("GET /api/me", srv.authed(srv.handleMe))
	mux.HandleFunc("GET /api/changelog", srv.authed(srv.handleChangelog))
	mux.HandleFunc("GET /api/commands", srv.authed(srv.handleCommands))
	mux.HandleFunc("GET /api/models", srv.authed(srv.handleModels))
	mux.HandleFunc("GET /api/search", srv.authed(srv.handleSearch))
	mux.HandleFunc("GET /api/export", srv.authed(srv.handleExport))
	mux.HandleFunc("GET /api/events", srv.authed(srv.handleEvents))
	mux.HandleFunc("POST /api/new", srv.authed(srv.handleNew))
	mux.HandleFunc("POST /api/active", srv.authed(srv.handleActive))
	mux.HandleFunc("POST /api/thread/rename", srv.authed(srv.handleThreadRename))
	mux.HandleFunc("POST /api/thread/pin", srv.authed(srv.handleThreadPin))
	mux.HandleFunc("POST /api/thread/archive", srv.authed(srv.handleThreadArchive))
	mux.HandleFunc("POST /api/thread/delete", srv.authed(srv.handleThreadDelete))
	mux.HandleFunc("POST /api/thread/clear", srv.authed(srv.handleThreadClear))
	mux.HandleFunc("POST /api/thread/model", srv.authed(srv.handleThreadModel))
	mux.HandleFunc("POST /api/send", srv.authed(srv.handleSend))
	mux.HandleFunc("POST /api/stop", srv.authed(srv.handleStop))
	mux.HandleFunc("POST /api/update", srv.authed(srv.handleUpdate))
	mux.HandleFunc("POST /api/upload", srv.authed(srv.handleUpload))
	mux.HandleFunc("GET /api/file", srv.authed(srv.handleFile))

	// Gateway delivery: loopback + shared secret, deliberately outside Access.
	mux.HandleFunc("POST /api/inject", srv.injectGuard(srv.handleInject))

	hs := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	// Closing this makes every SSE handler return, so Shutdown does not sit
	// out its whole grace period waiting on the tunnel's open event streams.
	hs.RegisterOnShutdown(func() { close(shutdownCh) })
	// #nosec G706 — addr/team/aud come from this service's own systemd
	// Environment= lines, not from requests; they are control-stripped anyway.
	log.Printf("sentinel listening on http://%s (dev=%v, access team=%s, aud=%s)",
		sanitizeLog(addr), dev, sanitizeLog(team), sanitizeLog(aud))
	errCh := make(chan error, 1)
	go func() {
		if err := hs.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errCh <- err
		}
		close(errCh)
	}()
	select {
	case err := <-errCh:
		if err != nil {
			log.Fatal(err)
		}
	case <-sigCtx.Done():
		log.Printf("sentinel shutting down...")
		srv.cancelAllRuns()
		shutCtx, shutCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer shutCancel()
		if err := hs.Shutdown(shutCtx); err != nil {
			log.Printf("sentinel shutdown error: %v", err)
		}
	}
}
