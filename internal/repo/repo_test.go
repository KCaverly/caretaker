package repo

import "testing"

func TestParseWorktreeList(t *testing.T) {
	out := `worktree /home/u/repos/caretaker
HEAD abc123
branch refs/heads/main

worktree /home/u/repos/caretaker/.worktrees/feat-login
HEAD def456
branch refs/heads/feat-login

worktree /home/u/repos/caretaker/.worktrees/detached
HEAD 789aaa
detached
`
	wts := parseWorktreeList(out)
	if len(wts) != 3 {
		t.Fatalf("got %d worktrees, want 3", len(wts))
	}
	if wts[0].Path != "/home/u/repos/caretaker" || wts[0].Branch != "main" {
		t.Fatalf("main worktree parsed wrong: %+v", wts[0])
	}
	if wts[1].Branch != "feat-login" {
		t.Fatalf("branch short name not stripped: %q", wts[1].Branch)
	}
	if wts[2].Branch != "" {
		t.Fatalf("detached worktree should have empty branch, got %q", wts[2].Branch)
	}
}

func TestParseWorktreeListEmpty(t *testing.T) {
	if got := parseWorktreeList(""); len(got) != 0 {
		t.Fatalf("expected no worktrees, got %d", len(got))
	}
}
