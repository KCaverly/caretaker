package stack

import (
	"fmt"
	"strings"
	"time"
)

const (
	postMergeSettleTimeout = 12 * time.Second
	// postMergeSettleInterval is the first gap between settle polls; it doubles up
	// to postMergeSettleMaxInterval. Each poll is a full status (git reads plus a
	// GitHub round trip), and GitHub's post-merge consistency lag is measured in
	// seconds, so a fixed half-second cadence spent most of its budget on queries
	// that could not yet have a different answer.
	postMergeSettleInterval    = 500 * time.Millisecond
	postMergeSettleMaxInterval = 3 * time.Second
)

// settlePoll sleeps before the next poll of an eventual-consistency loop and
// returns the next (doubled, capped) interval.
func settlePoll(interval time.Duration) time.Duration {
	time.Sleep(interval)
	return min(interval*2, postMergeSettleMaxInterval)
}

type MergeOptions struct{ Params Params }

type MergeResult struct {
	Status   StackStatus
	Executed []string

	// Merged is the PR number this run squash-merged, or 0 when the squash never
	// ran. It is stamped the moment GitHub accepts the merge, before any cleanup,
	// so a caller holding an error can tell "the merge did not happen" apart from
	// "the merge happened and the tidy-up after it did not". The two want opposite
	// things from the user — the first invites a retry, the second must not.
	Merged int
}

// mergeabilityPending reports whether the one thing standing between this stack
// and a merge is GitHub not having finished computing the bottom open PR's
// mergeability. GitHub answers UNKNOWN (or nothing at all) while that calculation
// is in flight, and an UNKNOWN bottom PR reconciles to "wait" even though the
// base, base chain, checks and review are all already clear.
//
// Telling that apart from a real refusal is what keeps a cascade merge — issued
// moments after the previous PR landed, which is exactly when GitHub is busy
// recomputing — from being reported as a stack that is not ready to merge.
func mergeabilityPending(st StackStatus) bool {
	if !st.GitHub.Available || !st.Stack.BaseChainOK {
		return false
	}
	bottom := bottomOpenPR(st.Commits)
	if bottom == nil {
		return false
	}
	if bottom.Mergeable != "" && bottom.Mergeable != "UNKNOWN" {
		return false
	}
	return mergeReadyApartFromMergeability(bottom, st.MainBranch)
}

// awaitMergeReady re-reads status for as long as the merge is blocked only by
// GitHub's pending mergeability calculation, returning as soon as that resolves —
// or when the settle budget runs out, leaving mergeArgs to report whatever the
// real state turned out to be. A stack blocked for any other reason returns
// immediately, so a genuinely unmergeable stack still fails fast.
func awaitMergeReady(p Params, st StackStatus) StackStatus {
	if !mergeabilityPending(st) {
		return st
	}
	deadline := time.Now().Add(postMergeSettleTimeout)
	interval := postMergeSettleInterval
	for time.Now().Before(deadline) {
		interval = settlePoll(interval)
		fresh, err := Status(p)
		if err != nil {
			return st
		}
		st = fresh
		if !mergeabilityPending(st) {
			return st
		}
	}
	return st
}

// mergeArgs validates fresh status and builds the guarded gh command. GitHub's
// affirmative MERGEABLE value is required; UNKNOWN and non-main bases fail.
func mergeArgs(st StackStatus) ([]string, error) {
	if !st.GitHub.Available {
		return nil, fmt.Errorf("GitHub is unavailable")
	}
	if st.Stack.NextAction != "merge" || st.MergeHint == nil {
		return nil, fmt.Errorf("stack is not ready to merge (next action: %s)", st.Stack.NextAction)
	}
	c := bottomOpenCommit(st.Commits)
	if c == nil || c.PR == nil || c.PR.Number != st.MergeHint.Number {
		return nil, fmt.Errorf("merge target is missing from the stack")
	}
	if c.PR.Base != st.MainBranch {
		return nil, fmt.Errorf("PR #%d targets %s, not main branch %s", c.PR.Number, c.PR.Base, st.MainBranch)
	}
	if c.PR.Mergeable != "MERGEABLE" {
		return nil, fmt.Errorf("PR #%d is not mergeable (GitHub: %s)", c.PR.Number, c.PR.Mergeable)
	}
	h := st.MergeHint
	return []string{"pr", "merge", fmt.Sprint(h.Number), "--squash", "--subject", h.Subject, "--body", h.Body}, nil
}

// postMergeSettled reports whether GitHub has fully reflected a merge and, when
// another PR remains, finished retargeting it and calculating mergeability.
// Before that, reconciliation can briefly say restack (deleted old base), then
// wait (UNKNOWN mergeability), even though no user action is required.
func postMergeSettled(st StackStatus, mergedNumber int) bool {
	reflected := false
	for _, c := range st.Commits {
		if c.State == StateMerged && c.PR != nil && c.PR.Number == mergedNumber {
			reflected = true
			break
		}
	}
	if !reflected {
		return false
	}
	bottom := bottomOpenPR(st.Commits)
	if bottom == nil {
		return true // the stack is fully landed; restack is the durable next step
	}
	if !st.Stack.BaseChainOK || bottom.Base != st.MainBranch {
		return false
	}
	return bottom.Mergeable != "" && bottom.Mergeable != "UNKNOWN"
}

// settledStatus polls through GitHub's eventually-consistent post-merge window.
// The caller keeps the TUI in its working state until this returns, preventing a
// transient restack → wait → merge sequence from being presented as real work.
func settledStatus(p Params, mergedNumber int) (StackStatus, error) {
	deadline := time.Now().Add(postMergeSettleTimeout)
	interval := postMergeSettleInterval
	var latest StackStatus
	for {
		st, err := Status(p)
		if err != nil {
			return st, err
		}
		latest = st
		if postMergeSettled(st, mergedNumber) || time.Now().After(deadline) {
			return latest, nil
		}
		interval = settlePoll(interval)
	}
}

// waitForMerged waits only for the merge itself to be visible. Dependent-PR
// retargeting is handled explicitly afterwards, so correctness does not depend
// on the repository's automatic branch-deletion setting.
func waitForMerged(p Params, mergedNumber int) (StackStatus, error) {
	deadline := time.Now().Add(postMergeSettleTimeout)
	interval := postMergeSettleInterval
	for {
		st, err := Status(p)
		if err != nil {
			return st, err
		}
		for _, c := range st.Commits {
			if c.State == StateMerged && c.PR != nil && c.PR.Number == mergedNumber {
				return st, nil
			}
		}
		if time.Now().After(deadline) {
			return st, fmt.Errorf("GitHub did not reflect merge of PR #%d within %s", mergedNumber, postMergeSettleTimeout)
		}
		interval = settlePoll(interval)
	}
}

// retargetOpenDependents moves every open stack PR still based on oldBase to
// newBase and verifies the resulting state. Usually there is one immediate
// child, but handling all matches keeps cleanup safe in a malformed or
// concurrently-edited stack.
func retargetOpenDependents(dir, worktree, mainBranch, oldBase, newBase string) ([]string, error) {
	prs, gh := gatherGitHub(dir, worktree, mainBranch)
	if !gh.Available {
		return nil, fmt.Errorf("cannot verify dependents of %s: %s", oldBase, strings.Join(gh.Warnings, "; "))
	}
	var changed []string
	for _, p := range openPRsBasedOn(prs, oldBase) {
		if err := ghEditBase(dir, p.Number, newBase); err != nil {
			// GitHub may have auto-retargeted between the read and edit. Verify the
			// desired state before treating that race as a failure — a single-PR
			// read, since the question is about this one PR.
			state, base, verr := ghPRBase(dir, p.Number)
			if verr != nil || state != "OPEN" || base != newBase {
				return changed, fmt.Errorf("retargeting dependent PR #%d from %s to %s: %w", p.Number, oldBase, newBase, err)
			}
		}
		changed = append(changed, fmt.Sprintf("retargeted dependent PR #%d -> %s", p.Number, newBase))
	}

	// One re-read of the whole scope, not one per dependent: the closing guarantee
	// is a set property (nothing open still targets oldBase), including a PR that
	// appeared after the snapshot above.
	fresh, freshGH := gatherGitHub(dir, worktree, mainBranch)
	if !freshGH.Available {
		return changed, fmt.Errorf("cannot verify dependents of %s after retargeting: %s", oldBase, strings.Join(freshGH.Warnings, "; "))
	}
	if dependents := openPRsBasedOn(fresh, oldBase); len(dependents) > 0 {
		return changed, fmt.Errorf("open PR #%d still targets %s after retargeting", dependents[0].Number, oldBase)
	}
	return changed, nil
}

func prOpenOnBase(prs []prRecord, number int, base string) bool {
	for _, p := range prs {
		if p.Number == number {
			return p.State == "OPEN" && p.Base == base
		}
	}
	return false
}

// Merge refreshes status, revalidates the target, squash-merges it, then waits
// for GitHub's dependent-PR retargeting to settle before returning status.
// Branch cleanup is deliberately left to GitHub's repository-level setting so
// dependent stacked PRs are retargeted atomically.
func Merge(o MergeOptions) (MergeResult, error) {
	p := o.Params
	p.Fetch = true
	st, err := Status(p)
	res := MergeResult{Status: st}
	if err != nil {
		return res, err
	}

	// Give GitHub's mergeability calculation a chance to land before judging the
	// stack. Polling uses o.Params, not p: the fetch p asks for has already
	// happened, and mergeability is remote state a git fetch cannot advance.
	st = awaitMergeReady(o.Params, st)
	res.Status = st

	args, err := mergeArgs(st)
	if err != nil {
		return res, err
	}
	if _, err := runGH(p.WorktreeDir, args...); err != nil {
		return res, fmt.Errorf("merging PR #%d: %w", st.MergeHint.Number, err)
	}
	// Stamped before any cleanup step can fail: past this line the squash has
	// landed on GitHub, whatever else goes wrong.
	res.Merged = st.MergeHint.Number
	res.Executed = append(res.Executed, fmt.Sprintf("merged PR #%d (squash)", st.MergeHint.Number))

	merged := bottomOpenCommit(st.Commits)
	if merged == nil || merged.RemoteBranch == nil || merged.PR == nil {
		return res, fmt.Errorf("merged PR #%d has no tracked stack branch", st.MergeHint.Number)
	}
	if _, err := waitForMerged(o.Params, st.MergeHint.Number); err != nil {
		return res, err
	}
	steps, err := retargetOpenDependents(p.WorktreeDir, st.Worktree, st.MainBranch, *merged.RemoteBranch, merged.PR.Base)
	res.Executed = append(res.Executed, steps...)
	if err != nil {
		return res, err
	}
	if err := ensureBranchHasNoOpenDependents(p.WorktreeDir, st.Worktree, st.MainBranch, *merged.RemoteBranch); err != nil {
		return res, err
	}
	if err := deleteRemoteBranch(p.WorktreeDir, *merged.RemoteBranch); err != nil {
		return res, fmt.Errorf("deleting merged remote branch %s: %w", *merged.RemoteBranch, err)
	}
	res.Executed = append(res.Executed, "deleted merged remote branch "+*merged.RemoteBranch)

	res.Status, err = settledStatus(o.Params, st.MergeHint.Number)
	return res, err
}
