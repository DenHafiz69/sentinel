package main

// /update — pull origin, run the quality gate, swap the binary and restart.
//
// The checkout is resolved from the running binary (<repo>/bin/webchat), so an
// update can only ever touch the tree Sentinel was built from. Every step is a
// fixed tool with fixed arguments: nothing the user types reaches a shell, and
// the merge is --ff-only so local work is never rewritten. If any gate fails
// the binary is left untouched and the old process keeps serving.

import (
	"context"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

const updateTimeout = 4 * time.Minute

var updateInFlight atomic.Bool

type updateStep struct {
	Name string `json:"name"`
	OK   bool   `json:"ok"`
	Out  string `json:"out,omitempty"`
}

type updateReport struct {
	OK        bool         `json:"ok"`
	Updated   bool         `json:"updated"`
	Restarted bool         `json:"restarted"`
	From      string       `json:"from,omitempty"`
	To        string       `json:"to,omitempty"`
	Message   string       `json:"message"`
	Steps     []updateStep `json:"steps,omitempty"`
}

// updateRepoDir resolves the source tree from the running executable.
func updateRepoDir() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	if resolved, rerr := filepath.EvalSymlinks(exe); rerr == nil {
		exe = resolved
	}
	dir := filepath.Dir(filepath.Dir(exe))
	if fi, err := os.Stat(filepath.Join(dir, ".git")); err != nil || !fi.IsDir() {
		return "", fmt.Errorf("no git checkout above the running binary (%s)", dir)
	}
	return dir, nil
}

func toolPath(names ...string) string {
	for _, n := range names {
		if p, err := exec.LookPath(n); err == nil {
			return p
		}
		if p := filepath.Join(homeBin, n); fileExists(p) {
			return p
		}
	}
	return ""
}

func fileExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && !fi.IsDir()
}

func gitBin() string {
	if p := toolPath("git"); p != "" {
		return p
	}
	return "/usr/bin/git"
}

func goBin() string {
	if p := toolPath("go"); p != "" {
		return p
	}
	return filepath.Join(homeBin, "go")
}

// runStep runs one fixed command in the checkout and captures its output.
func runStep(ctx context.Context, dir, name, bin string, args ...string) updateStep {
	// #nosec G204 — bin and args come from this file's own constants and the
	// repository path, never from a request body or query string.
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"PATH="+homeBin+":"+os.Getenv("PATH"),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_SSH_COMMAND=ssh -o BatchMode=yes",
		"GIT_PAGER=cat",
		"GOFLAGS=",
	)
	var buf cappedBuffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	out := strings.TrimSpace(buf.Tail())
	if err != nil {
		out = strings.TrimSpace(out + "\n" + err.Error())
	}
	if ctx.Err() != nil {
		err = ctx.Err()
	}
	return updateStep{Name: name, OK: err == nil, Out: shorten(out)}
}

func gitOut(ctx context.Context, dir string, args ...string) string {
	st := runStep(ctx, dir, "git "+strings.Join(args, " "), gitBin(), args...)
	if !st.OK {
		return ""
	}
	return strings.TrimSpace(st.Out)
}

func shortRev(rev string) string {
	if len(rev) > 7 {
		return rev[:7]
	}
	return rev
}

// dirtyTracked reports modified tracked files (untracked files such as the
// gitignored data/ directory are fine).
func dirtyTracked(ctx context.Context, dir string) string {
	st := runStep(ctx, dir, "git status", gitBin(), "status", "--porcelain", "--untracked-files=no")
	if !st.OK {
		return ""
	}
	var names []string
	for _, line := range strings.Split(st.Out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// porcelain v1 is "XY <path>" — the path starts at column 4.
		if len(line) > 3 {
			names = append(names, strings.TrimSpace(line[3:]))
		}
	}
	if len(names) > 5 {
		names = append(names[:5], "…")
	}
	return strings.Join(names, ", ")
}

// binaryStale reports whether any source file is newer than the binary, which
// covers the case where a previous update pulled but failed the gate.
func binaryStale(dir string) bool {
	binInfo, err := os.Stat(filepath.Join(dir, "bin", "webchat"))
	if err != nil {
		return true
	}
	newest := time.Time{}
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "bin", "data":
				return fs.SkipDir
			}
			return nil
		}
		switch filepath.Ext(d.Name()) {
		case ".go", ".html", ".js", ".webmanifest", ".png", ".mod", ".sum":
			if fi, err := d.Info(); err == nil && fi.ModTime().After(newest) {
				newest = fi.ModTime()
			}
		}
		return nil
	})
	return newest.After(binInfo.ModTime())
}

func gofmtStep(ctx context.Context, dir string) updateStep {
	bin := toolPath("gofmt")
	if bin == "" {
		bin = filepath.Join(filepath.Dir(goBin()), "gofmt")
	}
	st := runStep(ctx, dir, "gofmt -l", bin, "-l", ".")
	if st.OK && st.Out != "" {
		st.OK = false
		st.Out = "unformatted:\n" + st.Out
	}
	return st
}

// selfUpdate does the work and reports what happened. It never leaves a broken
// binary in place: the new build is written to bin/webchat.new and only renamed
// over bin/webchat after every gate passed.
func selfUpdate(ctx context.Context, dir string) updateReport {
	rep := updateReport{}
	var steps []updateStep
	defer func() { rep.Steps = steps }()

	branch := gitOut(ctx, dir, "symbolic-ref", "--short", "-q", "HEAD")
	if branch == "" {
		rep.Message = "Update refused: the checkout is not on a branch (detached HEAD)."
		return rep
	}
	remoteRef := "origin/" + branch

	steps = append(steps, runStep(ctx, dir, "git fetch", gitBin(), "fetch", "--prune", "origin"))
	if !steps[len(steps)-1].OK {
		rep.Message = "Update failed: could not fetch origin."
		return rep
	}
	head := gitOut(ctx, dir, "rev-parse", "HEAD")
	remote := gitOut(ctx, dir, "rev-parse", remoteRef)
	if head == "" || remote == "" {
		rep.Message = "Update failed: could not resolve HEAD or " + remoteRef + "."
		return rep
	}
	rep.From, rep.To = shortRev(head), shortRev(remote)

	if dirty := dirtyTracked(ctx, dir); dirty != "" {
		rep.Message = "Update refused: uncommitted changes in the checkout (" + dirty + ")."
		return rep
	}

	rebuilt := false
	if head != remote {
		st := runStep(ctx, dir, "git merge --ff-only "+remoteRef, gitBin(), "merge", "--ff-only", remoteRef)
		steps = append(steps, st)
		if !st.OK {
			rep.Message = "Fast-forward failed; the checkout was left as it was."
			return rep
		}
		rep.Updated = true
	} else {
		if !binaryStale(dir) {
			rep.OK = true
			rep.Message = "Already up to date at " + rep.From + " — nothing to rebuild."
			return rep
		}
		rebuilt = true
	}

	checks := []updateStep{gofmtStep(ctx, dir)}
	for _, g := range []struct {
		name string
		args []string
	}{
		{"go vet", []string{"vet", "./..."}},
		{"go test", []string{"test", "./..."}},
	} {
		checks = append(checks, runStep(ctx, dir, g.name, goBin(), g.args...))
	}
	// staticcheck and gosec live in ~/go/bin; skip them when not installed
	// rather than failing the update on a tool that is simply absent.
	for _, g := range []struct {
		name string
		args []string
	}{
		{"staticcheck", []string{"./..."}},
		{"gosec", []string{"-quiet", "./..."}},
	} {
		bin := filepath.Join(homeDir, "go", "bin", g.name)
		if !fileExists(bin) {
			bin = toolPath(g.name)
		}
		if bin == "" {
			continue
		}
		checks = append(checks, runStep(ctx, dir, g.name, bin, g.args...))
	}
	steps = append(steps, checks...)
	for _, st := range checks {
		if !st.OK {
			rep.Message = "Gate failed at " + st.Name + " — the running binary was left untouched."
			if rep.Updated {
				rep.Message += " Source is now at " + rep.To + "; fix the gate and run /update again."
			}
			return rep
		}
	}

	tmp := filepath.Join(dir, "bin", "webchat.new")
	if err := os.MkdirAll(filepath.Dir(tmp), 0o750); err != nil {
		rep.Message = "Update failed: " + err.Error()
		return rep
	}
	st := runStep(ctx, dir, "go build", goBin(), "build", "-o", tmp, ".")
	steps = append(steps, st)
	if !st.OK {
		rep.Message = "Build failed — the running binary was left untouched."
		return rep
	}
	if err := os.Rename(tmp, filepath.Join(dir, "bin", "webchat")); err != nil {
		rep.Message = "Update failed: could not swap the binary: " + err.Error()
		return rep
	}

	rep.OK = true
	rep.Updated = true
	rep.Restarted = true
	if rebuilt {
		rep.Message = "Rebuilt " + rep.From + " (gates green). Restarting Sentinel…"
	} else {
		rep.Message = "Updated " + rep.From + " → " + rep.To + " (gates green). Restarting Sentinel…"
	}
	return rep
}

// restartUnit runs the restart from outside Sentinel's own cgroup: a transient
// systemd-run job survives the stop it triggers. If systemd-run is unavailable
// we signal ourselves and let the unit's Restart=always bring it back.
func restartUnit() {
	unit := envOr("SENTINEL_UNIT", "hermes-webchat")
	runDir := os.Getenv("XDG_RUNTIME_DIR")
	if runDir == "" {
		runDir = filepath.Join("/run/user", strconv.Itoa(os.Getuid()))
	}
	// #nosec G204 — unit name and runtime dir come from the unit's own
	// environment, not from a request.
	cmd := exec.Command("systemd-run",
		"--user", "--collect", "--quiet",
		"--unit=sentinel-selfupdate-"+strconv.FormatInt(time.Now().Unix(), 10),
		"--description=Sentinel self-update restart",
		"/bin/sh", "-c", "sleep 2; systemctl --user restart "+unit,
	)
	cmd.Env = append(os.Environ(),
		"XDG_RUNTIME_DIR="+runDir,
		"DBUS_SESSION_BUS_ADDRESS=unix:path="+filepath.Join(runDir, "bus"),
	)
	if err := cmd.Run(); err == nil {
		return
	}
	go func() {
		time.Sleep(1500 * time.Millisecond)
		_ = syscall.Kill(os.Getpid(), syscall.SIGTERM)
	}()
}

func (s *Server) notifyUpdate(role, text string) {
	s.store.AddNotice(s.store.Snapshot().Active, role, text)
	s.broadcast("snapshot", s.snapshotPayload())
}

func (s *Server) handleUpdate(w http.ResponseWriter, r *http.Request) {
	if !updateInFlight.CompareAndSwap(false, true) {
		writeJSON(w, updateReport{Message: "An update is already running."})
		return
	}
	defer updateInFlight.Store(false)

	if busy, thread, _, _, _, _ := s.runningState(); busy {
		writeJSON(w, updateReport{Message: "Sentinel is mid-reply on " + thread + " — stop it first (/stop)."})
		return
	}
	dir, err := updateRepoDir()
	if err != nil {
		s.notifyUpdate("error", "Update failed: "+err.Error())
		writeJSON(w, updateReport{Message: "Update failed: " + err.Error()})
		return
	}

	// Detached from the request so a closed tab cannot abandon a half-done
	// update; the timeout still bounds it.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), updateTimeout)
	defer cancel()

	rep := selfUpdate(ctx, dir)
	role := "assistant"
	if !rep.OK {
		role = "error"
	}
	s.notifyUpdate(role, rep.Message)
	writeJSON(w, rep)
	if rep.Restarted {
		restartUnit()
	}
}
