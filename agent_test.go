package main

import (
	"context"
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
