package main

// Run-failure alerts.
//
// When a webchat run dies (timeout, crash, provider error) the message only
// appeared in the browser, so a failure that happened while the tab was closed
// was invisible. This file posts a short plain-text alert to Telegram using the
// same bot the Hermes gateway already uses — the token is read at startup from
// the allowlisted keys of $HERMES_HOME/.env and is never logged.
//
// Configuration (all optional):
//   SENTINEL_ALERT_TELEGRAM=0     disable alerts entirely
//   SENTINEL_ALERT_CHAT_ID        override the destination chat
//   SENTINEL_RUN_TIMEOUT          run limit, Go duration (default 30m)

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const (
	alertCooldown  = 5 * time.Minute // per thread
	alertMaxPerHr  = 10              // across all threads
	alertTailChars = 200             // bytes of a partial reply that go into an alert
	alertMaxChars  = 3500
	alertsDisabled = "0"
)

// alertEnvKeys are the only keys ever read out of the Hermes env file.
var alertEnvKeys = []string{"TELEGRAM_BOT_TOKEN", "TELEGRAM_HOME_CHANNEL"}

type notifier struct {
	token  string
	chatID string
	client *http.Client

	// injectable for tests
	now  func() time.Time
	send func(ctx context.Context, text string) error

	mu        sync.Mutex
	lastByKey map[string]time.Time
	sentAt    []time.Time
}

// newNotifier builds the notifier from the environment, or returns nil when
// alerts are disabled or unconfigured. It never logs secret material.
func newNotifier() *notifier {
	if strings.TrimSpace(os.Getenv("SENTINEL_ALERT_TELEGRAM")) == alertsDisabled {
		log.Printf("sentinel: run alerts disabled by SENTINEL_ALERT_TELEGRAM=0")
		return nil
	}

	token := strings.TrimSpace(os.Getenv("TELEGRAM_BOT_TOKEN"))
	chatID := strings.TrimSpace(os.Getenv("SENTINEL_ALERT_CHAT_ID"))

	if token == "" || chatID == "" {
		vals := readEnvAllowlist(filepath.Join(hermesHome(), ".env"), alertEnvKeys)
		if token == "" {
			token = vals["TELEGRAM_BOT_TOKEN"]
		}
		if chatID == "" {
			chatID = vals["TELEGRAM_HOME_CHANNEL"]
		}
	}

	if token == "" || chatID == "" {
		log.Printf("sentinel: run alerts disabled (no telegram token or chat id)")
		return nil
	}

	n := &notifier{
		token:     token,
		chatID:    chatID,
		client:    &http.Client{Timeout: 8 * time.Second},
		now:       time.Now,
		lastByKey: map[string]time.Time{},
	}
	n.send = n.sendTelegram
	log.Printf("sentinel: run alerts enabled (telegram)")
	return n
}

// hermesHome resolves the Hermes home without hardcoding the path.
func hermesHome() string {
	if h := strings.TrimSpace(os.Getenv("HERMES_HOME")); h != "" {
		return h
	}
	if h, err := os.UserHomeDir(); err == nil {
		return filepath.Join(h, ".hermes")
	}
	return ".hermes"
}

// readEnvAllowlist parses KEY=VALUE lines, keeping only the requested keys.
// Values are returned as-is (quotes stripped, no expansion) and never logged.
func readEnvAllowlist(path string, keys []string) map[string]string {
	out := map[string]string{}
	want := make(map[string]bool, len(keys))
	for _, k := range keys {
		want[k] = true
	}

	f, err := os.Open(path) // #nosec G304 -- path is $HERMES_HOME/.env, never request data
	if err != nil {
		return out
	}
	defer f.Close()

	b, err := io.ReadAll(io.LimitReader(f, 1<<20))
	if err != nil {
		return out
	}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		k, v, ok := strings.Cut(line, "=")
		if !ok || !want[strings.TrimSpace(k)] {
			continue
		}
		v = strings.TrimSpace(v)
		v = strings.Trim(v, `"'`)
		if v != "" {
			out[strings.TrimSpace(k)] = v
		}
	}
	return out
}

// AlertRunFailure reports a failed run and returns whether it reached the user.
// key is the thread id, so one broken thread cannot spam the chat.
// partial is the run's text so far, when any: its tail goes into the alert so
// the message is actionable without opening the chat.
func (n *notifier) AlertRunFailure(ctx context.Context, title, threadID, sessionName string, runErr error, partial string) bool {
	if n == nil || runErr == nil {
		return false
	}
	now := n.now()
	if !n.allow(threadID, now) {
		return false
	}

	var b strings.Builder
	b.WriteString("Sentinel · run failed\n")
	if t := strings.TrimSpace(title); t != "" {
		fmt.Fprintf(&b, "Thread: %s\n", t)
	}
	fmt.Fprintf(&b, "Reason: %s\n", oneLine(runErr.Error()))
	if tail := replyTail(partial, alertTailChars); tail != "" {
		fmt.Fprintf(&b, "Partial reply: %s\n", tail)
	}
	if s := strings.TrimSpace(sessionName); s != "" {
		fmt.Fprintf(&b, "Session: %s\n", s)
	}
	fmt.Fprintf(&b, "Time: %s\n", now.Format("2006-01-02 15:04 MST"))
	b.WriteString("\nOpen chat.denhafiz.my to retry, or run long jobs from the CLI.")

	text := b.String()
	if len(text) > alertMaxChars {
		text = text[:alertMaxChars]
	}
	if err := n.send(ctx, text); err != nil {
		log.Printf("sentinel: run alert not delivered: %v", err)
		return false
	}
	log.Printf("sentinel: run alert delivered (thread %s)", threadID)
	return true
}

// allow applies the per-thread cooldown and the hourly ceiling.
func (n *notifier) allow(key string, now time.Time) bool {
	n.mu.Lock()
	defer n.mu.Unlock()

	if last, ok := n.lastByKey[key]; ok && now.Sub(last) < alertCooldown {
		return false
	}

	cutoff := now.Add(-time.Hour)
	kept := n.sentAt[:0]
	for _, t := range n.sentAt {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	n.sentAt = kept
	if len(n.sentAt) >= alertMaxPerHr {
		return false
	}

	n.lastByKey[key] = now
	n.sentAt = append(n.sentAt, now)
	return true
}

func (n *notifier) sendTelegram(ctx context.Context, text string) error {
	form := url.Values{
		"chat_id":                  {n.chatID},
		"text":                     {text},
		"disable_web_page_preview": {"true"},
	}
	// The token is in the URL by Telegram's design; it must never be logged.
	endpoint := "https://api.telegram.org/bot" + n.token + "/sendMessage"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return fmt.Errorf("building request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := n.client.Do(req)
	if err != nil {
		return fmt.Errorf("posting to telegram: %w", redact(err, n.token))
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode == http.StatusOK {
		return nil
	}
	var apiErr struct {
		Description string `json:"description"`
	}
	if json.Unmarshal(body, &apiErr) == nil && apiErr.Description != "" {
		return fmt.Errorf("telegram %d: %s", resp.StatusCode, apiErr.Description)
	}
	return fmt.Errorf("telegram %d", resp.StatusCode)
}

func oneLine(s string) string {
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.TrimSpace(s)
	if len(s) > 300 {
		s = s[:300] + "…"
	}
	return s
}

// replyTail returns the last n bytes of s, cut on a rune boundary so the
// alert never shows half a character. Empty input stays empty.
func replyTail(s string, n int) string {
	s = strings.TrimSpace(s)
	if n <= 0 || len(s) <= n {
		return oneLine(s)
	}
	cut := len(s) - n
	for cut < len(s) && !utf8.RuneStart(s[cut]) {
		cut++
	}
	return "…" + oneLine(strings.TrimSpace(s[cut:]))
}

// redact strips the bot token from transport errors.
func redact(err error, token string) error {
	if err == nil || token == "" {
		return err
	}
	msg := strings.ReplaceAll(err.Error(), token, "<redacted>")
	return fmt.Errorf("%s", msg)
}

// alertRunFailure reports a failed run, capped so a slow Telegram call cannot
// hold up the UI update for long. partial is the run's text so far, if any.
func (s *Server) alertRunFailure(threadID, session string, err error, partial string) bool {
	if s.notifier == nil || err == nil {
		return false
	}
	title := ""
	if th, ok := s.store.ThreadByID(threadID); ok {
		title = th.Title
	}
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	return s.notifier.AlertRunFailure(ctx, title, threadID, session, err, partial)
}
