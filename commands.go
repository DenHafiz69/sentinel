package main

import "net/http"

// CommandSpec defines a slash command supported by both the UI autocomplete
// and Sentinel's backend/client actions.
type CommandSpec struct {
	Name        string `json:"name"`
	Usage       string `json:"usage"`
	Description string `json:"description"`
	TakesArgs   bool   `json:"takesArgs,omitempty"`
}

var commandRegistry = []CommandSpec{
	{Name: "/new", Usage: "/new", Description: "Start a new conversation"},
	{Name: "/help", Usage: "/help", Description: "Show all available commands"},
	{Name: "/clear", Usage: "/clear", Description: "Clear current conversation"},
	{Name: "/rename", Usage: "/rename <title>", Description: "Rename current thread", TakesArgs: true},
	{Name: "/search", Usage: "/search [query]", Description: "Search conversation history", TakesArgs: true},
	{Name: "/archive", Usage: "/archive", Description: "Archive current conversation"},
	{Name: "/stop", Usage: "/stop", Description: "Stop active generation"},
}

func (s *Server) handleCommands(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{"commands": commandRegistry})
}
