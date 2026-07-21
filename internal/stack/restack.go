package stack

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/KCaverly/caretaker/internal/repo"
)

// RestackOptions configures a restack. It embeds the same Params a status/submit
// uses (worktree location, main branch) plus the dry-run flag. Fetch on the
// embedded Params is ignored: restack always fetches, once, up front.
type RestackOptions struct {
	Params
	DryRun bool // plan only; execute nothing mutating (fetch still runs)
}

// RestackResult is what Restack reports back to the CLI. Status is the stack
// status after the pipeline ran (the pre-execution status in a dry-run); Plan is
// the post-rebase submit convergence plan. Nothing marks the no-landed-commits
// case ("nothing to restack"). Drops, RebaseCmd, and BranchDeletes describe the
// landed-prefix removal, and Executed logs the mutating steps that completed.
type RestackResult struct {
	Status        StackStatus
	Plan          Plan
	DryRun        bool
	Nothing       bool
	Drops         []DropAction
	RebaseCmd     []string
	BranchDeletes []string
	Executed      []string
}

// DropAction is a landed commit that the rebase will drop from the local branch:
// it has squash-merged upstream, so its local copy is redundant.
type DropAction struct {
	Position int    `json:"position"`
	ShortSHA string `json:"short_sha"`
	Subject  string `json:"subject"`
	Number   int    `json:"number"` // merged PR number, 0 if unknown
}

// restackPlan is the pure landed-prefix computation: which commits form the
// merged bottom prefix (to drop and whose remote branches to delete) and the
// upstream ref the rebase should exclude.
type restackPlan struct {
	MergedIDs     []string // bottom-first stack ids of the landed prefix
	LastMergedSHA string   // sha of the top landed commit — rebase --onto upstream
	Drops         []DropAction
}

// planRestack validates a status for restacking and computes the landed-prefix
// drop. It escalates (error) on the same states the status engine flags —
// duplicate-id, closed PR, orphan, non-contiguous merged prefix — and reports
// nothing=true when no commit has landed (the "nothing to restack" case). It runs
// no subprocesses, so every guard is unit-testable.
func planRestack(st StackStatus) (restackPlan, bool, error) {
	for _, c := range st.Commits {
		if c.State == StateDuplicateID {
			return restackPlan{}, false, fmt.Errorf("commit %s (%s) has a duplicate or malformed ct-stack-id; make ids unique before restacking", c.ShortSHA, c.Subject)
		}
	}
	for _, c := range st.Commits {
		if c.State == StateClosed {
			return restackPlan{}, false, fmt.Errorf("commit %s has a PR (%s) closed without merging; reopen or drop it before restacking", c.ShortSHA, prRef(c))
		}
	}
	if len(st.Stack.Orphans) > 0 {
		o := st.Stack.Orphans[0]
		return restackPlan{}, false, fmt.Errorf("orphan PR #%d (head %s) matches no local commit; resolve orphan PRs before restacking", o.Number, o.Head)
	}
	if !mergedPrefixContiguous(st.Commits) {
		return restackPlan{}, false, fmt.Errorf("a landed commit sits above an unlanded one (out-of-order land); resolve it by hand before restacking")
	}

	var plan restackPlan
	for _, c := range st.Commits {
		if c.State != StateMerged {
			break // merged commits are a contiguous bottom prefix (checked above)
		}
		plan.LastMergedSHA = c.SHA
		if c.StackID != nil {
			plan.MergedIDs = append(plan.MergedIDs, *c.StackID)
		}
		num := 0
		if c.PR != nil {
			num = c.PR.Number
		}
		plan.Drops = append(plan.Drops, DropAction{
			Position: c.Position, ShortSHA: c.ShortSHA, Subject: c.Subject, Number: num,
		})
	}
	if len(plan.Drops) == 0 {
		return restackPlan{}, true, nil
	}
	return plan, false, nil
}

// rebaseOntoArgs builds the argv that drops the landed prefix: replay the commits
// in upstream..branch onto newBase (origin/<main>), leaving the landed commits
// behind. Pure and separate from the runner so the exact argv is unit-testable.
func rebaseOntoArgs(newBase, upstream, branch string) []string {
	return []string{"rebase", "--onto", newBase, upstream, branch}
}

// pushDeleteArgs builds the argv to delete a landed remote branch. GitHub deletes
// it automatically on a squash merge with --delete-branch, so this is a best-
// effort cleanup whose "remote ref does not exist" is tolerated by the caller.
func pushDeleteArgs(branch string) []string {
	return []string{"push", "origin", "--delete", branch}
}

// branchSHA reads a local branch's tip, trimmed. Used to record the pre-rebase
// position so an aborted rebase can be proven to have restored it exactly.
func branchSHA(dir, branch string) (string, error) {
	out, err := repo.Git(dir, "rev-parse", "refs/heads/"+branch)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// ensureNoRebaseInProgress refuses to restack while a rebase or merge is
// half-finished: those leave the worktree in a state where a fresh rebase would
// compound the mess. It checks the three sentinels git uses, resolving each via
// `git rev-parse --git-path` so it works from a linked worktree (whose real git
// dir is elsewhere).
func ensureNoRebaseInProgress(dir string) error {
	for _, sentinel := range []string{"rebase-merge", "rebase-apply", "MERGE_HEAD"} {
		out, err := repo.Git(dir, "rev-parse", "--git-path", sentinel)
		if err != nil {
			return err
		}
		path := strings.TrimSpace(out)
		if path == "" {
			continue
		}
		if !filepath.IsAbs(path) {
			path = filepath.Join(dir, path)
		}
		if _, err := os.Stat(path); err == nil {
			return fmt.Errorf("a rebase or merge is in progress (%s present); finish or abort it before restacking", sentinel)
		}
	}
	return nil
}

// rebaseDropLanded runs the landed-prefix rebase non-interactively. On any
// failure it gathers the conflicting files, runs `git rebase --abort`, and
// verifies the branch tip is back at preSHA — reporting loudly if the abort did
// not fully restore it. It returns an error describing the conflict and never
// leaves a rebase in progress.
func rebaseDropLanded(dir, newBase, upstream, branch, preSHA string) error {
	_, err := gitEnv(dir, []string{"GIT_EDITOR=true"}, rebaseOntoArgs(newBase, upstream, branch)...)
	if err == nil {
		return nil
	}

	// Capture the conflicting files before unwinding the rebase.
	conflicts := unmergedFiles(dir)

	if _, abortErr := repo.Git(dir, "rebase", "--abort"); abortErr != nil {
		return fmt.Errorf("rebase failed (%v) and the follow-up `git rebase --abort` also failed (%v); the worktree may be mid-rebase — resolve it by hand", err, abortErr)
	}

	after, shaErr := branchSHA(dir, branch)
	if shaErr != nil {
		return fmt.Errorf("rebase failed and was aborted, but the restored branch tip could not be read: %v (original rebase error: %v)", shaErr, err)
	}
	if after != preSHA {
		return fmt.Errorf("DANGER: rebase failed and was aborted, but %s is now %s, not its pre-rebase %s — inspect the worktree before proceeding", branch, after, preSHA)
	}

	msg := "rebase --onto failed and was aborted cleanly (branch restored)"
	if len(conflicts) > 0 {
		msg += "; conflicting files: " + strings.Join(conflicts, ", ")
	}
	return fmt.Errorf("%s: %w", msg, err)
}

// unmergedFiles lists the paths git marked as conflicting during a rebase, for a
// human-readable failure report. Best-effort: an error here just yields no names.
func unmergedFiles(dir string) []string {
	out, err := repo.Git(dir, "diff", "--name-only", "--diff-filter=U")
	if err != nil {
		return nil
	}
	var files []string
	for _, ln := range strings.Split(out, "\n") {
		if ln = strings.TrimSpace(ln); ln != "" {
			files = append(files, ln)
		}
	}
	return files
}

// fastForwardMain advances the local main ref to origin/main after the rebase.
// The stack model keeps local main at the pre-land base so a squash-landed commit
// still shows up in <main>..HEAD as "merged"; once restack has rebased the
// survivors onto origin/main, that landed commit would otherwise phantom back
// into the range and re-trip the merged guard on the reused submit pipeline. The
// update is a compare-and-swap fast-forward: it refuses unless origin/main is a
// linear descendant of the current main, so it can never clobber divergent work
// (and it is a no-op when they already match). update-ref moves only the ref, so
// the primary worktree's own files are left untouched.
func fastForwardMain(dir, mainBranch string) error {
	cur, err := repo.Git(dir, "rev-parse", "refs/heads/"+mainBranch)
	if err != nil {
		return err
	}
	remote, err := repo.Git(dir, "rev-parse", "refs/remotes/origin/"+mainBranch)
	if err != nil {
		return err
	}
	curSHA, remoteSHA := strings.TrimSpace(cur), strings.TrimSpace(remote)
	if curSHA == remoteSHA {
		return nil
	}
	if _, err := repo.Git(dir, "merge-base", "--is-ancestor", curSHA, remoteSHA); err != nil {
		return fmt.Errorf("local %s (%s) is not an ancestor of origin/%s (%s); refusing to fast-forward — reconcile main by hand before restacking", mainBranch, curSHA, mainBranch, remoteSHA)
	}
	if _, err := repo.Git(dir, "update-ref", "refs/heads/"+mainBranch, remoteSHA, curSHA); err != nil {
		return fmt.Errorf("fast-forwarding %s to origin/%s: %w", mainBranch, mainBranch, err)
	}
	return nil
}

// deleteRemoteBranch deletes a landed remote branch, tolerating the case where
// GitHub's --delete-branch already removed it (a squash merge does this). Any
// other push error is fatal.
func deleteRemoteBranch(dir, branch string) error {
	_, err := repo.Git(dir, pushDeleteArgs(branch)...)
	if err == nil {
		return nil
	}
	if strings.Contains(err.Error(), "remote ref does not exist") {
		return nil
	}
	return err
}

// predictRestackSubmit computes the submit convergence a restack would run after
// dropping the landed prefix, for the dry-run plan. Every surviving commit is
// rebased, so its SHA moves and its branch needs a (force) push; the new bottom
// open PR is retargeted to main if GitHub has not auto-retargeted it; and each
// surviving PR's nav table is re-spliced. Positions are the post-rebase ones.
func predictRestackSubmit(st StackStatus) Plan {
	var survivors []Commit
	for _, c := range st.Commits {
		if c.State != StateMerged {
			survivors = append(survivors, c)
		}
	}
	branchOf := func(c Commit) string {
		if c.StackID != nil {
			return "ct/" + st.Worktree + "/" + *c.StackID
		}
		return "ct/" + st.Worktree + "/(new)"
	}

	var plan Plan
	for i, c := range survivors {
		pos := i + 1
		branch := branchOf(c)
		if c.StackID == nil {
			plan.Assigns = append(plan.Assigns, AssignAction{Position: pos, ShortSHA: c.ShortSHA, Subject: c.Subject})
		}
		// The rebase moves every surviving commit's SHA, so each branch is pushed:
		// a force-update when it exists, a create otherwise.
		plan.Pushes = append(plan.Pushes, PushAction{Position: pos, Branch: branch, Create: c.RemoteBranch == nil})

		wantBase := st.MainBranch
		if i > 0 {
			wantBase = branchOf(survivors[i-1])
		}
		if c.PR == nil {
			plan.Creates = append(plan.Creates, CreateAction{Position: pos, Head: branch, Base: wantBase, Title: c.Subject})
			continue
		}
		if c.PR.Base != wantBase {
			plan.Retargets = append(plan.Retargets, RetargetAction{Position: pos, Number: c.PR.Number, OldBase: c.PR.Base, NewBase: wantBase})
		}
		plan.Bodies = append(plan.Bodies, BodyAction{Position: pos, Number: c.PR.Number})
	}
	return plan
}

// Restack drops the landed bottom prefix of a stack and re-converges the survivors
// onto main. It is the repair action for a blocked cascade: guards, fetch, then a
// `rebase --onto` that removes every squash-merged commit, deletion of the landed
// remote branches, and the existing submit pipeline (force pushes, base retargets,
// nav re-splice). In dry-run it stops after planning and mutates nothing (the
// fetch aside). On any mid-pipeline failure it returns the error with a partial
// Executed log; the rebase itself is atomic — it aborts and restores on conflict.
func Restack(o RestackOptions) (RestackResult, error) {
	res := RestackResult{DryRun: o.DryRun}
	dir := o.WorktreeDir

	// 2. Guards. A history rewrite needs a clean tree and no half-finished
	// rebase/merge, and restack cannot delete branches or drive PRs without gh.
	if err := ensureCleanTree(dir); err != nil {
		return res, err
	}
	if err := ensureNoRebaseInProgress(dir); err != nil {
		return res, err
	}
	if err := requireGH(); err != nil {
		return res, err
	}

	// 3. Fetch: a stale remote view would make the landed-prefix detection and the
	// rebase target unsafe, so a failed fetch is fatal.
	if err := fetchOrigin(dir); err != nil {
		return res, fmt.Errorf("git fetch origin failed: %w", err)
	}

	// 4. Compute status (fetch already done) and the landed-prefix plan.
	p := o.Params
	p.Fetch = false
	st, _, err := gatherStatus(p)
	if err != nil {
		return res, err
	}
	st.Fetched = true
	res.Status = st

	if !st.GitHub.Available {
		return res, fmt.Errorf("GitHub is unavailable, restack needs it: %s", strings.Join(st.GitHub.Warnings, "; "))
	}

	plan, nothing, err := planRestack(st)
	if err != nil {
		return res, err
	}
	if nothing {
		res.Nothing = true
		return res, nil
	}

	res.Drops = plan.Drops
	res.RebaseCmd = append([]string{"git"}, rebaseOntoArgs("origin/"+st.MainBranch, plan.LastMergedSHA, st.Branch)...)
	for _, id := range plan.MergedIDs {
		res.BranchDeletes = append(res.BranchDeletes, "ct/"+st.Worktree+"/"+id)
	}

	if o.DryRun {
		res.Plan = predictRestackSubmit(st)
		return res, nil
	}

	// 5. Rebase away the landed prefix, atomically (abort + restore on conflict).
	preSHA, err := branchSHA(dir, st.Branch)
	if err != nil {
		return res, err
	}
	if err := rebaseDropLanded(dir, "origin/"+st.MainBranch, plan.LastMergedSHA, st.Branch, preSHA); err != nil {
		return res, err
	}
	res.Executed = append(res.Executed, fmt.Sprintf("rebased %s onto origin/%s, dropping %d landed commit(s)", st.Branch, st.MainBranch, len(plan.Drops)))

	// 5b. Advance local main to the landed remote main so the dropped commits leave
	// <main>..HEAD; otherwise the just-landed squash commit phantoms back as merged
	// and the reused submit pipeline would refuse ("restack first").
	if err := fastForwardMain(dir, st.MainBranch); err != nil {
		return res, err
	}
	res.Executed = append(res.Executed, fmt.Sprintf("fast-forwarded %s to origin/%s", st.MainBranch, st.MainBranch))

	// 6. Delete the landed remote branches (tolerating GitHub's auto-delete).
	for _, b := range res.BranchDeletes {
		if err := deleteRemoteBranch(dir, b); err != nil {
			return res, fmt.Errorf("deleting landed remote branch %s: %w", b, err)
		}
		res.Executed = append(res.Executed, "deleted landed remote branch "+b)
	}

	// 7. Re-converge the survivors via the existing submit pipeline: force-with-
	// lease pushes of the rebased branches, retarget of the new bottom PR to main,
	// title updates, and nav-table re-splice.
	sub, err := Submit(SubmitOptions{Params: o.Params, DryRun: false})
	res.Executed = append(res.Executed, sub.Executed...)
	if err != nil {
		return res, fmt.Errorf("re-converging after rebase: %w", err)
	}

	// 8. Report the converged status.
	res.Status = sub.Status
	res.Plan = sub.Plan
	return res, nil
}
