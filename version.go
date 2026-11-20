package main

import "net/http"

// Version can be overridden at build time via -ldflags "-X main.Version=x.y.z".
var Version = "0.2.0"

type ChangelogEntry struct {
	Version string   `json:"version"`
	Date    string   `json:"date"`
	Title   string   `json:"title"`
	Items   []string `json:"items"`
}

var changelog = []ChangelogEntry{
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
