package stack

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/KCaverly/caretaker/internal/repo"
)

// TestInjectTrailersIntegration drives the real trailer rewrite against a scripted
// repo: it asserts the rewrite mints ids for untrailered commits, preserves each
// commit's author/date/tree and the working tree, moves the branch ref, and is a
// no-op on a second run. It makes no GitHub calls. Follows step-1's convention:
// skip when git is absent, no build tag.
func TestInjectTrailersIntegration(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := scriptStackRepo(t)

	before, err := localCommits(dir, "main")
	if err != nil {
		t.Fatal(err)
	}
	if len(before) != 3 {
		t.Fatalf("expected 3 commits, got %d", len(before))
	}
	// Snapshot author identity, author date, and tree of each commit by subject,
	// so we can prove the rewrite preserved them.
	type ident struct{ an, ae, ad, tree string }
	snap := map[string]ident{}
	for _, c := range before {
		snap[c.Subject] = ident{
			an:   gitField(t, dir, "%an", c.SHA),
			ae:   gitField(t, dir, "%ae", c.SHA),
			ad:   gitField(t, dir, "%aI", c.SHA),
			tree: gitField(t, dir, "%T", c.SHA),
		}
	}
	oldHead := gitField(t, dir, "%H", "HEAD")

	assigns, err := injectTrailers(dir, "feat", before)
	if err != nil {
		t.Fatalf("injectTrailers: %v", err)
	}
	// Two commits lacked ids (bottom + middle); the top already had one.
	if len(assigns) != 2 {
		t.Fatalf("expected 2 id assignments, got %d: %+v", len(assigns), assigns)
	}

	after, err := localCommits(dir, "main")
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	for _, c := range after {
		if !validStackID(c.StackID) {
			t.Errorf("commit %q still lacks a valid stack id after inject: %q", c.Subject, c.StackID)
		}
		if ids[c.StackID] {
			t.Errorf("duplicate id %q after inject", c.StackID)
		}
		ids[c.StackID] = true

		want := snap[c.Subject]
		if got := gitField(t, dir, "%an", c.SHA); got != want.an {
			t.Errorf("%q author name changed: %q -> %q", c.Subject, want.an, got)
		}
		if got := gitField(t, dir, "%ae", c.SHA); got != want.ae {
			t.Errorf("%q author email changed: %q -> %q", c.Subject, want.ae, got)
		}
		if got := gitField(t, dir, "%aI", c.SHA); got != want.ad {
			t.Errorf("%q author date changed: %q -> %q", c.Subject, want.ad, got)
		}
		if got := gitField(t, dir, "%T", c.SHA); got != want.tree {
			t.Errorf("%q tree changed: %q -> %q", c.Subject, want.tree, got)
		}
	}

	// The pre-existing trailer id on the top commit must survive verbatim.
	if after[2].StackID != "cccccccc" {
		t.Errorf("top commit id = %q, want the pre-existing cccccccc", after[2].StackID)
	}

	// The branch ref moved (update-ref), and the working tree stayed clean.
	if newHead := gitField(t, dir, "%H", "HEAD"); newHead == oldHead {
		t.Error("HEAD did not move after the rewrite")
	}
	if out, err := repo.Git(dir, "status", "--porcelain"); err != nil {
		t.Fatal(err)
	} else if strings.TrimSpace(out) != "" {
		t.Errorf("working tree should be clean after rewrite, got:\n%s", out)
	}

	// Re-running is a no-op: everything already has an id, so HEAD must not move.
	headBefore := gitField(t, dir, "%H", "HEAD")
	reAssigns, err := injectTrailers(dir, "feat", after)
	if err != nil {
		t.Fatalf("second injectTrailers: %v", err)
	}
	if len(reAssigns) != 0 {
		t.Errorf("second inject should assign nothing, got %+v", reAssigns)
	}
	if headAfter := gitField(t, dir, "%H", "HEAD"); headAfter != headBefore {
		t.Errorf("no-op inject moved HEAD: %s -> %s", headBefore, headAfter)
	}
}

// TestPushIntegration pushes stack branches to a local file:// bare "origin" and
// verifies force-with-lease creates and updates them. No GitHub calls.
func TestPushIntegration(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := scriptStackRepo(t)

	// A bare repo acts as origin, reachable by file:// URL.
	bare := filepath.Join(t.TempDir(), "origin.git")
	if _, err := repo.Git(dir, "init", "--bare", bare); err != nil {
		t.Fatalf("init bare: %v", err)
	}
	if _, err := repo.Git(dir, "remote", "add", "origin", "file://"+bare); err != nil {
		t.Fatalf("remote add: %v", err)
	}

	// Give every commit an id, then push each as its stack branch (all creates).
	commits, err := localCommits(dir, "main")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := injectTrailers(dir, "feat", commits); err != nil {
		t.Fatal(err)
	}
	commits, err = localCommits(dir, "main")
	if err != nil {
		t.Fatal(err)
	}

	for _, pc := range computePushes(commits, map[string]string{}, "feat") {
		if _, err := repo.Git(dir, pushArgs(pc.Branch, pc.SHA, pc.Expected, pc.Create)...); err != nil {
			t.Fatalf("push %s: %v", pc.Branch, err)
		}
	}

	// Every stack branch now exists on the bare remote at the local tip.
	for _, c := range commits {
		ref := "refs/heads/ct/feat/" + c.StackID
		out, err := repo.Git(bare, "rev-parse", ref)
		if err != nil {
			t.Fatalf("remote missing %s: %v", ref, err)
		}
		if strings.TrimSpace(out) != c.SHA {
			t.Errorf("remote %s = %s, want %s", ref, strings.TrimSpace(out), c.SHA)
		}
	}

	// Amend the top commit locally (changing its tree so the SHA really moves),
	// then a force-with-lease update should move the remote branch to the new tip.
	if err := os.WriteFile(filepath.Join(dir, "amended"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Git(dir, "add", "."); err != nil {
		t.Fatalf("add: %v", err)
	}
	if _, err := repo.Git(dir, "commit", "--amend", "--no-edit"); err != nil {
		t.Fatalf("amend: %v", err)
	}
	commits, err = localCommits(dir, "main")
	if err != nil {
		t.Fatal(err)
	}
	top := commits[len(commits)-1]
	remotes, err := remoteBranches(dir, "feat")
	if err != nil {
		t.Fatal(err)
	}
	pushes := computePushes(commits, remotes, "feat")
	if len(pushes) != 1 || pushes[0].Create || pushes[0].SHA != top.SHA {
		t.Fatalf("expected a single force-update for the amended top, got %+v", pushes)
	}
	if _, err := repo.Git(dir, pushArgs(pushes[0].Branch, pushes[0].SHA, pushes[0].Expected, pushes[0].Create)...); err != nil {
		t.Fatalf("force push: %v", err)
	}
	out, err := repo.Git(bare, "rev-parse", "refs/heads/ct/feat/"+top.StackID)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(out) != top.SHA {
		t.Errorf("force-updated remote tip = %s, want %s", strings.TrimSpace(out), top.SHA)
	}
}

// scriptStackRepo builds a repo with a `feat` worktree carrying three stack
// commits — two without a ct-stack-id trailer and a top one with a valid id — and
// returns the worktree path.
func scriptStackRepo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	repoDir := filepath.Join(root, "demo")
	if err := os.Mkdir(repoDir, 0o755); err != nil {
		t.Fatal(err)
	}
	run := func(dir string, args ...string) {
		t.Helper()
		if _, err := repo.Git(dir, args...); err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
	}
	run(repoDir, "init", "-b", "main")
	run(repoDir, "config", "user.email", "committer@t.t")
	run(repoDir, "config", "user.name", "Committer")
	if err := os.WriteFile(filepath.Join(repoDir, "f"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(repoDir, "add", ".")
	run(repoDir, "commit", "-m", "base")

	r := repo.Repo{Name: "demo", Path: repoDir}
	wt, err := repo.CreateWorktree(r, ".worktrees/feat", "feat", "")
	if err != nil {
		t.Fatal(err)
	}
	// Distinct author identity/date, so preservation is observable.
	run(wt.Path, "-c", "user.name=Author One", "-c", "user.email=author@x.y",
		"commit", "--allow-empty", "--date=2020-01-02T03:04:05", "-m", "bottom no trailer")
	run(wt.Path, "-c", "user.name=Author One", "-c", "user.email=author@x.y",
		"commit", "--allow-empty", "--date=2020-02-03T04:05:06", "-m", "middle no trailer")
	run(wt.Path, "commit", "--allow-empty", "-m", "top\n\nct-stack-id: cccccccc")
	return wt.Path
}

// gitField reads a single --format field from a commit, trimmed.
func gitField(t *testing.T, dir, format, rev string) string {
	t.Helper()
	out, err := repo.Git(dir, "show", "-s", "--format="+format, rev)
	if err != nil {
		t.Fatalf("git show %s %s: %v", format, rev, err)
	}
	return strings.TrimSpace(out)
}
