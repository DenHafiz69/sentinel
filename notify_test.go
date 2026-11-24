package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestReadEnvAllowlist(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	body := strings.Join([]string{
		"# comment",
		"OTHER_SECRET=must-not-be-read",
		`TELEGRAM_BOT_TOKEN="123:abc"`,
		"export TELEGRAM_HOME_CHANNEL=244300056",
		"EMPTY=",
	}, "\n")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	got := readEnvAllowlist(path, alertEnvKeys)
	if got["TELEGRAM_BOT_TOKEN"] != "123:abc" {
		t.Fatalf("token = %q, want the unquoted value", got["TELEGRAM_BOT_TOKEN"])
	}
	if got["TELEGRAM_HOME_CHANNEL"] != "244300056" {
		t.Fatalf("chat = %q", got["TELEGRAM_HOME_CHANNEL"])
	}
	if _, ok := got["OTHER_SECRET"]; ok {
		t.Fatal("allowlist leaked an unrelated secret")
	}
	if _, ok := got["EMPTY"]; ok {
		t.Fatal("empty values should be dropped")
	}
}

func TestReadEnvAllowlistMissingFile(t *testing.T) {
	got := readEnvAllowlist(filepath.Join(t.TempDir(), "nope"), alertEnvKeys)
	if len(got) != 0 {
		t.Fatalf("want empty map, got %v", got)
	}
}

func TestRunTimeout(t *testing.T) {
	cases := []struct {
		env  string
		want time.Duration
	}{
		{"", defaultRunTimeout},
		{"20m", 20 * time.Minute},
		{"90s", defaultRunTimeout}, // below the floor
		{"10h", defaultRunTimeout}, // above the ceiling
		{"nonsense", defaultRunTimeout},
		{" 7m ", 7 * time.Minute},
	}
	for _, c := range cases {
		t.Setenv("SENTINEL_RUN_TIMEOUT", c.env)
		if got := runTimeout(); got != c.want {
			t.Errorf("runTimeout(%q) = %s, want %s", c.env, got, c.want)
		}
	}
}

func newTestNotifier(now *time.Time) (*notifier, *int) {
	sent := new(int)
	n := &notifier{
		now:       func() time.Time { return *now },
		lastByKey: map[string]time.Time{},
	}
	n.send = func(ctx context.Context, text string) error {
		*sent++
		return nil
	}
	return n, sent
}

func TestAlertCooldownPerThread(t *testing.T) {
	now := time.Date(2026, 11, 21, 18, 0, 0, 0, time.UTC)
	n, sent := newTestNotifier(&now)
	err := errors.New("agent run exceeded the 15m0s limit and was stopped")

	if !n.AlertRunFailure(context.Background(), "PDF notes", "t1", "sess", err, "") {
		t.Fatal("first alert should send")
	}
	now = now.Add(2 * time.Minute)
	if n.AlertRunFailure(context.Background(), "PDF notes", "t1", "sess", err, "") {
		t.Fatal("second alert inside the cooldown must be suppressed")
	}
	if *sent != 1 {
		t.Fatalf("sent = %d, want 1", *sent)
	}
	now = now.Add(alertCooldown)
	if !n.AlertRunFailure(context.Background(), "PDF notes", "t1", "sess", err, "") {
		t.Fatal("alert after the cooldown should send")
	}
	if *sent != 2 {
		t.Fatalf("sent = %d, want 2", *sent)
	}
}

func TestAlertHourlyCeiling(t *testing.T) {
	now := time.Date(2026, 11, 21, 18, 0, 0, 0, time.UTC)
	n, sent := newTestNotifier(&now)
	err := errors.New("boom")

	for i := 0; i < alertMaxPerHr+3; i++ {
		n.AlertRunFailure(context.Background(), "t", string(rune('a'+i)), "s", err, "")
	}
	if *sent != alertMaxPerHr {
		t.Fatalf("sent = %d, want the %d/hour ceiling", *sent, alertMaxPerHr)
	}
}

func TestAlertFailingSendIsReported(t *testing.T) {
	now := time.Now()
	n, _ := newTestNotifier(&now)
	n.send = func(ctx context.Context, text string) error { return errors.New("telegram 502") }
	if n.AlertRunFailure(context.Background(), "t", "t1", "s", errors.New("boom"), "") {
		t.Fatal("a failed send must report false so the UI does not claim delivery")
	}
}

func TestAlertMessageContent(t *testing.T) {
	now := time.Date(2026, 11, 21, 18, 4, 0, 0, time.UTC)
	n, _ := newTestNotifier(&now)
	var got string
	n.send = func(ctx context.Context, text string) error { got = text; return nil }

	err := errors.New("agent run exceeded the 15m0s limit and was stopped")
	partial := "Chapter 1 drafted, cache warmed.\n\nThe deploy stalled at " +
		strings.Repeat("step 3 ", 60) + "and never reached the final gate."
	n.AlertRunFailure(context.Background(), "Build the PDF", "t9", "local-abc", err, partial)

	for _, want := range []string{"run failed", "Build the PDF", "15m0s", "local-abc"} {
		if !strings.Contains(got, want) {
			t.Errorf("alert text missing %q:\n%s", want, got)
		}
	}
	if !strings.Contains(got, "Partial reply:") || !strings.Contains(got, "never reached the final gate.") {
		t.Errorf("alert text missing the partial-reply tail:\n%s", got)
	}
	if strings.Contains(got, "Chapter 1 drafted") {
		t.Errorf("tail should drop the start of a long partial:\n%s", got)
	}
	if strings.Contains(got, "\n\n\n") {
		t.Errorf("alert text has stray blank lines:\n%s", got)
	}

	// A run with no text at all must not print an empty partial line.
	// (Fresh thread key: the per-thread cooldown would suppress a repeat.)
	n.AlertRunFailure(context.Background(), "Build the PDF", "t10", "local-abc", err, "")
	if strings.Contains(got, "Partial reply:") {
		t.Errorf("empty partial should be omitted:\n%s", got)
	}
}

func TestAlertNilNotifier(t *testing.T) {
	var n *notifier
	if n.AlertRunFailure(context.Background(), "t", "t1", "s", errors.New("boom"), "") {
		t.Fatal("nil notifier must report false")
	}
}

// TestAlertE2E sends a real Telegram message. Opt in with SENTINEL_ALERT_E2E=1.
func TestAlertE2E(t *testing.T) {
	if os.Getenv("SENTINEL_ALERT_E2E") != "1" {
		t.Skip("set SENTINEL_ALERT_E2E=1 to send a real alert")
	}
	n := newNotifier()
	if n == nil {
		t.Fatal("notifier not configured; check TELEGRAM_BOT_TOKEN and TELEGRAM_HOME_CHANNEL")
	}
	err := errors.New("sentinel alert self-test — this proves timeout alerts will reach you")
	if !n.AlertRunFailure(context.Background(), "Sentinel alert test", "selftest", "selftest", err, "") {
		t.Fatal("test alert was not delivered")
	}
}

func TestReplyTail(t *testing.T) {
	if got := replyTail("", 200); got != "" {
		t.Errorf("empty in, got %q", got)
	}
	short := "all done here"
	if got := replyTail(short, 200); got != short {
		t.Errorf("short input must pass through: %q", got)
	}
	got := replyTail(strings.Repeat("a", 300), 200)
	if len(got) != 203 || !strings.HasPrefix(got, "…") { // 3-byte ellipsis + 200 bytes
		t.Errorf("long tail = %q (%d bytes)", got, len(got))
	}
	// A 3-byte rune at an unaligned cut must not be split in half.
	got = replyTail(strings.Repeat("€", 300), 200)
	if !utf8.ValidString(got) || !strings.HasPrefix(got, "…") {
		t.Errorf("multibyte tail not rune-aligned: %q", got)
	}
	// Newlines inside the tail must not break the one-line Telegram format.
	got = replyTail("one\ntwo\nthree", 200)
	if strings.Contains(got, "\n") {
		t.Errorf("tail kept newlines: %q", got)
	}
}
