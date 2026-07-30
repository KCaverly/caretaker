package repo

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

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

// TestParseUntracked feeds the NUL-fenced shape `--porcelain -z` actually emits.
// The spaced and non-ASCII paths are the point: under plain --porcelain git would
// hand back `"path with spaces.md"` and `"unicod\303\251.txt"` — quoted, and in
// the second case octal-escaped — which is unusable both for display and for
// opening the file. -z leaves them literal.
func TestParseUntracked(t *testing.T) {
	out := " M tracked-modified.go\x00" +
		"?? new-file.txt\x00" +
		"A  staged.go\x00" +
		"?? dir/nested-new.go\x00" +
		"?? path with spaces.md\x00" +
		"?? unicodé.txt\x00"
	got := parseUntracked(out)
	want := []string{"new-file.txt", "dir/nested-new.go", "path with spaces.md", "unicodé.txt"}
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
	tips := parseBranchTips(out, false)
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

func TestParseBranchTipsAheadBehind(t *testing.T) {
	// Four NUL-fenced fields per line; the fourth is the ahead-behind atom's
	// "<ahead> <behind>" pair. The main branch compared to itself is 0/0; feat
	// is 2 ahead / 1 behind; the empty pair (a ref git couldn't compare) keeps
	// its tip but reports no base.
	out := "main\x001700000000\x00Initial commit\x000 0\n" +
		"feat\x001700000500\x00Add feature\x002 1\n" +
		"orphan\x001700000900\x00Unrelated\x00\n"
	tips := parseBranchTips(out, true)
	if len(tips) != 3 {
		t.Fatalf("expected 3 tips, got %d: %v", len(tips), tips)
	}
	if tip := tips["feat"]; !tip.HasBase || tip.Ahead != 2 || tip.Behind != 1 {
		t.Errorf("feat = {ahead=%d behind=%d hasBase=%v}, want 2/1/true", tip.Ahead, tip.Behind, tip.HasBase)
	}
	if tip := tips["main"]; !tip.HasBase || tip.Ahead != 0 || tip.Behind != 0 {
		t.Errorf("main = {ahead=%d behind=%d hasBase=%v}, want 0/0/true", tip.Ahead, tip.Behind, tip.HasBase)
	}
	if tip := tips["orphan"]; tip.HasBase {
		t.Errorf("orphan with empty ahead-behind should report HasBase=false, got %+v", tip)
	}
	if tips["orphan"].Subject != "Unrelated" {
		t.Errorf("orphan tip should still load its subject, got %q", tips["orphan"].Subject)
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

// mkRepo creates a directory with a .git marker, which is all DiscoverRepos
// requires to recognise a repository.
func mkRepo(t *testing.T, parent, name string) string {
	t.Helper()
	dir := filepath.Join(parent, name)
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

// TestDiscoverReposInMergesRoots covers issue #55: repos have to come from every
// configured tree, not just one.
func TestDiscoverReposInMergesRoots(t *testing.T) {
	base := t.TempDir()
	work := filepath.Join(base, "work")
	personal := filepath.Join(base, "personal")
	mkRepo(t, work, "alpha")
	mkRepo(t, personal, "beta")
	// Not a repo, and a dotdir: neither should be listed.
	if err := os.MkdirAll(filepath.Join(work, "notes"), 0o755); err != nil {
		t.Fatal(err)
	}
	mkRepo(t, work, ".hidden")

	repos, err := DiscoverReposIn([]string{work, personal})
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, r := range repos {
		names = append(names, r.Name)
	}
	if want := []string{"alpha", "beta"}; !reflect.DeepEqual(names, want) {
		t.Errorf("names = %v, want %v", names, want)
	}
}

// TestDiscoverReposInDisambiguatesNames is the correctness half of multi-root
// support. Repo.Name is half of the workspace key that identifies sessions and
// persisted state, so two repos called the same thing in different trees would
// share both.
func TestDiscoverReposInDisambiguatesNames(t *testing.T) {
	base := t.TempDir()
	work := filepath.Join(base, "work")
	personal := filepath.Join(base, "personal")
	mkRepo(t, work, "api")
	mkRepo(t, personal, "api")
	mkRepo(t, work, "solo") // no collision: keeps its bare name

	repos, err := DiscoverReposIn([]string{work, personal})
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]string{}
	for _, r := range repos {
		if _, dup := byName[r.Name]; dup {
			t.Fatalf("duplicate display name %q: %+v", r.Name, repos)
		}
		byName[r.Name] = r.Path
	}
	if _, ok := byName["solo"]; !ok {
		t.Errorf("a non-colliding repo should keep its bare name: %v", byName)
	}
	for _, want := range []string{"work/api", "personal/api"} {
		if _, ok := byName[want]; !ok {
			t.Errorf("expected a disambiguated %q, got %v", want, byName)
		}
	}
}

// TestDiscoverReposInDeduplicatesAndTolerates covers the two edges: one repo
// reachable from two roots is listed once, and a root that vanished under a
// running ct must not hide the roots that are still there.
func TestDiscoverReposInDeduplicatesAndTolerates(t *testing.T) {
	base := t.TempDir()
	work := filepath.Join(base, "work")
	mkRepo(t, work, "alpha")

	repos, err := DiscoverReposIn([]string{work, work})
	if err != nil {
		t.Fatal(err)
	}
	if len(repos) != 1 {
		t.Errorf("the same root twice should list one repo, got %+v", repos)
	}

	repos, err = DiscoverReposIn([]string{work, filepath.Join(base, "gone")})
	if err != nil {
		t.Fatalf("a missing root must not fail discovery outright: %v", err)
	}
	if len(repos) != 1 || repos[0].Name != "alpha" {
		t.Errorf("surviving root's repos = %+v, want alpha", repos)
	}

	// With nothing found at all, the error is surfaced rather than swallowed.
	if _, err := DiscoverReposIn([]string{filepath.Join(base, "gone")}); err == nil {
		t.Error("expected an error when no root could be read")
	}
}

func TestPathTail(t *testing.T) {
	cases := []struct {
		path string
		n    int
		want string
	}{
		{"/home/kc/work/api", 1, "api"},
		{"/home/kc/work/api", 2, "work/api"},
		{"/home/kc/work/api", 3, "kc/work/api"},
		{"/home/kc/work/api", 9, "home/kc/work/api"},
		{"api", 2, "api"},
	}
	for _, tc := range cases {
		if got := pathTail(tc.path, tc.n); got != tc.want {
			t.Errorf("pathTail(%q, %d) = %q, want %q", tc.path, tc.n, got, tc.want)
		}
	}
}
