package main

// Runs `hermes chat` in one-shot stream-json mode against a named session,
// relaying text deltas as they arrive. Stdout is pure JSONL; stderr carries
// the session banner and error output, kept as a tail for diagnostics.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// defaultRunTimeout bounds a single webchat run. It matches the old hardcoded
// 30 minutes so an unconfigured deploy behaves exactly as before; long jobs get
// more room through SENTINEL_RUN_TIMEOUT rather than by editing code.
const defaultRunTimeout = 30 * time.Minute

const (
	minRunTimeout = 5 * time.Minute
	maxRunTimeout = 180 * time.Minute
)

// runLimit resolves the active run limit. It is a variable so tests can shorten
// it without waiting minutes.
var runLimit = runTimeout

// fmtLimit renders a duration the way a human writes it ("30m", "2h", "90s"),
// so the failure message and the Telegram alert do not say "30m0s".
func fmtLimit(d time.Duration) string {
	switch {
	case d%time.Hour == 0:
		return fmt.Sprintf("%dh", int(d.Hours()))
	case d%time.Minute == 0:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	default:
		return d.String()
	}
}

// runTimeout reads SENTINEL_RUN_TIMEOUT and clamps it to a sane range.
func runTimeout() time.Duration {
	raw := strings.TrimSpace(os.Getenv("SENTINEL_RUN_TIMEOUT"))
	if raw == "" {
		return defaultRunTimeout
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d < minRunTimeout || d > maxRunTimeout {
		// The rejected value is deliberately not logged: env values are tainted
		// input and would be a log-injection vector.
		log.Printf("sentinel: ignoring invalid SENTINEL_RUN_TIMEOUT, using %s", defaultRunTimeout)
		return defaultRunTimeout
	}
	return d
}

type AgentResult struct {
	Text       string
	SessionID  string
	Activities []Activity
	Stopped    bool // cancelled via /api/stop
	TimedOut   bool // hit the run limit
	StderrTail string
	Err        error
}

func hermesBin() string {
	if p, err := exec.LookPath("hermes"); err == nil {
		return p
	}
	for _, p := range []string{
		filepath.Join(homeBin, "hermes"),
		filepath.Join(homeDir, ".hermes", "hermes-agent", "hermes"),
		"/usr/local/bin/hermes",
	} {
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			return p
		}
	}
	return "hermes"
}

type cappedBuffer struct{ b bytes.Buffer }

func (c *cappedBuffer) Write(p []byte) (int, error) {
	if c.b.Len() > 16*1024 {
		data := c.b.Bytes()
		c.b.Reset()
		c.b.Write(data[len(data)-8*1024:])
	}
	return c.b.Write(p)
}

func (c *cappedBuffer) Tail() string { return c.b.String() }

func shorten(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 1200 {
		s = s[len(s)-1200:]
	}
	return s
}

type streamEvent struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	SessionID string          `json:"session_id"`
	ExitCode  int             `json:"exit_code"`
	ID        string          `json:"id"`
	ToolID    string          `json:"tool_id"`
	CallID    string          `json:"call_id"`
	Name      string          `json:"name"`
	Tool      string          `json:"tool"`
	ToolName  string          `json:"tool_name"`
	Input     json.RawMessage `json:"input"`
	Args      json.RawMessage `json:"args"`
	Arguments json.RawMessage `json:"arguments"`
	Status    string          `json:"status"`
	Message   string          `json:"message"`
	Error     string          `json:"error"`
	IsError   bool            `json:"is_error"`
	Duration  int64           `json:"duration_ms"`
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func summarizeJSON(raws ...json.RawMessage) string {
	for _, raw := range raws {
		if len(raw) == 0 || string(raw) == "null" {
			continue
		}
		var m map[string]any
		if json.Unmarshal(raw, &m) == nil && len(m) > 0 {
			var parts []string
			for k, v := range m {
				s := fmt.Sprintf("%v", v)
				if len(s) > 48 {
					s = s[:48] + "…"
				}
				parts = append(parts, k+"="+s)
				if len(parts) >= 3 {
					break
				}
			}
			return strings.Join(parts, ", ")
		}
		s := strings.TrimSpace(string(raw))
		if len(s) > 100 {
			s = s[:100] + "…"
		}
		return s
	}
	return ""
}

func cloneActivities(src []Activity) []Activity {
	if len(src) == 0 {
		return nil
	}
	out := make([]Activity, len(src))
	copy(out, src)
	return out
}

func runHermes(ctx context.Context, sessionName, model, query string, onDelta func(string), onActivity func([]Activity)) AgentResult {
	var res AgentResult
	qf, err := os.CreateTemp(dataDir, "q-*.txt")
	if err != nil {
		res.Err = err
		return res
	}
	defer os.Remove(qf.Name())
	if err := os.WriteFile(qf.Name(), []byte(query), 0600); err != nil {
		res.Err = err
		return res
	}

	args := []string{
		"chat",
		"--query-file", qf.Name(),
		"-Q", "--format", "stream-json",
		"--continue", sessionName, "--create-if-missing",
	}
	if model != "" && validModelName(model) {
		args = append(args, "--model", model)
	}

	limit := runLimit()
	ctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()

	// The binary is resolved from fixed user-owned locations and every argument
	// is server-generated; the user's message content only ever travels inside
	// the query file, never on the command line.
	// #nosec G204
	cmd := exec.CommandContext(ctx, hermesBin(), args...)
	cmd.Dir = homeDir
	cmd.Env = append(os.Environ(), "PATH="+homeBin+":"+os.Getenv("PATH"))
	// hermes spawns tool subprocesses that inherit stdout. Killing only the
	// direct child left them running and kept the pipe open, so the parent sat
	// on a dead run for seconds: put the child in its own process group and kill
	// the whole group on deadline/cancel, then stop waiting on the pipe.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil && err != syscall.ESRCH {
			return err
		}
		return nil
	}
	cmd.WaitDelay = 5 * time.Second
	var errb cappedBuffer
	cmd.Stderr = &errb
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		res.Err = err
		return res
	}
	if err := cmd.Start(); err != nil {
		res.Err = fmt.Errorf("starting hermes: %w", err)
		return res
	}

	var activities []Activity
	emitAct := func() {
		if onActivity != nil {
			onActivity(cloneActivities(activities))
		}
	}

	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 || line[0] != '{' {
			continue
		}
		var ev streamEvent
		if err := json.Unmarshal(line, &ev); err != nil {
			continue
		}
		nowMs := time.Now().UnixMilli()
		switch ev.Type {
		case "text":
			if ev.Text != "" {
				// Mark any active thinking step complete when text starts arriving.
				for i := len(activities) - 1; i >= 0; i-- {
					if activities[i].Kind == "thinking" && activities[i].Status == "running" {
						activities[i].Status = "done"
						activities[i].DurationMs = nowMs - activities[i].StartedAt
						emitAct()
						break
					}
				}
				res.Text += ev.Text
				if onDelta != nil {
					onDelta(ev.Text)
				}
			}
		case "thinking", "reasoning":
			found := false
			for i := len(activities) - 1; i >= 0; i-- {
				if activities[i].Kind == "thinking" && activities[i].Status == "running" {
					found = true
					break
				}
			}
			if !found {
				activities = append(activities, Activity{
					ID:        fmt.Sprintf("act-%d", len(activities)+1),
					Kind:      "thinking",
					Name:      "Reasoning",
					Status:    "running",
					StartedAt: nowMs,
				})
				emitAct()
			}
		case "tool_use", "tool_call", "tool_start":
			id := firstNonEmpty(ev.ID, ev.ToolID, ev.CallID, fmt.Sprintf("tool-%d", len(activities)+1))
			name := firstNonEmpty(ev.Name, ev.ToolName, ev.Tool, "tool")
			detail := summarizeJSON(ev.Input, ev.Args, ev.Arguments)
			activities = append(activities, Activity{
				ID:        id,
				Kind:      "tool",
				Name:      name,
				Detail:    detail,
				Status:    "running",
				StartedAt: nowMs,
			})
			emitAct()
		case "tool_result", "tool_end", "tool_error":
			id := firstNonEmpty(ev.ID, ev.ToolID, ev.CallID)
			name := firstNonEmpty(ev.Name, ev.ToolName, ev.Tool)
			status := "done"
			if ev.Type == "tool_error" || ev.IsError || ev.Error != "" || ev.Status == "error" {
				status = "error"
			}
			errMsg := firstNonEmpty(ev.Error)
			matched := false
			for i := len(activities) - 1; i >= 0; i-- {
				if activities[i].Kind != "tool" || activities[i].Status != "running" {
					continue
				}
				if (id != "" && activities[i].ID == id) || (id == "" && (name == "" || activities[i].Name == name)) {
					activities[i].Status = status
					activities[i].Error = errMsg
					if ev.Duration > 0 {
						activities[i].DurationMs = ev.Duration
					} else {
						activities[i].DurationMs = nowMs - activities[i].StartedAt
					}
					matched = true
					break
				}
			}
			if !matched && name != "" {
				activities = append(activities, Activity{
					ID:         firstNonEmpty(id, fmt.Sprintf("tool-%d", len(activities)+1)),
					Kind:       "tool",
					Name:       name,
					Status:     status,
					Error:      errMsg,
					StartedAt:  nowMs,
					DurationMs: ev.Duration,
				})
			}
			emitAct()
		case "status":
			msg := firstNonEmpty(ev.Message, ev.Status, ev.Text)
			if msg != "" {
				activities = append(activities, Activity{
					ID:        fmt.Sprintf("st-%d", len(activities)+1),
					Kind:      "status",
					Name:      msg,
					Status:    "done",
					StartedAt: nowMs,
				})
				emitAct()
			}
		case "error":
			errMsg := firstNonEmpty(ev.Error, ev.Message, ev.Text, "agent error")
			activities = append(activities, Activity{
				ID:        fmt.Sprintf("err-%d", len(activities)+1),
				Kind:      "status",
				Name:      "Error",
				Status:    "error",
				Error:     errMsg,
				StartedAt: nowMs,
			})
			emitAct()
		case "result":
			res.SessionID = ev.SessionID
			if ev.Text != "" {
				res.Text = ev.Text
			}
		}
	}
	_ = sc.Err()
	werr := cmd.Wait()
	res.StderrTail = errb.Tail()

	// Close out any still-running activities when the process exits.
	endMs := time.Now().UnixMilli()
	for i := range activities {
		if activities[i].Status == "running" {
			if ctx.Err() != nil {
				activities[i].Status = "error"
			} else {
				activities[i].Status = "done"
			}
			activities[i].DurationMs = endMs - activities[i].StartedAt
		}
	}
	res.Activities = activities

	switch ctx.Err() {
	case context.DeadlineExceeded:
		res.TimedOut = true
		res.Err = fmt.Errorf("agent run exceeded the %s limit and was stopped", fmtLimit(limit))
		return res
	case context.Canceled:
		res.Stopped = true
		return res
	}
	if werr != nil && res.Text == "" && res.SessionID == "" {
		if tail := strings.TrimSpace(res.StderrTail); tail != "" {
			res.Err = fmt.Errorf("hermes exited with error: %s", shorten(tail))
		} else {
			res.Err = fmt.Errorf("hermes exited with error: %v", werr)
		}
	}
	return res
}
