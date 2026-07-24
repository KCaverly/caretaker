package repo

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestCreateWorktreeFetchesOrigin verifies that the default creation path does
// not branch from a stale local main when origin/main has advanced.
func TestCreateWorktreeFetchesOrigin(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}

	root := t.TempDir()
	remote := filepath.Join(root, "remote.git")
	seed := filepath.Join(root, "seed")
	local := filepath.Join(root, "local")
	for _, dir := range []string{remote, seed} {
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	run := func(dir string, args ...string) {
		t.Helper()
		if _, err := Git(dir, args...); err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
	}
	run(remote, "init", "--bare")
	run(seed, "init", "-b", "main")
	run(seed, "config", "user.email", "t@t.t")
	run(seed, "config", "user.name", "t")
	run(seed, "commit", "--allow-empty", "-m", "initial")
	run(seed, "remote", "add", "origin", remote)
	run(seed, "push", "-u", "origin", "main")
	run(root, "clone", remote, local)
	run(local, "checkout", "main")

	// Advance the remote without fetching in local, leaving local main stale.
	run(seed, "commit", "--allow-empty", "-m", "remote advance")
	run(seed, "push", "origin", "main")

	r := Repo{Name: "local", Path: local}
	wt, err := CreateWorktree(r, ".worktrees/feat", "feat", "")
	if err != nil {
		t.Fatal(err)
	}
	subject, err := Git(wt.Path, "log", "-1", "--format=%s")
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(subject); got != "remote advance" {
		t.Fatalf("new worktree tip = %q, want fetched origin/main tip", got)
	}
}

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
		if _, err := Git(dir, args...); err != nil {
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

	// Branch tips: one call covers every branch's time and subject.
	tips, _, err := BranchTips(r, "main")
	if err != nil {
		t.Fatal(err)
	}
	if tips["main"].Time == 0 || tips["feat"].Time == 0 {
		t.Fatalf("expected tip times for main and feat, got %v", tips)
	}
	if tips["main"].Subject != "init" {
		t.Fatalf("expected main tip subject %q, got %q", "init", tips["main"].Subject)
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

// TestAheadBehindAndDiffstat exercises the work-state git calls against a real
// repo: a worktree branched off main, advanced and rewound relative to it, then
// dirtied, so ahead/behind and the uncommitted diffstat can be checked end to
// end (and the left/right rev-list order pinned down).
func TestAheadBehindAndDiffstat(t *testing.T) {
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
		if _, err := Git(dir, args...); err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
	}
	runGit(repoDir, "init", "-b", "main")
	runGit(repoDir, "config", "user.email", "t@t.t")
	runGit(repoDir, "config", "user.name", "t")
	if err := os.WriteFile(filepath.Join(repoDir, "f"), []byte("a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(repoDir, "add", ".")
	runGit(repoDir, "commit", "-m", "init")

	r := Repo{Name: "demo", Path: repoDir}
	wt, err := CreateWorktree(r, ".worktrees/feat", "feat", "")
	if err != nil {
		t.Fatal(err)
	}

	// The main worktree never has an ahead/behind reading.
	wts, _ := ListWorktrees(r)
	if _, _, ok := AheadBehind(wts[0], "main"); ok {
		t.Fatalf("main worktree should report ahead/behind unavailable")
	}
	// An empty mainBranch also yields unavailable.
	if _, _, ok := AheadBehind(wt, ""); ok {
		t.Fatalf("empty mainBranch should report unavailable")
	}

	// Fresh branch: level with main.
	if a, b, ok := AheadBehind(wt, "main"); !ok || a != 0 || b != 0 {
		t.Fatalf("fresh feat: got ahead=%d behind=%d ok=%v, want 0/0/true", a, b, ok)
	}

	// Advance feat by two commits (ahead=2), then add a commit on main the feat
	// worktree doesn't have (behind=1).
	runGit(wt.Path, "commit", "-m", "f1", "--allow-empty")
	runGit(wt.Path, "commit", "-m", "f2", "--allow-empty")
	runGit(repoDir, "commit", "-m", "m1", "--allow-empty")
	if a, b, ok := AheadBehind(wt, "main"); !ok || a != 2 || b != 1 {
		t.Fatalf("diverged feat: got ahead=%d behind=%d ok=%v, want 2/1/true", a, b, ok)
	}

	// BranchTips folds the same ahead/behind into its single for-each-ref pass
	// (the fast path the deck load uses). On git 2.41+ it must agree with the
	// per-worktree AheadBehind above.
	if tips, ab, err := BranchTips(r, "main"); err != nil {
		t.Fatalf("BranchTips: %v", err)
	} else if ab {
		if tip := tips["feat"]; !tip.HasBase || tip.Ahead != 2 || tip.Behind != 1 {
			t.Fatalf("BranchTips feat = {ahead=%d behind=%d hasBase=%v}, want 2/1/true", tip.Ahead, tip.Behind, tip.HasBase)
		}
	}

	// Clean tree: no uncommitted diffstat. Then modify a tracked file.
	if add, del, err := UncommittedDiffstat(wt); err != nil || add != 0 || del != 0 {
		t.Fatalf("clean feat: got add=%d del=%d err=%v, want 0/0/nil", add, del, err)
	}
	if err := os.WriteFile(filepath.Join(wt.Path, "f"), []byte("b\nc\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// One line removed ("a"), two added ("b","c").
	if add, del, err := UncommittedDiffstat(wt); err != nil || add != 2 || del != 1 {
		t.Fatalf("dirty feat: got add=%d del=%d err=%v, want 2/1/nil", add, del, err)
	}
}

// TestRemoveWorktreeKeepsBranch verifies deleteBranch=false removes the working
// tree but leaves its branch behind, so the user can re-check it out later.
func TestRemoveWorktreeKeepsBranch(t *testing.T) {
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
		if _, err := Git(dir, args...); err != nil {
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

	r := Repo{Name: "demo", Path: repoDir}
	wt, err := CreateWorktree(r, ".worktrees/feat", "feat", "")
	if err != nil {
		t.Fatal(err)
	}

	// Remove keeping the branch.
	if err := RemoveWorktree(r, wt, false); err != nil {
		t.Fatal(err)
	}
	if wts, _ := ListWorktrees(r); len(wts) != 1 {
		t.Fatalf("expected only main worktree after remove, got %+v", wts)
	}
	// The branch tip must survive.
	tips, _, err := BranchTips(r, "main")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := tips["feat"]; !ok {
		t.Fatalf("expected branch 'feat' to survive removal, tips=%v", tips)
	}
}

// commitDiffRepo builds a throwaway git repo with two commits: a root commit
// adding greet.txt, then a tip commit that rewrites one of its lines and adds a
// second file. It returns the repo dir plus the root and tip SHAs.
func commitDiffRepo(t *testing.T) (dir, root, tip string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir = t.TempDir()
	run := func(args ...string) {
		t.Helper()
		if _, err := Git(dir, args...); err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
	}
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	head := func() string {
		t.Helper()
		out, err := Git(dir, "rev-parse", "HEAD")
		if err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(out)
	}

	run("init", "-b", "main")
	run("config", "user.email", "t@t.t")
	run("config", "user.name", "t")

	write("greet.txt", "alpha\nbravo\ncharlie\n")
	run("add", ".")
	run("commit", "-m", "root")
	root = head()

	write("greet.txt", "alpha\nBRAVO\ncharlie\n")
	write("extra.txt", "new file\n")
	run("add", ".")
	run("commit", "-m", "tip")
	tip = head()
	return dir, root, tip
}

// statByPath indexes a numstat result by path so assertions don't depend on
// git's ordering.
func statByPath(stats []FileStat) map[string]FileStat {
	m := make(map[string]FileStat, len(stats))
	for _, fs := range stats {
		m[fs.Path] = fs
	}
	return m
}

// TestDiffCommitAndNumstatCommit covers the ordinary case: a commit with a
// parent diffs against it, showing only what that one commit changed.
func TestDiffCommitAndNumstatCommit(t *testing.T) {
	dir, _, tip := commitDiffRepo(t)

	body, err := DiffCommit(dir, tip)
	if err != nil {
		t.Fatalf("DiffCommit: %v", err)
	}
	for _, want := range []string{
		"diff --git a/greet.txt b/greet.txt",
		"-bravo",
		"+BRAVO",
		"extra.txt",
		"+new file",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("tip patch missing %q:\n%s", want, body)
		}
	}
	// The parent's own content is untouched by this commit, so it must not leak
	// into the patch as an addition.
	if strings.Contains(body, "+alpha") || strings.Contains(body, "+charlie") {
		t.Errorf("tip patch should only carry this commit's changes:\n%s", body)
	}

	stats, err := NumstatCommit(dir, tip)
	if err != nil {
		t.Fatalf("NumstatCommit: %v", err)
	}
	if len(stats) != 2 {
		t.Fatalf("expected 2 file stats, got %d: %+v", len(stats), stats)
	}
	byPath := statByPath(stats)
	if fs, ok := byPath["greet.txt"]; !ok || fs.Add != 1 || fs.Del != 1 || fs.Binary {
		t.Errorf("greet.txt stat = %+v, want +1 −1", byPath["greet.txt"])
	}
	if fs, ok := byPath["extra.txt"]; !ok || fs.Add != 1 || fs.Del != 0 || fs.Binary {
		t.Errorf("extra.txt stat = %+v, want +1 −0", byPath["extra.txt"])
	}
}

// TestDiffCommitRootFallback covers the defensive path: a root commit has no
// `sha^`, so both helpers fall back to `git show` and diff it against the empty
// tree rather than erroring out.
func TestDiffCommitRootFallback(t *testing.T) {
	dir, root, _ := commitDiffRepo(t)

	body, err := DiffCommit(dir, root)
	if err != nil {
		t.Fatalf("DiffCommit on the root commit: %v", err)
	}
	for _, want := range []string{
		"diff --git a/greet.txt b/greet.txt",
		"+alpha",
		"+bravo",
		"+charlie",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("root patch missing %q:\n%s", want, body)
		}
	}
	// The fallback uses `--format=`, so the commit message must not be in the body.
	if strings.Contains(body, "root") && !strings.Contains(body, "greet") {
		t.Errorf("root patch should carry the diff, not the log entry:\n%s", body)
	}
	// extra.txt only arrives in the tip commit.
	if strings.Contains(body, "extra.txt") {
		t.Errorf("root patch should not include the tip commit's file:\n%s", body)
	}

	stats, err := NumstatCommit(dir, root)
	if err != nil {
		t.Fatalf("NumstatCommit on the root commit: %v", err)
	}
	if len(stats) != 1 {
		t.Fatalf("expected 1 file stat for the root commit, got %d: %+v", len(stats), stats)
	}
	if fs := stats[0]; fs.Path != "greet.txt" || fs.Add != 3 || fs.Del != 0 || fs.Binary {
		t.Errorf("root stat = %+v, want greet.txt +3 −0", fs)
	}
}

// TestUntrackedFilesLeavesPathsLiteral is the regression guard for path quoting.
// Plain `git status --porcelain` wraps any path that isn't simple ASCII in
// double quotes and escapes non-ASCII bytes octally, so a file called
// `spaced name.txt` comes back as `"spaced name.txt"` and an accented one as
// `"unicod\303\251.txt"`. Both forms are useless: the viewer would print the
// quotes, and nothing can open the escaped path. -z suppresses quoting, and this
// test pins that by round-tripping through the filesystem — the paths returned
// have to be openable.
func TestUntrackedFilesLeavesPathsLiteral(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init", "-b", "main"},
		{"config", "user.email", "t@t.t"},
		{"config", "user.name", "t"},
	} {
		if _, err := Git(dir, args...); err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
	}
	// A committed file so the repo has a HEAD, then untracked files whose names
	// git would otherwise quote.
	if err := os.WriteFile(filepath.Join(dir, "tracked.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Git(dir, "add", "."); err != nil {
		t.Fatal(err)
	}
	if _, err := Git(dir, "commit", "-m", "root"); err != nil {
		t.Fatal(err)
	}

	want := []string{"plain.txt", "spaced name.txt", "unicodé.txt"}
	for _, name := range want {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("new\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	got, err := UntrackedFiles(Worktree{Path: dir})
	if err != nil {
		t.Fatalf("UntrackedFiles: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("expected %d untracked paths, got %d: %q", len(want), len(got), got)
	}
	for _, name := range want {
		found := false
		for _, g := range got {
			if g == name {
				found = true
			}
		}
		if !found {
			t.Errorf("untracked path %q missing or mangled; got %q", name, got)
		}
	}
	// The real proof: every returned path must resolve on disk as-is.
	for _, g := range got {
		if _, err := os.Stat(filepath.Join(dir, g)); err != nil {
			t.Errorf("returned path %q is not openable: %v", g, err)
		}
	}
}
