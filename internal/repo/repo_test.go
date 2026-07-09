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

func TestValidateWorktreeName(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		wantErr bool
	}{
		// Accepted.
		{"simple", "feature-x", false},
		{"interior slash namespacing", "feature/foo", false},
		{"digits and dashes", "fix-123", false},
		{"underscore", "my_branch", false},
		{"nested namespace", "team/api/login", false},
		// Rejected.
		{"empty", "", true},
		{"whitespace only", "   ", true},
		{"dotdot traversal", "../evil", true},
		{"dotdot interior", "a/../b", true},
		{"leading dash", "-rf", true},
		{"leading slash", "/abs", true},
		{"trailing slash", "feature/", true},
		{"dot lock suffix", "foo.lock", true},
		{"leading dot", ".hidden", true},
		{"dot after slash", "feature/.hidden", true},
		{"interior space", "foo bar", true},
		{"tilde", "foo~1", true},
		{"caret", "foo^", true},
		{"colon", "foo:bar", true},
		{"question", "foo?", true},
		{"star", "foo*", true},
		{"open bracket", "foo[", true},
		{"backslash", `foo\bar`, true},
		{"tab control char", "foo\tbar", true},
		{"del control char", "foo\x7f", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateWorktreeName(tc.in)
			if tc.wantErr && err == nil {
				t.Errorf("ValidateWorktreeName(%q) = nil, want error", tc.in)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("ValidateWorktreeName(%q) = %v, want nil", tc.in, err)
			}
		})
	}
}
