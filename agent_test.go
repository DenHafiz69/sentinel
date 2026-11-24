package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestRunHermesStopsAtDeadline proves the run limit actually kills the child and
// returns the message the chat UI shows. A fake `hermes` script sleeps far longer
// than the parent deadline, so the test finishes in well under a second.
func TestRunHermesStopsAtDeadline(t *testing.T) {
	oldHome, oldBin, oldData := homeDir, homeBin, dataDir
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(bin, "hermes")
	if err := os.WriteFile(script, []byte("#!/bin/sh\n/bin/sleep 30\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	homeDir, homeBin, dataDir = dir, bin, dir
	t.Cleanup(func() { homeDir, homeBin, dataDir = oldHome, oldBin, oldData })

	// Keep LookPath from finding a real hermes first.
	t.Setenv("PATH", "")

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	start := time.Now()
	res := runHermes(ctx, "sess", "", "hello", nil, nil)
	elapsed := time.Since(start)

	if res.Err == nil {
		t.Fatal("expected a timeout error, got none")
	}
	if !strings.Contains(res.Err.Error(), "limit and was stopped") {
		t.Fatalf("unexpected error: %v", res.Err)
	}
	if elapsed > 12*time.Second {
		t.Fatalf("child was not killed promptly: %s", elapsed)
	}
}

func TestRunTimeoutMessageNamesTheLimit(t *testing.T) {
	t.Setenv("SENTINEL_RUN_TIMEOUT", "10m")
	if got := runTimeout(); got != 10*time.Minute {
		t.Fatalf("runTimeout = %s", got)
	}
}

func TestFmtLimit(t *testing.T) {
	cases := map[time.Duration]string{
		90 * time.Minute:        "90m",
		2 * time.Hour:           "2h",
		45 * time.Second:        "45s",
		1500 * time.Millisecond: "1.5s",
	}
	for in, want := range cases {
		if got := fmtLimit(in); got != want {
			t.Errorf("fmtLimit(%s) = %q, want %q", in, got, want)
		}
	}
}

// TestRunJobKeepsPartialTextOnTimeout is the regression test for the failure
// Hafiz kept hitting: a run that hit the limit threw away everything the agent
// had already written and left only "Agent run failed: ...". The text must
// survive, marked as ended early, with the error below it.
func TestRunJobKeepsPartialTextOnTimeout(t *testing.T) {
	oldHome, oldBin, oldData, oldLimit := homeDir, homeBin, dataDir, runLimit
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(bin, "hermes")
	body := "#!/bin/sh\nprintf '{\"type\":\"text\",\"text\":\"Half a report.\"}\\n'\n/bin/sleep 30\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	homeDir, homeBin, dataDir = dir, bin, dir
	runLimit = func() time.Duration { return 500 * time.Millisecond }
	t.Cleanup(func() { homeDir, homeBin, dataDir, runLimit = oldHome, oldBin, oldData, oldLimit })
	t.Setenv("PATH", "")

	st := NewStore(dir)
	srv := &Server{
		store:        st,
		hub:          NewHub(),
		activeRuns:   map[string]*ThreadRun{},
		threadQueues: map[string]chan Job{},
	}
	th := st.NewThread()
	st.AddUser("write the notes", nil)
	id := st.ReserveMsgID()

	start := time.Now()
	srv.runJob(Job{Thread: th.ID, MsgID: id, Query: "write the notes"})
	if elapsed := time.Since(start); elapsed > 12*time.Second {
		t.Fatalf("run was not cut off promptly: %s", elapsed)
	}

	got, ok := st.ThreadByID(th.ID)
	if !ok {
		t.Fatal("thread vanished")
	}
	var partial, failure string
	for _, m := range got.Messages {
		switch m.Role {
		case "assistant":
			partial = m.Text
		case "error":
			failure = m.Text
		}
	}
	if !strings.Contains(partial, "Half a report.") {
		t.Fatalf("partial reply was discarded, got %q", partial)
	}
	if !strings.Contains(partial, "run ended early") {
		t.Errorf("partial reply is not marked as ended early: %q", partial)
	}
	if !strings.Contains(failure, "limit and was stopped") {
		t.Errorf("failure message = %q", failure)
	}
	if !strings.Contains(failure, "SENTINEL_RUN_TIMEOUT") {
		t.Errorf("failure message should say how to raise the limit: %q", failure)
	}
}

// TestRunJobCoalescesDeltas proves streaming sends a few cumulative events
// instead of one event per token: 60 rapid tokens must produce a handful of
// "delta" frames, each carrying the full text so far (a prefix of the final
// reply), so a client that drops one is healed by the next.
func TestRunJobCoalescesDeltas(t *testing.T) {
	oldHome, oldBin, oldData := homeDir, homeBin, dataDir
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(bin, "hermes")
	body := "#!/bin/sh\ni=0\nwhile [ $i -lt 60 ]; do printf '{\"type\":\"text\",\"text\":\"word%d \"}\\n' \"$i\"; i=$((i+1)); done\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	homeDir, homeBin, dataDir = dir, bin, dir
	t.Cleanup(func() { homeDir, homeBin, dataDir = oldHome, oldBin, oldData })
	t.Setenv("PATH", "")

	st := NewStore(dir)
	srv := &Server{
		store:        st,
		hub:          NewHub(),
		activeRuns:   map[string]*ThreadRun{},
		threadQueues: map[string]chan Job{},
	}
	ch := srv.hub.Add()
	defer srv.hub.Remove(ch)
	th := st.NewThread()
	st.AddUser("go", nil)
	id := st.ReserveMsgID()

	srv.runJob(Job{Thread: th.ID, MsgID: id, Query: "go"})

	var frames []string
	for {
		select {
		case b := <-ch:
			frames = append(frames, string(b))
			continue
		default:
		}
		break
	}

	got, ok := st.ThreadByID(th.ID)
	if !ok {
		t.Fatal("thread vanished")
	}
	full := ""
	for _, m := range got.Messages {
		if m.Role == "assistant" {
			full = m.Text
		}
	}
	if !strings.Contains(full, "word0 ") || !strings.Contains(full, "word59 ") {
		t.Fatalf("reply lost tokens, got %q", full)
	}

	deltaCount := 0
	for _, f := range frames {
		if !strings.HasPrefix(f, "event: delta\n") {
			continue
		}
		deltaCount++
		var d struct {
			Text string `json:"text"`
		}
		data := strings.TrimSuffix(strings.TrimPrefix(f, "event: delta\ndata: "), "\n\n")
		if err := json.Unmarshal([]byte(data), &d); err != nil {
			t.Fatalf("bad delta frame %q: %v", f, err)
		}
		if !strings.HasPrefix(full, d.Text) {
			t.Errorf("delta %d is not cumulative text-so-far: %q", deltaCount, d.Text)
		}
	}
	if deltaCount < 1 {
		t.Fatal("no delta events at all — streaming broke")
	}
	if deltaCount > 6 {
		t.Fatalf("%d delta events for 60 tokens — coalescing is off", deltaCount)
	}
}
