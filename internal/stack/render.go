package stack

import (
	"fmt"
	"strings"
)

// Render returns a compact, human-readable view of a StackStatus: one aligned
// row per commit followed by a summary line carrying the next action. It is the
// non-JSON output of `ct stack status`, meant to be skimmed in a terminal.
func Render(st StackStatus) string {
	var b strings.Builder

	fmt.Fprintf(&b, "%s/%s  (%s..%s)\n", st.Repo, st.Worktree, st.MainBranch, st.Branch)
	if !st.GitHub.Available {
		b.WriteString("  github: unavailable — PR status omitted\n")
		for _, w := range st.GitHub.Warnings {
			fmt.Fprintf(&b, "    ! %s\n", w)
		}
	}

	if len(st.Commits) == 0 {
		b.WriteString("  (no commits ahead of main)\n")
	}
	for _, c := range st.Commits {
		fmt.Fprintf(&b, "  %d %s %-7s %-12s %-6s %-8s %s\n",
			c.Position,
			glyph(c.State),
			c.ShortSHA,
			c.State,
			prLabel(c.PR),
			checksLabel(c.PR),
			c.Subject,
		)
	}

	fmt.Fprintf(&b, "  stack: %d commit(s), base_chain=%s, next=%s\n",
		st.Stack.Size, okLabel(st.Stack.BaseChainOK), st.Stack.NextAction)
	for _, o := range st.Stack.Orphans {
		fmt.Fprintf(&b, "  orphan PR #%d (%s) head=%s\n", o.Number, o.URL, o.Head)
	}
	if h := st.MergeHint; h != nil {
		fmt.Fprintf(&b, "  merge: gh pr merge %d --squash --subject %q --body %q\n",
			h.Number, h.Subject, h.Body)
	}
	return b.String()
}

// RenderPlan returns a human-readable dry-run plan: one line per action, grouped
// by kind, bottom-first within each group. An empty plan reports that the remote
// already matches the local stack.
func RenderPlan(st StackStatus, plan Plan) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s/%s  submit plan (dry-run)\n", st.Repo, st.Worktree)

	if plan.IsEmpty() {
		b.WriteString("  everything is in sync — nothing to submit\n")
		return b.String()
	}

	for _, a := range plan.Assigns {
		fmt.Fprintf(&b, "  would assign id   commit %d %s %q\n", a.Position, a.ShortSHA, a.Subject)
	}
	for _, r := range plan.Retargets {
		if !r.AfterPush {
			fmt.Fprintf(&b, "  would retarget PR #%d  %s -> %s (before pushes)\n", r.Number, r.OldBase, r.NewBase)
		}
	}
	for _, p := range plan.Pushes {
		verb := "force-update"
		if p.Create {
			verb = "create"
		}
		fmt.Fprintf(&b, "  would push %-12s %s (position %d)\n", verb, p.Branch, p.Position)
	}
	for _, c := range plan.Creates {
		fmt.Fprintf(&b, "  would create PR   head %s base %s title %q\n", c.Head, c.Base, c.Title)
	}
	for _, r := range plan.Retargets {
		if r.AfterPush {
			fmt.Fprintf(&b, "  would retarget PR #%d  %s -> %s (after pushes)\n", r.Number, r.OldBase, r.NewBase)
		}
	}
	for _, r := range plan.Retitles {
		fmt.Fprintf(&b, "  would retitle PR  #%d  %q -> %q\n", r.Number, r.OldTitle, r.NewTitle)
	}
	for _, r := range plan.Bodies {
		fmt.Fprintf(&b, "  would update body PR #%d  (nav table)\n", r.Number)
	}
	return b.String()
}

// RenderRestackPlan returns a human-readable dry-run plan for a restack: the
// landed commits that would be dropped, the exact rebase command, the remote
// branches that would be deleted, and the post-rebase submit actions (whose SHAs
// are post-rebase, so pushes are flagged "rebased").
func RenderRestackPlan(res RestackResult) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s/%s  restack plan (dry-run)\n", res.Status.Repo, res.Status.Worktree)

	for _, d := range res.Drops {
		ref := "no PR"
		if d.Number > 0 {
			ref = fmt.Sprintf("#%d", d.Number)
		}
		fmt.Fprintf(&b, "  would drop landed  %s %s (position %d) %q\n", d.ShortSHA, ref, d.Position, d.Subject)
	}
	fmt.Fprintf(&b, "  would rebase       %s\n", strings.Join(res.RebaseCmd, " "))
	for _, br := range res.BranchDeletes {
		fmt.Fprintf(&b, "  would delete branch origin/%s\n", br)
	}

	plan := res.Plan
	if plan.IsEmpty() {
		b.WriteString("  survivors already converge — no post-rebase submit actions\n")
		return b.String()
	}
	b.WriteString("  post-rebase submit:\n")
	for _, a := range plan.Assigns {
		fmt.Fprintf(&b, "    would assign id   commit %d %s %q\n", a.Position, a.ShortSHA, a.Subject)
	}
	for _, pa := range plan.Pushes {
		verb := "force-update"
		if pa.Create {
			verb = "create"
		}
		fmt.Fprintf(&b, "    would push %-12s %s (rebased, position %d)\n", verb, pa.Branch, pa.Position)
	}
	for _, c := range plan.Creates {
		fmt.Fprintf(&b, "    would create PR   head %s base %s title %q\n", c.Head, c.Base, c.Title)
	}
	for _, r := range plan.Retargets {
		fmt.Fprintf(&b, "    would retarget PR #%d  %s -> %s\n", r.Number, r.OldBase, r.NewBase)
	}
	for _, r := range plan.Bodies {
		fmt.Fprintf(&b, "    would update body PR #%d  (nav table)\n", r.Number)
	}
	return b.String()
}

// RenderFinishPlan uses the restack plan's details but names the user-facing
// operation accurately for a stack with no surviving commits.
func RenderFinishPlan(res FinishResult) string {
	return strings.Replace(RenderRestackPlan(res), "restack plan (dry-run)", "finish plan (dry-run)", 1)
}

// glyph is the single-character status mark for a commit state: a check for
// good/landed, a cross for problems, a dotted circle for in-flight/todo, an
// ellipsis for "moved, needs a push".
func glyph(s State) string {
	switch s {
	case StateOpen, StateMerged:
		return "✓"
	case StateClosed, StateDuplicateID:
		return "✗"
	case StateDiverged:
		return "…"
	default: // unsubmitted, unpushed, missing-pr
		return "◌"
	}
}

// prLabel renders a PR reference ("#123") or a dash when there is no PR.
func prLabel(pr *PR) string {
	if pr == nil {
		return "-"
	}
	return fmt.Sprintf("#%d", pr.Number)
}

// checksLabel renders a PR's check summary, or a dash when there is no PR.
func checksLabel(pr *PR) string {
	if pr == nil {
		return "-"
	}
	return pr.Checks.Summary
}

// okLabel renders a boolean as "ok"/"broken" for the base-chain summary.
func okLabel(ok bool) string {
	if ok {
		return "ok"
	}
	return "broken"
}
