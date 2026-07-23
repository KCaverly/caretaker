package stack

import (
	"fmt"
	"strings"
)

type ArchiveResult struct {
	Status        StackStatus
	BranchDeletes []string
	Executed      []string
}

func planArchive(st StackStatus) ([]string, error) {
	if st.Stack.NextAction != "complete" {
		return nil, fmt.Errorf("stack is no longer complete (next action: %s)", st.Stack.NextAction)
	}
	plan, nothing, err := planRestack(st)
	if err != nil {
		return nil, err
	}
	if nothing {
		return nil, fmt.Errorf("complete stack has no landed commits to archive")
	}
	var branches []string
	for _, id := range plan.MergedIDs {
		branches = append(branches, "ct/"+st.Worktree+"/"+id)
	}
	return branches, nil
}

// ArchiveCleanup performs the remote half of archiving a complete stack. It
// deliberately does not rewrite local history or require a clean worktree: the
// TUI removes that worktree only after showing its destructive warning.
func ArchiveCleanup(p Params) (ArchiveResult, error) {
	var res ArchiveResult
	if err := requireGH(); err != nil {
		return res, err
	}
	if err := fetchOrigin(p.WorktreeDir); err != nil {
		return res, fmt.Errorf("git fetch origin failed: %w", err)
	}
	p.Fetch = false
	st, _, err := gatherStatus(p)
	if err != nil {
		return res, err
	}
	st.Fetched = true
	res.Status = st
	if !st.GitHub.Available {
		return res, fmt.Errorf("GitHub is unavailable: %s", strings.Join(st.GitHub.Warnings, "; "))
	}
	res.BranchDeletes, err = planArchive(st)
	if err != nil {
		return res, err
	}
	for _, branch := range res.BranchDeletes {
		if err := ensureBranchHasNoOpenDependents(p.WorktreeDir, st.Worktree, branch); err != nil {
			return res, err
		}
		if err := deleteRemoteBranch(p.WorktreeDir, branch); err != nil {
			return res, fmt.Errorf("deleting landed remote branch %s: %w", branch, err)
		}
		res.Executed = append(res.Executed, "deleted landed remote branch "+branch)
	}
	return res, nil
}
