package stack

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/KCaverly/caretaker/internal/repo"
)

// TestLocalGathering exercises the local git gathering (localCommits +
// remoteBranches) against a real scripted repo, then feeds the results through
// the pure reconciler with synthetic PRs. It makes no GitHub calls. It follows
// internal/repo/integration_test.go's convention: skip when git is absent, no
// build tag.
func TestLocalGathering(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}

	root := t.TempDir()
	repoDir := filepath.Join(root, "demo")
	if err := os.Mkdir(repoDir, 0o755); err != nil {
		t.Fatal(err)
	}
	runGit := func(dir string, args ...string) {
		t.Helper()
		if _, err := repo.Git(dir, args...); err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
	}
	runGit(repoDir, "init", "-b", "main")
	runGit(repoDir, "config", "user.email", "t@t.t")
	runGit(repoDir, "config", "user.name", "t")
	if err := os.WriteFile(filepath.Join(repoDir, "f"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(repoDir, "add", ".")
	runGit(repoDir, "commit", "-m", "base")

	r := repo.Repo{Name: "demo", Path: repoDir}
	wt, err := repo.CreateWorktree(r, ".worktrees/feat", "feat", "")
	if err != nil {
		t.Fatal(err)
	}

	// Three commits on the stack: trailer / no trailer / trailer, bottom-first.
	runGit(wt.Path, "commit", "--allow-empty", "-m", "bottom\n\nct-stack-id: aaaaaaaa")
	runGit(wt.Path, "commit", "--allow-empty", "-m", "middle no trailer")
	runGit(wt.Path, "commit", "--allow-empty", "-m", "top\n\nct-stack-id: bbbbbbbb")

	commits, err := localCommits(wt.Path, "main")
	if err != nil {
		t.Fatal(err)
	}
	if len(commits) != 3 {
		t.Fatalf("expected 3 commits, got %d: %+v", len(commits), commits)
	}
	if commits[0].Subject != "bottom" || commits[0].StackID != "aaaaaaaa" {
		t.Errorf("bottom commit wrong: %+v", commits[0])
	}
	if commits[1].StackID != "" {
		t.Errorf("middle commit should have no stack id: %+v", commits[1])
	}
	if commits[2].StackID != "bbbbbbbb" {
		t.Errorf("top commit stack id wrong: %+v", commits[2])
	}
	if !validStackID(commits[0].StackID) {
		t.Errorf("bottom stack id should be valid: %q", commits[0].StackID)
	}

	// Register a remote stack branch for the bottom commit at its real SHA, so
	// remoteBranches reads it back and reconcile sees it in sync.
	bottomSHA := commits[0].SHA
	runGit(wt.Path, "update-ref", "refs/remotes/origin/ct/feat/aaaaaaaa", bottomSHA)

	remotes, err := remoteBranches(wt.Path, "feat")
	if err != nil {
		t.Fatal(err)
	}
	if got := remotes["aaaaaaaa"]; got != bottomSHA {
		t.Errorf("remote tip for aaaaaaaa = %q, want %q", got, bottomSHA)
	}
	if len(remotes) != 1 {
		t.Errorf("expected exactly one remote stack branch, got %v", remotes)
	}

	// Reconcile the real local data with a synthetic open PR on the bottom commit.
	prs := []prRecord{{
		Number: 1, State: "OPEN", Head: "ct/feat/aaaaaaaa", Base: "main",
		Review: "APPROVED", Checks: Checks{Summary: "passing", Failing: []string{}},
	}}
	stk, out := reconcile("feat", "main", commits, remotes, prs)
	if out[0].State != StateOpen {
		t.Errorf("bottom commit state = %q, want open", out[0].State)
	}
	if out[1].State != StateUnsubmitted {
		t.Errorf("middle commit state = %q, want unsubmitted", out[1].State)
	}
	if out[2].State != StateUnpushed {
		t.Errorf("top commit state = %q, want unpushed", out[2].State)
	}
	// An unsubmitted commit sitting in the stack means the next action is submit.
	if stk.NextAction != "submit" {
		t.Errorf("next_action = %q, want submit", stk.NextAction)
	}
	if !strings.HasPrefix(*out[0].RemoteBranch, "ct/feat/") {
		t.Errorf("remote_branch = %v, want ct/feat/… prefix", out[0].RemoteBranch)
	}
}

// TestLocalCommitsUsesFetchedMain verifies stack discovery stays aligned with
// worktree creation when the primary worktree's local main is behind its
// remote-tracking branch. The landed remote-only commit must not be mistaken
// for the bottom of the stack.
func TestLocalCommitsUsesFetchedMain(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}

	dir := t.TempDir()
	runGit := func(args ...string) {
		t.Helper()
		if _, err := repo.Git(dir, args...); err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
	}
	runGit("init", "-b", "main")
	runGit("config", "user.email", "t@t.t")
	runGit("config", "user.name", "t")
	runGit("commit", "--allow-empty", "-m", "local main")
	runGit("checkout", "-b", "feat")
	runGit("commit", "--allow-empty", "-m", "landed remotely\n\nct-stack-id: aaaaaaaa")
	remoteMain, err := repo.Git(dir, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	runGit("update-ref", "refs/remotes/origin/main", strings.TrimSpace(remoteMain))
	runGit("commit", "--allow-empty", "-m", "actual stack commit\n\nct-stack-id: bbbbbbbb")

	commits, err := localCommits(dir, "main")
	if err != nil {
		t.Fatal(err)
	}
	if len(commits) != 1 || commits[0].Subject != "actual stack commit" {
		t.Fatalf("localCommits = %+v, want only the commit after origin/main", commits)
	}
}
