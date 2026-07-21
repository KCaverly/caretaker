package stack

import "strings"

// reconcile is the heart of the engine: a pure function from gathered data
// (bottom-first local commits, a stack id -> remote-tip-SHA map, and the stack's
// PRs) to the stack-level rollup and the per-commit views. It runs no
// subprocesses, so every branch of its logic is unit-testable in isolation.
func reconcile(worktree, mainBranch string, commits []LocalCommit, remotes map[string]string, prs []prRecord) (Stack, []Commit) {
	// How many commits carry each clean id — the signal for duplicate-id. A
	// malformed value (not a single 8-hex id) is handled per-commit below and is
	// deliberately not counted here.
	idCounts := map[string]int{}
	for _, c := range commits {
		if validStackID(c.StackID) {
			idCounts[c.StackID]++
		}
	}

	out := make([]Commit, 0, len(commits))
	for i, lc := range commits {
		out = append(out, resolveCommit(i, lc, worktree, idCounts, remotes, prs))
	}

	stk := Stack{
		Size:        len(out),
		BaseChainOK: baseChainOK(out, worktree, mainBranch),
		Counts:      countStates(out),
		Orphans:     findOrphans(out, worktree, prs),
	}
	stk.NextAction = nextAction(out, stk, mainBranch, anyMergedPR(prs))
	return stk, out
}

// anyMergedPR reports whether any of the stack's (already namespace-filtered) PRs
// is merged. It is the archive signal for an empty stack, where no *commit* is
// merged (they've all been restacked away) but a landed PR still records that the
// workflow finished here.
func anyMergedPR(prs []prRecord) bool {
	for _, p := range prs {
		if p.State == "MERGED" {
			return true
		}
	}
	return false
}

// resolveCommit classifies a single local commit into its State and attaches the
// matching PR view. The precedence is: no trailer -> unsubmitted; malformed or
// duplicated trailer -> duplicate-id (escalation, checked before remote/PR state
// because the id can't be trusted); a MERGED PR -> merged (squash merges leave
// the commit local, matched by branch not SHA); a CLOSED-without-open PR ->
// closed; then the ordinary push/sync ladder (unpushed -> diverged -> open ->
// missing-pr).
func resolveCommit(i int, lc LocalCommit, worktree string, idCounts map[string]int, remotes map[string]string, prs []prRecord) Commit {
	c := Commit{
		Position: i + 1,
		SHA:      lc.SHA,
		ShortSHA: lc.ShortSHA,
		Subject:  lc.Subject,
	}

	if lc.StackID == "" {
		c.State = StateUnsubmitted
		return c
	}

	id := lc.StackID
	c.StackID = &id

	// A malformed value (e.g. two trailers concatenated) or an id shared with
	// another commit is a duplicate-id escalation; the branch/PR state can't be
	// trusted, so stop here.
	if !validStackID(id) || idCounts[id] > 1 {
		c.State = StateDuplicateID
		return c
	}

	branch := "ct/" + worktree + "/" + id
	remoteSHA, hasRemote := remotes[id]
	inSync := hasRemote && remoteSHA == lc.SHA
	if hasRemote {
		rb := branch
		c.RemoteBranch = &rb
		c.RemoteInSync = inSync
	}

	merged, open, closed := matchPRs(prs, branch)

	switch {
	case merged != nil:
		c.State = StateMerged
		c.PR = publicPR(merged)
	case closed != nil && open == nil:
		c.State = StateClosed
		c.PR = publicPR(closed)
	case !hasRemote:
		c.State = StateUnpushed
	case !inSync:
		// Remote branch exists but the local commit moved past it. Attach the open
		// PR when there is one so the caller can still show its number.
		c.State = StateDiverged
		if open != nil {
			c.PR = publicPR(open)
		}
	case open != nil:
		c.State = StateOpen
		c.PR = publicPR(open)
	default:
		c.State = StateMissingPR
	}
	return c
}

// matchPRs finds the merged, open, and closed-without-merge PRs whose head is
// the given branch. Each is the first of its kind; a well-formed stack has at
// most one PR per branch, but a branch can accumulate a stale closed PR beside a
// newer one, so all three are surfaced for resolveCommit's precedence to pick.
func matchPRs(prs []prRecord, branch string) (merged, open, closed *prRecord) {
	for i := range prs {
		p := &prs[i]
		if p.Head != branch {
			continue
		}
		switch p.State {
		case "MERGED":
			if merged == nil {
				merged = p
			}
		case "OPEN":
			if open == nil {
				open = p
			}
		case "CLOSED":
			if closed == nil {
				closed = p
			}
		}
	}
	return merged, open, closed
}

// publicPR projects a prRecord into the JSON PR view, turning an empty mergedAt
// into a nil pointer (JSON null) so the contract's "merged_at": string|null is
// honoured.
func publicPR(p *prRecord) *PR {
	pr := &PR{
		Number:    p.Number,
		URL:       p.URL,
		Base:      p.Base,
		Draft:     p.Draft,
		Review:    p.Review,
		Mergeable: p.Mergeable,
		Checks:    p.Checks,
	}
	if p.MergedAt != "" {
		m := p.MergedAt
		pr.MergedAt = &m
	}
	return pr
}

// baseChainOK walks the open PRs of the non-merged commits bottom-up: the lowest
// such PR must target mainBranch, and every open PR above it must target the
// branch of the previous non-merged open-PR commit (ct/<wt>/<id>). Merged commits
// are skipped entirely — their branches are being deleted by the squash-land, so
// the expected base never advances through them. This is what makes a lowest
// non-merged PR that still bases on a landed commit's branch (auto-retarget not
// yet applied) break the chain, routing it to restack rather than merge. A stack
// with no open PRs is vacuously OK.
func baseChainOK(commits []Commit, worktree, mainBranch string) bool {
	expected := mainBranch
	for _, c := range commits {
		if c.State == StateMerged {
			continue
		}
		if c.State != StateOpen || c.PR == nil {
			continue
		}
		if c.PR.Base != expected {
			return false
		}
		expected = "ct/" + worktree + "/" + *c.StackID
	}
	return true
}

// findOrphans lists open PRs under this worktree's ct/<worktree>/ namespace whose
// id matches no local commit. They are reported for the human but never acted on
// — a landed-and-restacked commit's PR, or a PR for a commit that was dropped.
func findOrphans(commits []Commit, worktree string, prs []prRecord) []Orphan {
	known := map[string]bool{}
	for _, c := range commits {
		if c.StackID != nil {
			known[*c.StackID] = true
		}
	}
	prefix := "ct/" + worktree + "/"
	orphans := []Orphan{}
	for _, p := range prs {
		if p.State != "OPEN" || !strings.HasPrefix(p.Head, prefix) {
			continue
		}
		id := p.Head[strings.LastIndex(p.Head, "/")+1:]
		if known[id] {
			continue
		}
		orphans = append(orphans, Orphan{Number: p.Number, URL: p.URL, Head: p.Head, StackID: id})
	}
	return orphans
}

// countStates tallies how many commits are in each state, omitting zero counts
// so the JSON only lists states that actually occur.
func countStates(commits []Commit) map[State]int {
	counts := map[State]int{}
	for _, c := range commits {
		counts[c.State]++
	}
	return counts
}

// nextAction picks the single most urgent hint, first match wins, in the fixed
// cascade-merge-aware priority order:
//
//  1. escalate — closed PR, duplicate-id, orphan, or a non-contiguous merged prefix
//  2. resolve-conflicts — the bottom open PR conflicts with its base
//  3. merge    — the bottom open PR is independently landable (the cascade: this
//     fires even when landed-local commits sit below it)
//  4. restack  — landed-local commits exist and the cascade is blocked
//  5. submit   — an unsubmitted/unpushed/diverged/missing-pr commit or a broken
//     base chain, with no landed-local commit involved
//  6. fix-ci   — bottom open PR failing (no landed-local commits)
//  7. wait     — bottom open PR pending, or passing but review outstanding
//  8. archive  — empty stack with a landed ct/<wt>/ PR
//  9. clean
func nextAction(commits []Commit, stk Stack, mainBranch string, landed bool) string {
	var hasClosed, hasDup, hasMerged, hasSubmit bool
	for _, c := range commits {
		switch c.State {
		case StateClosed:
			hasClosed = true
		case StateDuplicateID:
			hasDup = true
		case StateMerged:
			hasMerged = true
		case StateUnsubmitted, StateUnpushed, StateDiverged, StateMissingPR:
			hasSubmit = true
		}
	}
	contiguous := mergedPrefixContiguous(commits)

	// 1. Escalations, including a merged commit sitting above a non-merged one
	// (an out-of-order land the human must untangle).
	if hasClosed || hasDup || len(stk.Orphans) > 0 || (hasMerged && !contiguous) {
		return "escalate"
	}

	bottom := bottomOpenPR(commits)

	// A conflict is actionable regardless of whether this is an ordinary stack
	// or a cascade with landed commits below it. Keep it ahead of submit/restack
	// so neither workflow recommendation hides the conflict GitHub reported.
	if bottom != nil && bottom.Mergeable == "CONFLICTING" {
		return "resolve-conflicts"
	}

	// 3. Cascade merge: with landed-local commits below, a green/approved/main-
	// based/non-conflicting bottom open PR on a well-formed chain keeps merging
	// rather than restacking — that IS the cascade, so it beats the restack in
	// step 3. (For a stack with no landed commits, merge is reached via the CI
	// ladder in step 5, which still lets an ordinary submit in step 4 win first —
	// leaving no-merged behavior unchanged.)
	if hasMerged && bottom != nil && stk.BaseChainOK && mergeEligible(bottom, mainBranch) {
		return "merge"
	}

	// 4. Landed-local commits exist but the cascade is blocked: the bottom open PR
	// is mis-based or failing; or there is no open PR left above the
	// landed prefix yet unlanded commits remain. Restack repairs it.
	if hasMerged {
		if bottom != nil {
			if !stk.BaseChainOK || bottom.Checks.Summary == "failing" {
				return "restack"
			}
			// Merely pending or awaiting review: CI may go green and the cascade
			// continues, so wait rather than restack.
			return "wait"
		}
		// No open PR above the landed prefix — drop it (and re-submit anything
		// still stranded above the landed commits).
		return "restack"
	}

	// 5. No landed-local commits: an out-of-sync commit or a broken chain is an
	// ordinary submit.
	if hasSubmit || !stk.BaseChainOK {
		return "submit"
	}

	// 6/7. The bottom open PR drives the CI/review-gated actions.
	if bottom != nil {
		switch bottom.Checks.Summary {
		case "failing":
			return "fix-ci"
		case "pending":
			return "wait"
		default: // passing or none
			if bottom.Review == "APPROVED" || bottom.Review == "" {
				return "merge"
			}
			// Passing CI but review still outstanding (REVIEW_REQUIRED,
			// CHANGES_REQUESTED): nothing to do but wait for the reviewer.
			return "wait"
		}
	}

	// 8/9. No commits and no open PRs: archive if something landed here, else clean.
	if len(commits) == 0 && landed {
		return "archive"
	}
	return "clean"
}

// mergeEligible reports whether a bottom open PR is independently landable: based
// on main, not conflicting, checks passing (or none), and either approved or with
// no review requested. The caller additionally gates on a well-formed base chain.
func mergeEligible(pr *PR, mainBranch string) bool {
	if pr == nil {
		return false
	}
	if pr.Base != mainBranch {
		return false
	}
	if pr.Mergeable == "CONFLICTING" {
		return false
	}
	if pr.Checks.Summary != "passing" && pr.Checks.Summary != "none" {
		return false
	}
	return pr.Review == "APPROVED" || pr.Review == ""
}

// mergedPrefixContiguous reports whether every merged commit forms an unbroken
// bottom prefix of the stack: once a non-merged commit is seen, no merged commit
// may follow. A merged commit above a non-merged one is an out-of-order land.
func mergedPrefixContiguous(commits []Commit) bool {
	seenNonMerged := false
	for _, c := range commits {
		if c.State == StateMerged {
			if seenNonMerged {
				return false
			}
			continue
		}
		seenNonMerged = true
	}
	return true
}

// bottomOpenPR returns the PR of the lowest open commit (the bottom of the
// stack), or nil when no commit is open.
func bottomOpenPR(commits []Commit) *PR {
	if c := bottomOpenCommit(commits); c != nil {
		return c.PR
	}
	return nil
}

// bottomOpenCommit returns the lowest open commit (the bottom of the stack that
// still needs landing), or nil when no commit is open.
func bottomOpenCommit(commits []Commit) *Commit {
	for i := range commits {
		if commits[i].State == StateOpen && commits[i].PR != nil {
			return &commits[i]
		}
	}
	return nil
}

// makeMergeHint builds the squash subject/body hint for a merge-eligible bottom
// commit. body is the commit's raw message body (ct-stack-id trailer included),
// which the merging agent passes to `gh pr merge --squash --body` so a multi-
// commit squash keeps this commit's message verbatim. It returns nil when the
// commit has no PR.
func makeMergeHint(c *Commit, body string) *MergeHint {
	if c == nil || c.PR == nil {
		return nil
	}
	return &MergeHint{
		Number:  c.PR.Number,
		Subject: c.Subject,
		Body:    strings.TrimRight(body, "\n"),
	}
}

// validStackID reports whether s is a clean stack id: exactly 8 lowercase hex
// characters. Anything else (empty is handled separately, malformed, or two
// concatenated ids) is not a usable id.
func validStackID(s string) bool {
	if len(s) != 8 {
		return false
	}
	for _, r := range s {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')) {
			return false
		}
	}
	return true
}
