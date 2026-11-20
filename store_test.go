package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestStoreThreadManagementAndSearch(t *testing.T) {
	dir := t.TempDir()
	st := NewStore(dir)

	t1 := st.NewThread()
	st.AddUser("deploying sentinel to cloudflare tunnel", nil)
	id1 := st.ReserveMsgID()
	st.FinalizeAssistant(t1.ID, id1, "Tunnel configured cleanly.", nil, []Activity{
		{ID: "a1", Kind: "tool", Name: "bash", Status: "done", DurationMs: 250},
	}, "sess-1")

	t2 := st.NewThread()
	st.AddUser("grocery list", nil)

	if !st.RenameThread(t2.ID, "Shopping") {
		t.Fatal("RenameThread failed")
	}
	if !st.SetPinned(t1.ID, true) {
		t.Fatal("SetPinned failed")
	}

	hits := st.Search("cloudflare", 10)
	if len(hits) != 1 || hits[0].ThreadID != t1.ID || hits[0].MsgID == 0 {
		t.Fatalf("unexpected message search hits: %+v", hits)
	}

	titleHits := st.Search("shopping", 10)
	if len(titleHits) != 1 || titleHits[0].ThreadID != t2.ID || titleHits[0].MsgID != 0 {
		t.Fatalf("unexpected title search hits: %+v", titleHits)
	}

	if !st.SetArchived(t1.ID, true) {
		t.Fatal("SetArchived failed")
	}
	snap := st.Snapshot()
	for _, th := range snap.Threads {
		if th.ID == t1.ID && (!th.Archived || th.Pinned) {
			t.Fatalf("archived thread should have Archived=true and Pinned=false: %+v", th)
		}
	}

	if !st.ClearThread(t2.ID) {
		t.Fatal("ClearThread failed")
	}
	if got := len(st.Snapshot().Messages); got != 0 {
		t.Fatalf("expected 0 messages after ClearThread, got %d", got)
	}

	if !st.DeleteThread(t1.ID) || !st.DeleteThread(t2.ID) {
		t.Fatal("DeleteThread failed")
	}
	// Deleting the last thread must auto-create a replacement active thread.
	if got := len(st.Snapshot().Threads); got != 1 {
		t.Fatalf("expected 1 fallback thread after deleting all, got %d", got)
	}
}

func TestHandlersCommandsSearchAndThreads(t *testing.T) {
	dir := t.TempDir()
	st := NewStore(dir)
	thr := st.NewThread()
	st.AddUser("hello search needle", nil)

	srv := &Server{
		store:     st,
		hub:       NewHub(),
		dev:       true,
		uploadDir: dir,
		jobs:      make(chan Job, 4),
	}

	// /api/commands
	req := httptest.NewRequest(http.MethodGet, "/api/commands", nil)
	rec := httptest.NewRecorder()
	srv.handleCommands(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("commands status = %d", rec.Code)
	}
	var cmdResp struct {
		Commands []CommandSpec `json:"commands"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &cmdResp); err != nil || len(cmdResp.Commands) == 0 {
		t.Fatalf("unexpected commands response: %v %s", err, rec.Body.String())
	}

	// /api/search
	req = httptest.NewRequest(http.MethodGet, "/api/search?q=needle", nil)
	rec = httptest.NewRecorder()
	srv.handleSearch(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("search status = %d", rec.Code)
	}
	var searchResp struct {
		Hits []SearchHit `json:"hits"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &searchResp); err != nil || len(searchResp.Hits) != 1 {
		t.Fatalf("unexpected search response: %v %s", err, rec.Body.String())
	}

	// /api/thread/rename
	body, _ := json.Marshal(map[string]any{"id": thr.ID, "title": "Renamed"})
	req = httptest.NewRequest(http.MethodPost, "/api/thread/rename", bytes.NewReader(body))
	rec = httptest.NewRecorder()
	srv.handleThreadRename(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("rename status = %d: %s", rec.Code, rec.Body.String())
	}
	if st.Snapshot().Threads[0].Title != "Renamed" {
		t.Fatalf("expected Renamed title, got %q", st.Snapshot().Threads[0].Title)
	}
}

func TestPWAInstallAssets(t *testing.T) {
	srv := &Server{dev: false}
	for _, tc := range []struct {
		path string
		want string
	}{
		{"/manifest.webmanifest", "application/manifest+json"},
		{"/sw.js", "text/javascript"},
		{"/icon-192.png", "image/png"},
		{"/icon-512.png", "image/png"},
	} {
		req := httptest.NewRequest(http.MethodGet, tc.path, nil)
		rec := httptest.NewRecorder()
		srv.serveWebAsset(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s returned %d", tc.path, rec.Code)
		}
		if ct := rec.Header().Get("Content-Type"); !bytes.Contains([]byte(ct), []byte(tc.want)) {
			t.Fatalf("%s Content-Type = %q, want %q", tc.path, ct, tc.want)
		}
	}
}

func TestUpdateCommandRegistered(t *testing.T) {
	found := false
	for _, c := range commandRegistry {
		if c.Name == "/update" {
			found = true
			if c.TakesArgs {
				t.Fatal("/update takes no arguments")
			}
		}
	}
	if !found {
		t.Fatal("/update missing from commandRegistry")
	}
}

func TestUpdateRefusedWhenBusy(t *testing.T) {
	dir := t.TempDir()
	st := NewStore(dir)
	srv := &Server{store: st, hub: NewHub(), dev: true, uploadDir: dir, jobs: make(chan Job, 4)}
	srv.running = true
	srv.runningThread = "t1"

	rec := httptest.NewRecorder()
	srv.handleUpdate(rec, httptest.NewRequest(http.MethodPost, "/api/update", nil))

	var rep updateReport
	if err := json.Unmarshal(rec.Body.Bytes(), &rep); err != nil {
		t.Fatalf("bad response: %v %s", err, rec.Body.String())
	}
	if rep.OK || rep.Restarted || rep.Updated {
		t.Fatalf("busy server must not update, got %+v", rep)
	}
	if !bytes.Contains([]byte(rep.Message), []byte("mid-reply")) {
		t.Fatalf("expected a mid-reply refusal, got %q", rep.Message)
	}
	if msgs := st.Snapshot().Messages; len(msgs) != 0 {
		t.Fatalf("refusal should not touch the transcript, got %d messages", len(msgs))
	}
}

func TestUpdateRefusedWhenAlreadyRunning(t *testing.T) {
	dir := t.TempDir()
	st := NewStore(dir)
	srv := &Server{store: st, hub: NewHub(), dev: true, uploadDir: dir, jobs: make(chan Job, 4)}
	updateInFlight.Store(true)
	defer updateInFlight.Store(false)

	rec := httptest.NewRecorder()
	srv.handleUpdate(rec, httptest.NewRequest(http.MethodPost, "/api/update", nil))

	var rep updateReport
	if err := json.Unmarshal(rec.Body.Bytes(), &rep); err != nil {
		t.Fatalf("bad response: %v %s", err, rec.Body.String())
	}
	if rep.OK {
		t.Fatalf("concurrent update must be refused, got %+v", rep)
	}
	if !bytes.Contains([]byte(rep.Message), []byte("already running")) {
		t.Fatalf("expected an already-running refusal, got %q", rep.Message)
	}
	if !updateInFlight.Load() {
		t.Fatal("the in-flight guard belongs to the original update and must stay set")
	}
}

// Outside a real checkout the handler must fail closed and say so, without
// touching the running binary.
func TestUpdateOutsideCheckoutFailsClosed(t *testing.T) {
	if _, err := updateRepoDir(); err == nil {
		t.Skip("test binary lives inside a git checkout; nothing to assert")
	}
	dir := t.TempDir()
	st := NewStore(dir)
	srv := &Server{store: st, hub: NewHub(), dev: true, uploadDir: dir, jobs: make(chan Job, 4)}

	rec := httptest.NewRecorder()
	srv.handleUpdate(rec, httptest.NewRequest(http.MethodPost, "/api/update", nil))

	var rep updateReport
	if err := json.Unmarshal(rec.Body.Bytes(), &rep); err != nil {
		t.Fatalf("bad response: %v %s", err, rec.Body.String())
	}
	if rep.OK || rep.Restarted {
		t.Fatalf("expected a refusal outside a checkout, got %+v", rep)
	}
	if !bytes.Contains([]byte(rep.Message), []byte("no git checkout")) {
		t.Fatalf("unexpected message %q", rep.Message)
	}
	msgs := st.Snapshot().Messages
	if len(msgs) != 1 || msgs[0].Role != "error" {
		t.Fatalf("expected one error notice in the transcript, got %+v", msgs)
	}
}

func TestAddNoticeRoles(t *testing.T) {
	dir := t.TempDir()
	st := NewStore(dir)
	st.NewThread()
	st.AddUser("hello", nil)
	st.AddNotice(st.Snapshot().Active, "assistant", "Updated 7dae566 → 0ccc8b8.")
	st.AddError(st.Snapshot().Active, "Update failed: no git checkout")

	msgs := st.Snapshot().Messages
	if len(msgs) != 3 {
		t.Fatalf("expected 3 messages, got %d", len(msgs))
	}
	if msgs[1].Role != "assistant" || msgs[2].Role != "error" {
		t.Fatalf("unexpected roles %q/%q", msgs[1].Role, msgs[2].Role)
	}
}
