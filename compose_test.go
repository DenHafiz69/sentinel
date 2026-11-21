package main

import (
	"strings"
	"testing"
	"time"
)

// The house-rules note rides on every composed query: it is what bounds a
// chat run from the inside, so a missing note is a behaviour regression, not
// a formatting nit.
func TestComposeQueryCarriesHouseRules(t *testing.T) {
	orig := runLimit
	runLimit = func() time.Duration { return 10 * time.Minute }
	defer func() { runLimit = orig }()

	q := composeQuery("t3", "check the disk", nil)
	if !strings.Contains(q, "check the disk") {
		t.Fatalf("user text missing: %q", q)
	}
	if !strings.Contains(q, "sentinel:t3") {
		t.Fatalf("thread delivery target missing: %q", q)
	}
	if !strings.Contains(q, "capped at 10m") {
		t.Fatalf("run cap missing from note: %q", q)
	}
	if !strings.Contains(q, "background") {
		t.Fatalf("background deferral missing: %q", q)
	}
}

// Attachments keep their guidance lines and still get the note.
func TestComposeQueryWithFilesKeepsNote(t *testing.T) {
	orig := runLimit
	runLimit = func() time.Duration { return 45 * time.Minute }
	defer func() { runLimit = orig }()

	q := composeQuery("t7", "look at these", []Attachment{
		{Kind: "image", Path: "/tmp/shot.png", Name: "shot.png"},
		{Kind: "file", Path: "/tmp/report.md", Name: "report.md"},
	})
	for _, want := range []string{
		"vision_analyze", "/tmp/shot.png", "/tmp/report.md",
		"sentinel:t7", "capped at 45m",
	} {
		if !strings.Contains(q, want) {
			t.Fatalf("expected %q in query, got: %q", want, q)
		}
	}

	// A file-only message (no text) still carries the note.
	q = composeQuery("t7", "", []Attachment{{Kind: "file", Path: "/tmp/a.txt", Name: "a.txt"}})
	if !strings.Contains(q, "sentinel:t7") {
		t.Fatalf("note missing on text-less message: %q", q)
	}
}
