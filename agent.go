package main

// Runs `hermes chat` in one-shot stream-json mode against a named session,
// relaying text deltas as they arrive. Stdout is pure JSONL; stderr carries
// the session banner and error output, kept as a tail for diagnostics.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

type AgentResult struct {
	Text       string
	SessionID  string
	Stopped    bool // cancelled via /api/stop
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

func runHermes(ctx context.Context, sessionName, query string, onDelta func(string)) AgentResult {
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

	ctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()

	// The binary is resolved from fixed user-owned locations and every argument
	// is server-generated; the user's message content only ever travels inside
	// the query file, never on the command line.
	// #nosec G204
	cmd := exec.CommandContext(ctx, hermesBin(), args...)
	cmd.Dir = homeDir
	cmd.Env = append(os.Environ(), "PATH="+homeBin+":"+os.Getenv("PATH"))
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

	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 || line[0] != '{' {
			continue
		}
		var ev struct {
			Type      string `json:"type"`
			Text      string `json:"text"`
			SessionID string `json:"session_id"`
			ExitCode  int    `json:"exit_code"`
		}
		if err := json.Unmarshal(line, &ev); err != nil {
			continue
		}
		switch ev.Type {
		case "text":
			if ev.Text != "" {
				res.Text += ev.Text
				if onDelta != nil {
					onDelta(ev.Text)
				}
			}
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

	switch ctx.Err() {
	case context.DeadlineExceeded:
		res.Err = errors.New("agent run exceeded the 30-minute budget and was stopped")
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
