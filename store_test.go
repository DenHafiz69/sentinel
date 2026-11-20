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
