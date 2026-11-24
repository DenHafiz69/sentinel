package main

import "net/http"

// Version can be overridden at build time via -ldflags "-X main.Version=x.y.z".
var Version = "0.2.9"

type ChangelogEntry struct {
	Version string   `json:"version"`
	Date    string   `json:"date"`
	Title   string   `json:"title"`
	Items   []string `json:"items"`
}

var changelog = []ChangelogEntry{
	{
		Version: "0.2.9",
		Date:    "2026-11-25",
		Title:   "Unread Badges, Richer Markdown & Auth/Worker Hardening",
		Items: []string{
			"Added unread indicators on sidebar threads when replies or background deliveries arrive in other chats",
			"Upgraded Markdown rendering with tables, blockquotes, [label](url) links, and italics",
			"Deleting a thread now cancels any active run and closes its worker goroutine",
			"Sidebar thread titles and timestamps now update immediately on incoming msg events",
			"Hardened Cloudflare Access auth: fail closed on missing CF_ACCESS_TEAM/CF_AUD, removed CF_AUD='-' bypass, and added optional ALLOWED_EMAILS allowlist",
		},
	},
	{
		Version: "0.2.8",
		Date:    "2026-11-25",
		Title:   "Crash-Safe Saves, Instant Restarts & Coalesced Streaming",
		Items: []string{
			"state.json is written, fsynced, and the directory synced around the rename, so a power cut cannot leave it truncated",
			"Shutdown closes open SSE streams instead of waiting out its grace period: restarts with a tab open are instant and no longer log 'context deadline exceeded'",
			"Telegram failure alerts carry the tail of the partial reply, so you can see how far a dead run got without opening the chat",
			"Streaming sends the full text so far at most every 250ms instead of an event per token; a dropped event is healed by the next one",
		},
	},
	{
		Version: "0.2.7",
		Date:    "2026-11-21",
		Title:   "Background Work House Rules",
		Items: []string{
			"Every message now tells the agent the run cap and to defer work longer than ~8 minutes to the background",
			"Background results post back into the chat as real messages, via a one-shot cron with deliver=sentinel:<thread>",
			"On each new message the agent first reports any background work that has finished",
			"Live run cap drops from 45m to 10m (SENTINEL_RUN_TIMEOUT in the service unit)",
			"composeQuery carries the thread id; compose_test.go pins the house-rules note to every message, file-only ones included",
		},
	},
	{
		Version: "0.2.6",
		Date:    "2026-11-21",
		Title:   "Run Limits & Failure Alerts",
		Items: []string{
			"Run limit is configurable: SENTINEL_RUN_TIMEOUT (default 30m, clamped 5m-3h)",
			"A timed-out run keeps the partial reply instead of discarding the work",
			"Timeouts name the actual limit and say how to raise it",
			"Failed runs post a short alert to Telegram, throttled per thread and per hour",
			"Chat shows whether the alert was delivered instead of failing silently",
		},
	},
	{
		Version: "0.2.5",
		Date:    "2026-11-21",
		Title:   "Per-Thread Workers, Scoped Files, Media Players & Export",
		Items: []string{
			"Made active thread client-local and added per-thread agent workers",
			"Scoped /api/file to uploaded or message-attached files only",
			"Added hourly orphan upload reaper and inline audio/video players",
			"Added graceful SIGINT/SIGTERM shutdown and /export (md, json, txt)",
		},
	},
	{
		Version: "0.2.4",
		Date:    "2026-11-21",
		Title:   "Mobile Polish & Per-Thread Thinking Status",
		Items: []string{
			"Hid desktop-only Ctrl+K hints on mobile/touch devices while keeping them on desktop",
			"Shortened composer placeholder and prevented wrapping/clipping on narrow screens",
			"Scoped header thinking status, elapsed timer, and Stop button to the active thread",
		},
	},
	{
		Version: "0.2.3",
		Date:    "2026-11-21",
		Title:   "Mobile Responsiveness",
		Items: []string{
			"Touch layout: 44px targets on every primary control, 16px inputs so iOS does not zoom on focus",
			"Sidebar becomes an off-canvas drawer under 860px, with edge-swipe to open and swipe-left to close",
			"App height follows the visual viewport, so the composer stays above the Android keyboard",
			"Modals become full-screen sheets on phones, with contained overscroll",
			"Landscape block for phones held sideways: tighter header, composer and activity list",
			"aria-labels on icon buttons, press states on tappable rows, drag-and-drop disabled on touch",
		},
	},
	{
		Version: "0.2.2",
		Date:    "2026-11-21",
		Title:   "Gateway Delivery",
		Items: []string{
			"Added POST /api/inject so the Hermes gateway can push cron results and notifications into a thread as real chat messages",
			"Deliveries appear live over SSE and rename an unnamed thread from the job title",
			"Added a loopback-only, shared-secret guard that also refuses any request carrying Cloudflare or proxy headers, so the public hostname cannot reach the endpoint",
			"The inject token is generated on first run at ~/.config/sentinel/inject.token (0600)",
			"Added the sentinel gateway platform plugin (plugins/sentinel) for `deliver=sentinel:<thread>`",
		},
	},
	{
		Version: "0.2.1",
		Date:    "2026-11-21",
		Title:   "Message Actions, Drag & Drop, and Stream Recovery",
		Items: []string{
			"Added message Copy button and one-tap Retry on failed messages",
			"Added clipboard image paste and drag-and-drop file uploads",
			"Added mid-stream refresh recovery and SSE reconnect / session-expiry status",
		},
	},
	{
		Version: "0.2.0",
		Date:    "2026-11-21",
		Title:   "Settings, Slash Commands, Search & PWA Polish",
		Items: []string{
			"Added Settings modal (General, Appearance, Notifications, Full Changelog & About) and version number",
			"Added automatic What's New changelog popup when Sentinel updates",
			"Added slash command autocomplete (/new, /help, /clear, /rename, /search, /archive, /stop, /settings)",
			"Added instant conversation search (Ctrl+K) with match highlighting and jump-to-message",
			"Upgraded code blocks with syntax highlighting, line numbers, Copy, and Download",
			"Added thread management: pin, rename, archive, delete, and sort by recent/created",
			"Added live collapsible Agent Activity panel and per-message tool history",
			"Fixed PWA installability, full-width desktop layout, and mobile safe-area/composer sizing",
		},
	},
	{
		Version: "0.1.0",
		Date:    "2026-11-01",
		Title:   "Initial Release",
		Items: []string{
			"Telegram-style web chat backed by Hermes CLI stream-json sessions",
			"Cloudflare Access JWT verification (fail-closed) and os.Root-scoped file delivery",
			"Image/file uploads, SSE streaming replies, and offline-capable PWA shell",
		},
	},
}

func (s *Server) handleChangelog(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{
		"version":   Version,
		"changelog": changelog,
	})
}
