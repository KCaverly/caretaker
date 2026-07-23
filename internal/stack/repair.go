package stack

import (
	"fmt"
	"strings"

	"github.com/KCaverly/caretaker/internal/repo"
)

type RepairOptions struct {
	Params
	PR     int
	DryRun bool
}

type RepairPlan struct {
	PR            int    `json:"pr"`
	Head          string `json:"head"`
	FormerBase    string `json:"former_base"`
	FormerBaseSHA string `json:"former_base_sha"`
	NewBase       string `json:"new_base"`
}

type RepairResult struct {
	Status   StackStatus `json:"status"`
	Plan     RepairPlan  `json:"plan"`
	DryRun   bool        `json:"dry_run"`
	Executed []string    `json:"executed,omitempty"`
}

func planRepair(st StackStatus, prs []prRecord, number int) (RepairPlan, error) {
	if number <= 0 {
		return RepairPlan{}, fmt.Errorf("a positive PR number is required")
	}
	var target *prRecord
	for i := range prs {
		if prs[i].Number == number {
			target = &prs[i]
			break
		}
	}
	if target == nil {
		return RepairPlan{}, fmt.Errorf("PR #%d is not part of stack %s", number, st.Worktree)
	}
	if target.State != "CLOSED" || target.MergedAt != "" {
		return RepairPlan{}, fmt.Errorf("PR #%d is %s; repair requires a closed, unmerged PR", number, strings.ToLower(target.State))
	}
	prefix := "ct/" + st.Worktree + "/"
	if !strings.HasPrefix(target.Base, prefix) {
		return RepairPlan{}, fmt.Errorf("PR #%d former base %s is not a stack branch", number, target.Base)
	}
	var basePR *prRecord
	for i := range prs {
		if prs[i].Head == target.Base && prs[i].State == "MERGED" && prs[i].HeadSHA != "" {
			basePR = &prs[i]
			break
		}
	}
	if basePR == nil {
		return RepairPlan{}, fmt.Errorf("former base %s has no merged PR with a recoverable head SHA", target.Base)
	}

	newBase := st.MainBranch
	for i, c := range st.Commits {
		if c.PR == nil || c.PR.Number != number {
			continue
		}
		for j := i - 1; j >= 0; j-- {
			if st.Commits[j].State == StateMerged || st.Commits[j].StackID == nil {
				continue
			}
			newBase = "ct/" + st.Worktree + "/" + *st.Commits[j].StackID
			break
		}
		break
	}
	if newBase == target.Base {
		return RepairPlan{}, fmt.Errorf("PR #%d still belongs on %s; refusing temporary-base recovery", number, target.Base)
	}
	return RepairPlan{PR: number, Head: target.Head, FormerBase: target.Base, FormerBaseSHA: basePR.HeadSHA, NewBase: newBase}, nil
}

func Repair(o RepairOptions) (RepairResult, error) {
	res := RepairResult{DryRun: o.DryRun}
	if err := ensureCleanTree(o.WorktreeDir); err != nil {
		return res, err
	}
	if err := requireGH(); err != nil {
		return res, err
	}
	if err := fetchOrigin(o.WorktreeDir); err != nil {
		return res, fmt.Errorf("git fetch origin failed: %w", err)
	}
	p := o.Params
	p.Fetch = false
	st, prs, err := gatherStatus(p)
	if err != nil {
		return res, err
	}
	st.Fetched = true
	res.Status = st
	if !st.GitHub.Available {
		return res, fmt.Errorf("GitHub is unavailable: %s", strings.Join(st.GitHub.Warnings, "; "))
	}
	res.Plan, err = planRepair(st, prs, o.PR)
	if err != nil || o.DryRun {
		return res, err
	}

	if _, err := repo.Git(o.WorktreeDir, pushArgs(res.Plan.FormerBase, res.Plan.FormerBaseSHA, "", true)...); err != nil {
		return res, fmt.Errorf("temporarily restoring %s: %w", res.Plan.FormerBase, err)
	}
	res.Executed = append(res.Executed, fmt.Sprintf("restored temporary base %s at %s", res.Plan.FormerBase, res.Plan.FormerBaseSHA))
	if _, err := runGH(o.WorktreeDir, "pr", "reopen", fmt.Sprint(o.PR)); err != nil {
		return res, fmt.Errorf("reopening PR #%d (temporary base %s was preserved): %w", o.PR, res.Plan.FormerBase, err)
	}
	res.Executed = append(res.Executed, fmt.Sprintf("reopened PR #%d", o.PR))
	if err := ghEditBase(o.WorktreeDir, o.PR, res.Plan.NewBase); err != nil {
		return res, fmt.Errorf("retargeting PR #%d (temporary base %s was preserved): %w", o.PR, res.Plan.FormerBase, err)
	}
	res.Executed = append(res.Executed, fmt.Sprintf("retargeted PR #%d -> %s", o.PR, res.Plan.NewBase))

	fresh, gh := gatherGitHub(o.WorktreeDir, st.Worktree)
	if !gh.Available || !prOpenOnBase(fresh, o.PR, res.Plan.NewBase) {
		return res, fmt.Errorf("PR #%d was not open on %s after recovery; temporary base %s was preserved", o.PR, res.Plan.NewBase, res.Plan.FormerBase)
	}
	if err := ensureBranchHasNoOpenDependents(o.WorktreeDir, st.Worktree, res.Plan.FormerBase); err != nil {
		return res, err
	}
	if err := deleteRemoteBranch(o.WorktreeDir, res.Plan.FormerBase); err != nil {
		return res, fmt.Errorf("deleting temporary base %s: %w", res.Plan.FormerBase, err)
	}
	res.Executed = append(res.Executed, "deleted temporary base "+res.Plan.FormerBase)
	res.Status, err = Status(o.Params)
	return res, err
}
