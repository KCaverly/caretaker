package repo

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestWorktreeLifecycle exercises discovery + create/list/status/remove against a
// real temporary git repo.
func TestWorktreeLifecycle(t *testing.T) {
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
		if _, err := git(dir, args...); err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
	}
	runGit(repoDir, "init", "-b", "main")
	runGit(repoDir, "config", "user.email", "t@t.t")
	runGit(repoDir, "config", "user.name", "t")
	if err := os.WriteFile(filepath.Join(repoDir, "README"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(repoDir, "add", ".")
	runGit(repoDir, "commit", "-m", "init")

	// Discovery finds the repo.
	repos, err := DiscoverRepos(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(repos) != 1 || repos[0].Name != "demo" {
		t.Fatalf("DiscoverRepos = %+v", repos)
	}
	r := repos[0]

	// Create a worktree.
	wt, err := CreateWorktree(r, ".worktrees/feat", "feat", "")
	if err != nil {
		t.Fatal(err)
	}
	if wt.Branch != "feat" {
		t.Fatalf("created worktree branch = %q", wt.Branch)
	}
	if _, err := os.Stat(wt.Path); err != nil {
		t.Fatalf("worktree dir missing: %v", err)
	}

	// List shows main + the new worktree.
	wts, err := ListWorktrees(r)
	if err != nil {
		t.Fatal(err)
	}
	if len(wts) != 2 || !wts[0].IsMain || wts[1].Name != "feat" {
		t.Fatalf("ListWorktrees = %+v", wts)
	}

	// Branch tip times: one call covers every branch.
	tips, err := BranchTipTimes(r)
	if err != nil {
		t.Fatal(err)
	}
	if tips["main"] == 0 || tips["feat"] == 0 {
		t.Fatalf("expected tip times for main and feat, got %v", tips)
	}

	// Status: clean, then dirty.
	st, err := WorktreeStatus(wts[1])
	if err != nil || st.Dirty {
		t.Fatalf("expected clean worktree, got %+v err=%v", st, err)
	}
	if err := os.WriteFile(filepath.Join(wt.Path, "scratch"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if st, _ := WorktreeStatus(wts[1]); !st.Dirty {
		t.Fatal("expected dirty worktree after writing a file")
	}

	// Remove worktree + branch.
	if err := RemoveWorktree(r, wts[1], true); err != nil {
		t.Fatal(err)
	}
	if wts, _ := ListWorktrees(r); len(wts) != 1 {
		t.Fatalf("expected only main worktree after remove, got %+v", wts)
	}
}
