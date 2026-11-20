package main

// Covers the /update ref resolution: a local working branch has no
// origin/<branch>, which used to fail with
// "could not resolve HEAD or origin/<branch>".

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// gitRepo builds a remote + clone pair under t.TempDir and returns the clone.
func gitRepo(t *testing.T) (work, remote string) {
	t.Helper()
	base := t.TempDir()
	remote = filepath.Join(base, "remote.git")
	work = filepath.Join(base, "work")

	// One config file for the whole test, exported so runStep (which inherits
	// os.Environ) and the helper below share an identity.
	cfg := filepath.Join(base, "gitconfig")
	if err := os.WriteFile(cfg, []byte("[user]\n	name = Test\n	email = test@example.invalid\n[init]\n	defaultBranch = main\n[commit]\n	gpgsign = false\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", base)
	t.Setenv("GIT_CONFIG_GLOBAL", cfg)
	t.Setenv("GIT_CONFIG_SYSTEM", "/dev/null")

	run := func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"HOME="+base, "GIT_CONFIG_GLOBAL="+cfg, "GIT_CONFIG_SYSTEM=/dev/null",
			"GIT_TERMINAL_PROMPT=0", "GIT_AUTHOR_DATE=2026-01-01T00:00:00Z", "GIT_COMMITTER_DATE=2026-01-01T00:00:00Z")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %s in %s: %v\n%s", strings.Join(args, " "), dir, err, out)
		}
		return strings.TrimSpace(string(out))
	}

	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	run(base, "init", "--bare", "-q", remote)
	run(base, "clone", "-q", remote, work)
	if err := os.WriteFile(filepath.Join(work, "f.txt"), []byte("one\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run(work, "add", "f.txt")
	run(work, "commit", "-q", "-m", "one")
	run(work, "push", "-q", "-u", "origin", "main")
	// The remote's default branch, as a real clone would have it.
	run(work, "remote", "set-head", "origin", "main")
	return work, remote
}

func TestResolveUpdateRefUsesUpstreamOnMain(t *testing.T) {
	work, _ := gitRepo(t)
	ctx := context.Background()
	ref, fallback, err := resolveUpdateRef(ctx, work, "main")
	if err != nil {
		t.Fatalf("resolveUpdateRef: %v", err)
	}
	if ref != "origin/main" || fallback != "" {
		t.Fatalf("want origin/main with no fallback, got %q fallback %q", ref, fallback)
	}
}

// The regression: a local branch that was never pushed.
func TestResolveUpdateRefFallsBackForUnpublishedBranch(t *testing.T) {
	work, _ := gitRepo(t)
	ctx := context.Background()
	if st := runStep(ctx, work, "git checkout -b", gitBin(), "checkout", "-q", "-b", "fix/mobile-header-crowding"); !st.OK {
		t.Fatalf("checkout -b failed: %s", st.Out)
	}
	ref, fallback, err := resolveUpdateRef(ctx, work, "fix/mobile-header-crowding")
	if err != nil {
		t.Fatalf("unpublished branch should fall back to the remote default, got error: %v", err)
	}
	if ref != "origin/main" {
		t.Fatalf("want origin/main, got %q", ref)
	}
	if fallback != "fix/mobile-header-crowding" {
		t.Fatalf("fallback should name the unpublished branch, got %q", fallback)
	}
}

func TestResolveUpdateRefPrefersPublishedBranch(t *testing.T) {
	work, _ := gitRepo(t)
	ctx := context.Background()
	for _, args := range [][]string{
		{"checkout", "-q", "-b", "feat/x"},
		{"push", "-q", "-u", "origin", "feat/x"},
	} {
		if st := runStep(ctx, work, "git", gitBin(), args...); !st.OK {
			t.Fatalf("git %v failed: %s", args, st.Out)
		}
	}
	ref, fallback, err := resolveUpdateRef(ctx, work, "feat/x")
	if err != nil || ref != "origin/feat/x" || fallback != "" {
		t.Fatalf("want origin/feat/x, got %q fallback %q err %v", ref, fallback, err)
	}
}

func TestResolveUpdateRefFailsWithoutRemoteDefault(t *testing.T) {
	work, _ := gitRepo(t)
	ctx := context.Background()
	runStep(ctx, work, "git", gitBin(), "checkout", "-q", "-b", "local-only")
	// Drop origin/HEAD so no default branch is discoverable.
	runStep(ctx, work, "git", gitBin(), "symbolic-ref", "--delete", "refs/remotes/origin/HEAD")
	_, _, err := resolveUpdateRef(ctx, work, "local-only")
	if err == nil {
		t.Fatal("expected an error when the branch is unpublished and origin/HEAD is unset")
	}
	if !strings.Contains(err.Error(), "local-only") {
		t.Fatalf("error should name the branch, got: %v", err)
	}
}

// End-to-end: run the real selfUpdate against a clone sitting on an
// unpublished branch — the exact shape that produced
// "could not resolve HEAD or origin/fix/mobile-header-crowding".
// Opt-in because it runs the whole gate plus a build.
func TestSelfUpdateOnUnpublishedBranch(t *testing.T) {
	if os.Getenv("WEBCHAT_UPDATE_E2E") != "1" {
		t.Skip("set WEBCHAT_UPDATE_E2E=1 to exercise the full /update path")
	}
	repo, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), updateTimeout)
	defer cancel()

	base := t.TempDir()
	clone := filepath.Join(base, "clone")
	if st := runStep(ctx, base, "clone", gitBin(), "clone", "-q", repo, clone); !st.OK {
		t.Fatalf("clone failed: %s", st.Out)
	}
	if st := runStep(ctx, clone, "branch", gitBin(), "checkout", "-q", "-b", "fix/mobile-header-crowding"); !st.OK {
		t.Fatalf("checkout -b failed: %s", st.Out)
	}

	rep := selfUpdate(ctx, clone)
	if !rep.OK {
		t.Fatalf("update should succeed on an unpublished branch, got %q (steps: %+v)", rep.Message, rep.Steps)
	}
	if strings.Contains(rep.Message, "could not resolve") {
		t.Fatalf("resolved the wrong ref: %q", rep.Message)
	}
	if _, err := os.Stat(filepath.Join(clone, "bin", "webchat")); err != nil {
		t.Fatalf("expected a built binary in the clone: %v", err)
	}
}

func TestAheadBehindCounts(t *testing.T) {
	work, remote := gitRepo(t)
	ctx := context.Background()

	ahead, behind := aheadBehind(ctx, work, "origin/main")
	if ahead != 0 || behind != 0 {
		t.Fatalf("fresh clone should be level, got ahead=%d behind=%d", ahead, behind)
	}

	// Local commit: one ahead, nothing behind, and /update must not call the
	// push a merge failure.
	if err := os.WriteFile(filepath.Join(work, "f.txt"), []byte("two\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "f.txt"}, {"commit", "-q", "-m", "two"}} {
		if st := runStep(ctx, work, "git", gitBin(), args...); !st.OK {
			t.Fatalf("git %v: %s", args, st.Out)
		}
	}
	if ahead, behind = aheadBehind(ctx, work, "origin/main"); ahead != 1 || behind != 0 {
		t.Fatalf("want ahead=1 behind=0, got ahead=%d behind=%d", ahead, behind)
	}
	msg := upToDateMessage("abc1234", "origin/main", ahead, "")
	if !strings.Contains(msg, "ahead of origin/main") {
		t.Fatalf("message should explain the unpushed commit: %q", msg)
	}

	// Move the remote forward and confirm the clone reads as behind.
	other := filepath.Join(t.TempDir(), "other")
	if st := runStep(ctx, t.TempDir(), "clone", gitBin(), "clone", "-q", remote, other); !st.OK {
		t.Fatalf("clone: %s", st.Out)
	}
	if err := os.WriteFile(filepath.Join(other, "g.txt"), []byte("three\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "g.txt"}, {"commit", "-q", "-m", "three"}, {"push", "-q", "origin", "main"}} {
		if st := runStep(ctx, other, "git", gitBin(), args...); !st.OK {
			t.Fatalf("git %v: %s", args, st.Out)
		}
	}
	if st := runStep(ctx, work, "fetch", gitBin(), "fetch", "-q", "origin"); !st.OK {
		t.Fatalf("fetch: %s", st.Out)
	}
	if ahead, behind = aheadBehind(ctx, work, "origin/main"); behind < 1 {
		t.Fatalf("want at least one commit behind after the remote moved, got ahead=%d behind=%d", ahead, behind)
	}
}
