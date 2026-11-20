package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExtractAttachments(t *testing.T) {
	dir := t.TempDir()
	png := filepath.Join(dir, "chart.png")
	pdf := filepath.Join(dir, "report.pdf")
	goFile := filepath.Join(dir, "main.go")
	for _, p := range []string{png, pdf, goFile} {
		if err := os.WriteFile(p, []byte("x"), 0600); err != nil {
			t.Fatal(err)
		}
	}

	text := "Here is your chart: " + png + "\n" +
		"Full report at " + pdf + ".\n" +
		"Source in code block should NOT attach:\n" +
		"```go\n" + goFile + "\n// " + png + " inside fence\n```\n" +
		"Inline code `~/nope.md` ignored too, and missing file " + filepath.Join(dir, "ghost.png") + " ignored."

	got := extractAttachments(text)
	if len(got) != 2 {
		t.Fatalf("want 2 attachments, got %d: %+v", len(got), got)
	}
	if got[0].Path != png || got[0].Kind != "image" {
		t.Errorf("first attachment wrong: %+v", got[0])
	}
	if got[1].Path != pdf || got[1].Kind != "file" {
		t.Errorf("second attachment wrong: %+v", got[1])
	}
}

func TestExtractTildeExpansion(t *testing.T) {
	dir := t.TempDir()
	// Pretend homeDir is the temp dir for this test.
	old := homeDir
	homeDir = dir
	defer func() { homeDir = old }()

	p := filepath.Join(dir, "pic.jpg")
	os.WriteFile(p, []byte("x"), 0600)
	got := extractAttachments("see ~/pic.jpg please")
	if len(got) != 1 || got[0].Path != p || got[0].Kind != "image" {
		t.Fatalf("tilde expansion failed: %+v", got)
	}
}

func TestStripCodeBlocks(t *testing.T) {
	in := "a `code` span\n```go\nfenced /home/x/y.txt\n```\ntail"
	out := stripCodeBlocks(in)
	if strings.Contains(out, "fenced") || strings.Contains(out, "code") || !strings.Contains(out, "tail") {
		t.Fatalf("unexpected strip result: %q", out)
	}
}
