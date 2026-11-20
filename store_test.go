package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
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

func TestHandlerChangelog(t *testing.T) {
	srv := &Server{dev: true}
	req := httptest.NewRequest(http.MethodGet, "/api/changelog", nil)
	rec := httptest.NewRecorder()
	srv.handleChangelog(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("changelog status = %d", rec.Code)
	}
	var resp struct {
		Version   string           `json:"version"`
		Changelog []ChangelogEntry `json:"changelog"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal changelog: %v", err)
	}
	if resp.Version == "" || len(resp.Changelog) == 0 || resp.Changelog[0].Version != resp.Version {
		t.Fatalf("unexpected changelog payload: %+v", resp)
	}
}

func TestModelSelection(t *testing.T) {
	dir := t.TempDir()
	st := NewStore(dir)
	thr := st.NewThread()

	if st.SetModel(thr.ID, "--bad-flag") || st.SetModel(thr.ID, "has space") {
		t.Fatal("expected invalid model names to be rejected")
	}
	if !st.SetModel(thr.ID, "anthropic/claude-sonnet-4") {
		t.Fatal("expected valid model name to succeed")
	}
	if got := st.Model(thr.ID); got != "anthropic/claude-sonnet-4" {
		t.Fatalf("Model = %q, want anthropic/claude-sonnet-4", got)
	}
	if used := st.UsedModels(); len(used) != 1 || used[0] != "anthropic/claude-sonnet-4" {
		t.Fatalf("unexpected UsedModels: %+v", used)
	}

	srv := &Server{store: st, hub: NewHub(), dev: true, uploadDir: dir, jobs: make(chan Job, 4)}
	t.Setenv("SENTINEL_MODELS", "openai/gpt-5,google/gemini-2.5-pro")

	rec := httptest.NewRecorder()
	srv.handleModels(rec, httptest.NewRequest(http.MethodGet, "/api/models", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("models status = %d", rec.Code)
	}
	var mresp struct {
		Current string   `json:"current"`
		Models  []string `json:"models"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &mresp); err != nil {
		t.Fatalf("unmarshal models: %v", err)
	}
	if mresp.Current != "anthropic/claude-sonnet-4" || len(mresp.Models) < 3 {
		t.Fatalf("unexpected models response: %+v", mresp)
	}

	body, _ := json.Marshal(map[string]any{"id": thr.ID, "model": "default"})
	rec = httptest.NewRecorder()
	srv.handleThreadModel(rec, httptest.NewRequest(http.MethodPost, "/api/thread/model", bytes.NewReader(body)))
	if rec.Code != http.StatusOK || st.Model(thr.ID) != "" {
		t.Fatalf("expected reset to default, status=%d model=%q", rec.Code, st.Model(thr.ID))
	}
}

func TestClientThreadFileScopeReaperAndExport(t *testing.T) {
	dir := t.TempDir()
	uploadDir := filepath.Join(dir, "uploads")
	if err := os.MkdirAll(uploadDir, 0700); err != nil {
		t.Fatal(err)
	}
	st := NewStore(dir)
	t1 := st.NewThread()
	t2 := st.NewThread() // t2 is now global active

	refFile := filepath.Join(uploadDir, "kept.txt")
	orphanFile := filepath.Join(uploadDir, "orphan.txt")
	secretFile := filepath.Join(dir, "secret.txt")
	for _, p := range []string{refFile, orphanFile, secretFile} {
		if err := os.WriteFile(p, []byte("data"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	oldTime := time.Now().Add(-48 * time.Hour)
	_ = os.Chtimes(orphanFile, oldTime, oldTime)

	st.AddUserTo(t1.ID, "message on t1", []Attachment{{Path: refFile, Name: "kept.txt", Size: 4, Kind: "file"}})

	snap1 := st.SnapshotFor(t1.ID)
	if snap1.Active != t1.ID || len(snap1.Messages) != 1 || snap1.Messages[0].Text != "message on t1" {
		t.Fatalf("unexpected SnapshotFor(t1): %+v", snap1)
	}
	if st.Snapshot().Active != t2.ID {
		t.Fatalf("SnapshotFor should not mutate global active %q", t2.ID)
	}

	srv := &Server{store: st, hub: NewHub(), dev: true, uploadDir: uploadDir, jobs: make(chan Job, 4)}

	// File scope: unreferenced file outside uploadDir must be denied (404)
	rec := httptest.NewRecorder()
	srv.handleFile(rec, httptest.NewRequest(http.MethodGet, "/api/file?path="+secretFile, nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for unattached file outside uploadDir, got %d", rec.Code)
	}

	// Upload reaper: removes old orphan, keeps referenced file
	if removed := srv.reapUploadsOnce(24 * time.Hour); removed != 1 {
		t.Fatalf("expected 1 reaped upload, got %d", removed)
	}
	if _, err := os.Stat(orphanFile); !os.IsNotExist(err) {
		t.Fatalf("expected orphanFile to be removed")
	}
	if _, err := os.Stat(refFile); err != nil {
		t.Fatalf("expected refFile to be kept: %v", err)
	}

	// Export: md and json
	for _, fmtName := range []string{"md", "json", "txt"} {
		rec = httptest.NewRecorder()
		srv.handleExport(rec, httptest.NewRequest(http.MethodGet, "/api/export?thread="+t1.ID+"&format="+fmtName, nil))
		if rec.Code != http.StatusOK || !bytes.Contains(rec.Body.Bytes(), []byte("message on t1")) {
			t.Fatalf("export %s failed: code=%d body=%q", fmtName, rec.Code, rec.Body.String())
		}
	}
}
