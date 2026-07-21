package stack

import (
	"fmt"
	"time"
)

const (
	postMergeSettleTimeout  = 12 * time.Second
	postMergeSettleInterval = 500 * time.Millisecond
)

type MergeOptions struct{ Params Params }

type MergeResult struct {
	Status   StackStatus
	Executed []string
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
		time.Sleep(postMergeSettleInterval)
	}
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
	args, err := mergeArgs(st)
	if err != nil {
		return res, err
	}
	if _, err := runGH(p.WorktreeDir, args...); err != nil {
		return res, fmt.Errorf("merging PR #%d: %w", st.MergeHint.Number, err)
	}
	res.Executed = append(res.Executed, fmt.Sprintf("merged PR #%d (squash)", st.MergeHint.Number))
	res.Status, err = settledStatus(o.Params, st.MergeHint.Number)
	return res, err
}
