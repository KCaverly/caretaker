package stack

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/KCaverly/caretaker/internal/repo"
)

// TestRestackIntegration drives the git-side restack pipeline against a file://
// bare origin: a 3-commit stack A/B/C is submitted, A is squash-landed onto main
// (a squash-equivalent A′ pushed, its remote branch deleted), and then the
// landed-prefix rebase, main fast-forward, branch deletion, and survivor pushes
// run — with the GitHub half stubbed by synthetic prRecords, exactly as the
// existing integration tests avoid gh. It asserts A is dropped, B′/C′ reparent on
// origin/main with byte-identical messages/trailers, the working tree stays
// clean, the remote ct branches move to B′/C′, the landed branch is gone, and a
// rerun is a no-op.
func TestRestackIntegration(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	h := buildRestackStack(t, false)

	// Original commits (bottom-first) and their exact messages, captured pre-land.
	before, err := localCommits(h.dir, "main")
	if err != nil {
		t.Fatal(err)
	}
	if len(before) != 3 {
		t.Fatalf("expected 3 commits, got %d", len(before))
	}
	origA := before[0]
	msgB := rawMessage(t, h.dir, before[1].SHA)
	msgC := rawMessage(t, h.dir, before[2].SHA)

	// Squash-land A: a squash-equivalent A′ carrying A's message+trailer is
	// committed onto main in a clone and pushed, then A's remote branch is deleted
	// (as GitHub's post-merge branch cleanup would do).
	squashLand(t, h, "A\n\nct-stack-id: aaaaaaaa", "landed-a")
	if err := deleteRemoteBranch(h.dir, "ct/feat/aaaaaaaa"); err != nil {
		t.Fatalf("deleting landed branch: %v", err)
	}
	if _, err := repo.Git(h.dir, "fetch", "origin"); err != nil {
		t.Fatal(err)
	}
	originMain := revParse(t, h.dir, "origin/main")

	// Build the status the reconciler would produce: A merged, B/C open (B already
	// auto-retargeted onto main after A's branch was deleted).
	st := restackStatus(h.dir, before, []prRecord{
		mergedPR(41, "ct/feat/aaaaaaaa", "main"),
		openPR(42, "ct/feat/bbbbbbbb", "main"),
		openPR(43, "ct/feat/cccccccc", "ct/feat/bbbbbbbb"),
	})
	plan, nothing, err := planRestack(st)
	if err != nil {
		t.Fatalf("planRestack: %v", err)
	}
	if nothing {
		t.Fatal("planRestack reported nothing to restack, expected a landed prefix")
	}
	if plan.LastMergedSHA != origA.SHA {
		t.Errorf("LastMergedSHA = %s, want A's %s", plan.LastMergedSHA, origA.SHA)
	}
	if len(plan.MergedIDs) != 1 || plan.MergedIDs[0] != "aaaaaaaa" {
		t.Errorf("MergedIDs = %v, want [aaaaaaaa]", plan.MergedIDs)
	}
	if len(plan.Drops) != 1 || plan.Drops[0].Number != 41 {
		t.Errorf("Drops = %+v, want a single #41", plan.Drops)
	}

	// The rebase drops A and reparents B/C onto origin/main.
	preSHA := revParse(t, h.dir, "refs/heads/feat")
	if err := rebaseDropLanded(h.dir, "origin/main", plan.LastMergedSHA, "feat", preSHA); err != nil {
		t.Fatalf("rebaseDropLanded: %v", err)
	}
	if err := fastForwardMain(h.dir, "main"); err != nil {
		t.Fatalf("fastForwardMain: %v", err)
	}

	after, err := localCommits(h.dir, "main")
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != 2 {
		t.Fatalf("expected 2 commits after restack, got %d: %+v", len(after), after)
	}
	if after[0].Subject != "B" || after[0].StackID != "bbbbbbbb" {
		t.Errorf("survivor[0] = %+v, want B/bbbbbbbb", after[0])
	}
	if after[1].Subject != "C" || after[1].StackID != "cccccccc" {
		t.Errorf("survivor[1] = %+v, want C/cccccccc", after[1])
	}
	// A is dropped: the original A commit is no longer an ancestor of feat.
	if _, err := repo.Git(h.dir, "merge-base", "--is-ancestor", origA.SHA, "feat"); err == nil {
		t.Error("original A commit is still an ancestor of feat after restack")
	}
	// B′ parents directly on origin/main; messages and trailers are byte-identical.
	if parent := revParse(t, h.dir, after[0].SHA+"^"); parent != originMain {
		t.Errorf("B′ parent = %s, want origin/main %s", parent, originMain)
	}
	if got := rawMessage(t, h.dir, after[0].SHA); got != msgB {
		t.Errorf("B message changed:\n got %q\nwant %q", got, msgB)
	}
	if got := rawMessage(t, h.dir, after[1].SHA); got != msgC {
		t.Errorf("C message changed:\n got %q\nwant %q", got, msgC)
	}
	// The working tree is untouched (a rebase leaves no uncommitted changes).
	if out, err := repo.Git(h.dir, "status", "--porcelain"); err != nil {
		t.Fatal(err)
	} else if strings.TrimSpace(out) != "" {
		t.Errorf("working tree dirty after restack:\n%s", out)
	}

	// Delete the landed remote branch and push the rebased survivors.
	for _, id := range plan.MergedIDs {
		if err := deleteRemoteBranch(h.dir, "ct/feat/"+id); err != nil {
			t.Fatalf("delete landed branch %s: %v", id, err)
		}
	}
	remotes, err := remoteBranches(h.dir, "feat")
	if err != nil {
		t.Fatal(err)
	}
	pushes := computePushes(after, remotes, "feat")
	if len(pushes) != 2 {
		t.Fatalf("expected 2 survivor pushes, got %+v", pushes)
	}
	for _, pc := range pushes {
		if pc.Create {
			t.Errorf("survivor push should be a force-update, got a create: %+v", pc)
		}
		if _, err := repo.Git(h.dir, pushArgs(pc.Branch, pc.SHA, pc.Expected, pc.Create)...); err != nil {
			t.Fatalf("push %s: %v", pc.Branch, err)
		}
	}

	// The remote ct branches now point at B′/C′; the landed branch is gone.
	if got := revParse(t, h.bare, "refs/heads/ct/feat/bbbbbbbb"); got != after[0].SHA {
		t.Errorf("remote ct/feat/bbbbbbbb = %s, want B′ %s", got, after[0].SHA)
	}
	if got := revParse(t, h.bare, "refs/heads/ct/feat/cccccccc"); got != after[1].SHA {
		t.Errorf("remote ct/feat/cccccccc = %s, want C′ %s", got, after[1].SHA)
	}
	if _, err := repo.Git(h.bare, "rev-parse", "--verify", "refs/heads/ct/feat/aaaaaaaa"); err == nil {
		t.Error("landed remote branch ct/feat/aaaaaaaa still exists")
	}

	// Rerun: with A dropped and main caught up, there is nothing left to restack.
	after2, err := localCommits(h.dir, "main")
	if err != nil {
		t.Fatal(err)
	}
	remotes2, err := remoteBranches(h.dir, "feat")
	if err != nil {
		t.Fatal(err)
	}
	st2 := restackStatus(h.dir, after2, []prRecord{
		mergedPR(41, "ct/feat/aaaaaaaa", "main"), // still MERGED on GitHub, matches no local commit
		openPRSynced(42, "ct/feat/bbbbbbbb", "main", remotes2["bbbbbbbb"]),
		openPRSynced(43, "ct/feat/cccccccc", "ct/feat/bbbbbbbb", remotes2["cccccccc"]),
	})
	_, nothing2, err := planRestack(st2)
	if err != nil {
		t.Fatalf("rerun planRestack: %v", err)
	}
	if !nothing2 {
		t.Errorf("rerun should be a no-op (nothing to restack), got a plan; next_action=%s", st2.Stack.NextAction)
	}
}

// TestRestackIntegrationConflict makes A′ diverge from A on a file B also touches,
// so replaying B onto origin/main conflicts. It asserts the rebase aborts, the
// branch tip is restored exactly, the error reports the conflict, and no remote
// refs were touched.
func TestRestackIntegrationConflict(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	h := buildRestackStack(t, true) // B modifies the same file A created

	before, err := localCommits(h.dir, "main")
	if err != nil {
		t.Fatal(err)
	}
	origA := before[0]

	// Land a DIFFERENT A′ (conflicting content on file `a`).
	squashLand(t, h, "A\n\nct-stack-id: aaaaaaaa", "A-landed-different")
	if err := deleteRemoteBranch(h.dir, "ct/feat/aaaaaaaa"); err != nil {
		t.Fatalf("deleting landed branch: %v", err)
	}
	if _, err := repo.Git(h.dir, "fetch", "origin"); err != nil {
		t.Fatal(err)
	}

	st := restackStatus(h.dir, before, []prRecord{
		mergedPR(41, "ct/feat/aaaaaaaa", "main"),
		openPR(42, "ct/feat/bbbbbbbb", "main"),
		openPR(43, "ct/feat/cccccccc", "ct/feat/bbbbbbbb"),
	})
	plan, _, err := planRestack(st)
	if err != nil {
		t.Fatalf("planRestack: %v", err)
	}

	// Snapshot the remote refs so we can prove the failed restack touched none.
	remoteBefore := bareRefs(t, h.bare)

	preSHA := revParse(t, h.dir, "refs/heads/feat")
	err = rebaseDropLanded(h.dir, "origin/main", plan.LastMergedSHA, "feat", preSHA)
	if err == nil {
		t.Fatal("expected a conflict error from rebaseDropLanded, got nil")
	}
	if !strings.Contains(err.Error(), "conflicting files") || !strings.Contains(err.Error(), "a") {
		t.Errorf("conflict error should name the conflicting file: %v", err)
	}
	if !strings.Contains(err.Error(), "aborted") {
		t.Errorf("error should confirm the rebase was aborted: %v", err)
	}

	// The abort restored the branch tip exactly, and A is still present.
	if got := revParse(t, h.dir, "refs/heads/feat"); got != preSHA {
		t.Errorf("branch tip after abort = %s, want the pre-rebase %s", got, preSHA)
	}
	if _, err := repo.Git(h.dir, "merge-base", "--is-ancestor", origA.SHA, "feat"); err != nil {
		t.Error("original A commit should still be an ancestor of feat after an aborted restack")
	}
	// No rebase left in progress.
	if err := ensureNoRebaseInProgress(h.dir); err != nil {
		t.Errorf("a rebase is still in progress after the abort: %v", err)
	}
	// The working tree is clean.
	if out, err := repo.Git(h.dir, "status", "--porcelain"); err != nil {
		t.Fatal(err)
	} else if strings.TrimSpace(out) != "" {
		t.Errorf("working tree dirty after aborted restack:\n%s", out)
	}
	// No remote refs were touched (no deletes, no pushes).
	if got := bareRefs(t, h.bare); got != remoteBefore {
		t.Errorf("remote refs changed after a failed restack:\n before %q\n after  %q", remoteBefore, got)
	}
}

// restackHarness bundles the paths a restack integration test operates on.
type restackHarness struct {
	dir  string // the feat worktree
	bare string // the file:// bare origin
}

// buildRestackStack scripts a repo with a 3-commit stack A/B/C (each with a
// ct-stack-id trailer and real file content) on a `feat` worktree, a bare origin,
// and every stack branch plus main pushed. When bConflicts is set, B modifies the
// same file A created, so a later diverging land of A makes B's replay conflict.
func buildRestackStack(t *testing.T, bConflicts bool) restackHarness {
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
	writeFile(t, repoDir, "f", "base\n")
	run(repoDir, "add", ".")
	run(repoDir, "commit", "-m", "base")

	r := repo.Repo{Name: "demo", Path: repoDir}
	wt, err := repo.CreateWorktree(r, ".worktrees/feat", "feat", "")
	if err != nil {
		t.Fatal(err)
	}
	dir := wt.Path

	// A creates file `a`.
	writeFile(t, dir, "a", "from A\n")
	run(dir, "add", ".")
	run(dir, "commit", "-m", "A\n\nct-stack-id: aaaaaaaa")
	// B either modifies `a` (conflict setup) or adds its own file `b`.
	if bConflicts {
		writeFile(t, dir, "a", "from B\n")
	} else {
		writeFile(t, dir, "b", "from B\n")
	}
	run(dir, "add", ".")
	run(dir, "commit", "-m", "B\n\nct-stack-id: bbbbbbbb")
	// C adds file `c`.
	writeFile(t, dir, "c", "from C\n")
	run(dir, "add", ".")
	run(dir, "commit", "-m", "C\n\nct-stack-id: cccccccc")

	bare := filepath.Join(root, "origin.git")
	// The clone used by squashLand follows the bare remote's HEAD. Make it
	// deterministic instead of inheriting the runner's init.defaultBranch.
	run(dir, "init", "--bare", "-b", "main", bare)
	run(dir, "remote", "add", "origin", bare)
	// Resolve main from the primary worktree. Newer Git versions do not always
	// expose a branch checked out by another worktree as an unqualified push
	// source when the command runs from a linked worktree.
	run(repoDir, "push", "origin", "main")

	commits, err := localCommits(dir, "main")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range commits {
		run(dir, "push", "origin", c.SHA+":refs/heads/ct/feat/"+c.StackID)
	}
	// Refresh remote-tracking refs so remoteBranches sees the pushed branches.
	run(dir, "fetch", "origin")

	return restackHarness{dir: dir, bare: bare}
}

// squashLand simulates a squash merge of the bottom PR: in a fresh clone of the
// bare origin it commits a single squash-equivalent commit onto main (carrying
// the given message, so the trailer lands too) and pushes main. aContent is the
// content written to file `a` — identical to A's for the clean case, divergent
// for the conflict case.
func squashLand(t *testing.T, h restackHarness, message, aContent string) {
	t.Helper()
	clone := filepath.Join(t.TempDir(), "clone")
	if _, err := repo.Git(filepath.Dir(clone), "clone", h.bare, clone); err != nil {
		t.Fatalf("clone: %v", err)
	}
	run := func(args ...string) {
		t.Helper()
		if _, err := repo.Git(clone, args...); err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
	}
	run("config", "user.email", "lander@t.t")
	run("config", "user.name", "Lander")
	writeFile(t, clone, "a", aContent+"\n")
	run("add", ".")
	run("commit", "-m", message)
	run("push", "origin", "main")
}

// restackStatus reconciles real local commits with synthetic PR records into a
// StackStatus, the same shape gatherStatus produces — letting the integration
// test exercise planRestack without a live GitHub.
func restackStatus(dir string, commits []LocalCommit, prs []prRecord) StackStatus {
	stk, cs := reconcile("feat", "main", commits, remotesFor(dir), prs)
	return StackStatus{
		Schema: 1, Repo: "demo", Worktree: "feat", Branch: "feat", MainBranch: "main",
		Stack: stk, Commits: cs,
	}
}

// remotesFor reads the worktree's remote stack branches, ignoring errors (an
// empty map is a fine fallback for the reconcile the test drives).
func remotesFor(dir string) map[string]string {
	m, _ := remoteBranches(dir, "feat")
	return m
}

func mergedPR(num int, head, base string) prRecord {
	return prRecord{
		Number: num, State: "MERGED", Head: head, Base: base,
		Review: "APPROVED", MergedAt: "2026-07-10T12:00:00Z", Mergeable: "MERGEABLE",
		Checks: Checks{Summary: "passing", Failing: []string{}},
	}
}

func openPR(num int, head, base string) prRecord {
	return prRecord{
		Number: num, State: "OPEN", Head: head, Base: base,
		Review: "APPROVED", Mergeable: "MERGEABLE",
		Checks: Checks{Summary: "passing", Failing: []string{}},
	}
}

// openPRSynced is openPR whose head branch remote tip is known, so the reconciler
// classifies the commit as open (in sync) rather than diverged. The SHA is only
// used by the reconciler for the sync check via the remotes map, so this is just
// openPR — kept as a named helper for the rerun's intent.
func openPRSynced(num int, head, base, _ string) prRecord {
	return openPR(num, head, base)
}

// rawMessage returns a commit's full message (subject + body) verbatim, for
// byte-identity assertions across a rebase.
func rawMessage(t *testing.T, dir, sha string) string {
	t.Helper()
	out, err := repo.Git(dir, "show", "-s", "--format=%B", sha)
	if err != nil {
		t.Fatalf("reading message of %s: %v", sha, err)
	}
	return out
}

// revParse resolves a rev to a trimmed SHA, failing the test on error.
func revParse(t *testing.T, dir, rev string) string {
	t.Helper()
	out, err := repo.Git(dir, "rev-parse", rev)
	if err != nil {
		t.Fatalf("rev-parse %s: %v", rev, err)
	}
	return strings.TrimSpace(out)
}

// bareRefs returns a stable string of all refs in the bare repo, for proving a
// failed restack touched none of them.
func bareRefs(t *testing.T, bare string) string {
	t.Helper()
	out, err := repo.Git(bare, "for-each-ref", "--format=%(refname) %(objectname)")
	if err != nil {
		t.Fatalf("for-each-ref: %v", err)
	}
	return strings.TrimSpace(out)
}

// writeFile writes content to dir/name, failing the test on error.
func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
