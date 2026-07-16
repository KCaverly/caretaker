package repo

import "testing"

func TestParseAheadBehind(t *testing.T) {
	cases := []struct {
		name       string
		in         string
		wantBehind int
		wantAhead  int
		wantOK     bool
	}{
		// git prints "left\tright" = "behind\tahead".
		{"ahead only", "0\t3", 0, 3, true},
		{"behind only", "2\t0", 2, 0, true},
		{"diverged", "2\t3", 2, 3, true},
		{"level", "0\t0", 0, 0, true},
		{"trailing newline", "2\t3\n", 2, 3, true},
		{"space separated", "1 4", 1, 4, true},
		{"empty", "", 0, 0, false},
		{"one field", "5", 0, 0, false},
		{"non-numeric", "a\tb", 0, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			behind, ahead, ok := parseAheadBehind(tc.in)
			if ok != tc.wantOK || behind != tc.wantBehind || ahead != tc.wantAhead {
				t.Errorf("parseAheadBehind(%q) = (behind=%d, ahead=%d, ok=%v), want (%d, %d, %v)",
					tc.in, behind, ahead, ok, tc.wantBehind, tc.wantAhead, tc.wantOK)
			}
		})
	}
}

func TestParseShortstat(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		wantAdd int
		wantDel int
	}{
		{"both", " 3 files changed, 12 insertions(+), 4 deletions(-)", 12, 4},
		{"insertions only", " 1 file changed, 2 insertions(+)", 2, 0},
		{"deletions only", " 1 file changed, 5 deletions(-)", 0, 5},
		{"singular units", " 1 file changed, 1 insertion(+), 1 deletion(-)", 1, 1},
		{"empty", "", 0, 0},
		{"whitespace only", "   \n", 0, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			add, del := parseShortstat(tc.in)
			if add != tc.wantAdd || del != tc.wantDel {
				t.Errorf("parseShortstat(%q) = (add=%d, del=%d), want (%d, %d)",
					tc.in, add, del, tc.wantAdd, tc.wantDel)
			}
		})
	}
}

func TestParseNumstat(t *testing.T) {
	out := "12\t4\tmain.go\n" +
		"-\t-\tassets/logo.png\n" +
		"5\t3\told.go => new.go\n" +
		"0\t0\tempty.txt\n" +
		"\n" + // blank line skipped
		"garbage-line-no-tabs\n" // too few fields, skipped
	stats := parseNumstat(out)
	if len(stats) != 4 {
		t.Fatalf("expected 4 file stats (blank + malformed skipped), got %d: %+v", len(stats), stats)
	}
	if stats[0].Path != "main.go" || stats[0].Add != 12 || stats[0].Del != 4 || stats[0].Binary {
		t.Errorf("normal stat parsed wrong: %+v", stats[0])
	}
	if !stats[1].Binary || stats[1].Add != 0 || stats[1].Del != 0 || stats[1].Path != "assets/logo.png" {
		t.Errorf("binary stat should be Binary with zero counts: %+v", stats[1])
	}
	if stats[2].Path != "old.go => new.go" || stats[2].Add != 5 || stats[2].Del != 3 {
		t.Errorf("rename path should pass through verbatim: %+v", stats[2])
	}
	if stats[3].Add != 0 || stats[3].Del != 0 || stats[3].Binary {
		t.Errorf("zero-change stat parsed wrong: %+v", stats[3])
	}
}

func TestParseUntracked(t *testing.T) {
	out := " M tracked-modified.go\n" +
		"?? new-file.txt\n" +
		"A  staged.go\n" +
		"?? dir/nested-new.go\n" +
		"?? path with spaces.md\n"
	got := parseUntracked(out)
	want := []string{"new-file.txt", "dir/nested-new.go", "path with spaces.md"}
	if len(got) != len(want) {
		t.Fatalf("expected %d untracked paths, got %d: %v", len(want), len(got), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("untracked[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestParseBranchTips(t *testing.T) {
	// Three NUL-fenced fields per line; subjects carry spaces (and even a stray
	// '·'), which the NUL separator preserves where spaces couldn't. The last
	// line has an unparseable time and must be skipped.
	out := "main\x001700000000\x00Initial commit\n" +
		"feat-login\x001700000500\x00Add the login form · wip\n" +
		"broken\x00notanumber\x00nope\n"
	tips := parseBranchTips(out)
	if len(tips) != 2 {
		t.Fatalf("expected 2 parsed tips (broken one skipped), got %d: %v", len(tips), tips)
	}
	if tips["feat-login"].Subject != "Add the login form · wip" {
		t.Errorf("subject with spaces mangled: %q", tips["feat-login"].Subject)
	}
	if tips["feat-login"].Time != 1700000500 {
		t.Errorf("feat-login time = %d, want 1700000500", tips["feat-login"].Time)
	}
	if tips["main"].Subject != "Initial commit" {
		t.Errorf("main subject = %q", tips["main"].Subject)
	}
	if _, ok := tips["broken"]; ok {
		t.Errorf("line with unparseable time should have been skipped")
	}
}

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
