package stack

import "fmt"

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

// Merge refreshes status, revalidates the target, squash-merges it, and refreshes
// status again for callers. Branch cleanup is deliberately left to GitHub's
// repository-level setting so dependent stacked PRs are retargeted atomically.
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
	res.Status, err = Status(o.Params)
	return res, err
}
