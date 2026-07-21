package stack

import (
	"fmt"
	"strings"

	"github.com/KCaverly/caretaker/internal/repo"
)

// SubmitOptions configures a submit. It embeds the same Params a status uses
// (worktree location, main branch) plus the two submit-only flags. Fetch on the
// embedded Params is ignored: submit always fetches, once, up front.
type SubmitOptions struct {
	Params
	DryRun bool // plan only; execute nothing mutating
	Draft  bool // create PRs as drafts
}

// SubmitResult is what Submit reports back to the CLI. Status is the stack status
// after the pipeline ran (or the pre-execution status in a dry-run); Plan is the
// set of actions computed from that status. Nothing marks the empty-stack case;
// Executed is a human-readable log of the mutating steps that completed, used to
// tell the user what got done when a later step fails.
type SubmitResult struct {
	Status   StackStatus
	Plan     Plan
	DryRun   bool
	Nothing  bool
	Executed []string
}

// Plan is the set of typed actions a submit would perform, computed purely from a
// StackStatus (and the PR records that carry titles/bodies). It is the single
// source of truth the executor walks and the dry-run prints. An empty Plan means
// the remote already matches the local stack.
type Plan struct {
	Assigns   []AssignAction   `json:"assigns,omitempty"`
	Pushes    []PushAction     `json:"pushes,omitempty"`
	Creates   []CreateAction   `json:"creates,omitempty"`
	Retargets []RetargetAction `json:"retargets,omitempty"`
	Retitles  []RetitleAction  `json:"retitles,omitempty"`
	Bodies    []BodyAction     `json:"bodies,omitempty"`
}

// AssignAction is a previously-unsubmitted commit that will get a fresh
// ct-stack-id trailer (the actual id is minted during execution, so a dry-run
// only reports the commit).
type AssignAction struct {
	Position int    `json:"position"`
	ShortSHA string `json:"short_sha"`
	Subject  string `json:"subject"`
}

// PushAction is a branch that will be pushed. Create distinguishes a brand-new
// remote branch from a force-update of an existing one.
type PushAction struct {
	Position int    `json:"position"`
	Branch   string `json:"branch"`
	Create   bool   `json:"create"`
}

// CreateAction is a PR that will be opened for a commit with none.
type CreateAction struct {
	Position int    `json:"position"`
	Head     string `json:"head"`
	Base     string `json:"base"`
	Title    string `json:"title"`
}

// RetargetAction is an existing PR whose base branch will change to keep the
// chain well-formed.
type RetargetAction struct {
	Position int    `json:"position"`
	Number   int    `json:"number"`
	OldBase  string `json:"old_base"`
	NewBase  string `json:"new_base"`
}

// RetitleAction is an existing PR whose title will follow a changed commit
// subject.
type RetitleAction struct {
	Position int    `json:"position"`
	Number   int    `json:"number"`
	OldTitle string `json:"old_title"`
	NewTitle string `json:"new_title"`
}

// BodyAction is an existing PR whose body's nav-table region will be re-spliced.
type BodyAction struct {
	Position int `json:"position"`
	Number   int `json:"number"`
}

// IsEmpty reports whether the plan has no actions — the fully-converged case.
func (p Plan) IsEmpty() bool {
	return len(p.Assigns) == 0 && len(p.Pushes) == 0 && len(p.Creates) == 0 &&
		len(p.Retargets) == 0 && len(p.Retitles) == 0 && len(p.Bodies) == 0
}

// planSubmit is the pure planner: it turns a StackStatus (plus the PR records
// that carry titles and bodies) into a Plan, or returns a guard error for a state
// submit refuses to touch. It runs no subprocesses, so every guard and every
// action type is unit-testable in isolation. Called twice by the executor —
// before the rewrite (to show the dry-run plan / detect assigns) and after (when
// ids and new SHAs exist), keying its decisions off the current commit states
// each time.
func planSubmit(st StackStatus, prs []prRecord) (Plan, error) {
	// Guards, in the same escalation order the status engine uses. Each is a
	// specific, actionable refusal — submit converges healthy stacks, it does not
	// paper over escalations.
	for _, c := range st.Commits {
		if c.State == StateDuplicateID {
			return Plan{}, fmt.Errorf("commit %s (%s) has a duplicate or malformed ct-stack-id; make ids unique before submitting", c.ShortSHA, c.Subject)
		}
	}
	for _, c := range st.Commits {
		if c.State == StateClosed {
			return Plan{}, fmt.Errorf("commit %s has a PR (%s) closed without merging; reopen or drop it before submitting", c.ShortSHA, prRef(c))
		}
	}
	for _, c := range st.Commits {
		if c.State == StateMerged {
			return Plan{}, fmt.Errorf("restack first — a landed PR's commit is still local (commit %s, %s)", c.ShortSHA, prRef(c))
		}
	}
	if len(st.Stack.Orphans) > 0 {
		o := st.Stack.Orphans[0]
		return Plan{}, fmt.Errorf("orphan PR #%d (head %s) matches no local commit; resolve orphan PRs before submitting", o.Number, o.Head)
	}

	titleByNum := map[int]string{}
	bodyByNum := map[int]string{}
	for _, p := range prs {
		titleByNum[p.Number] = p.Title
		bodyByNum[p.Number] = p.Body
	}

	branchOf := func(c Commit) string {
		if c.StackID != nil {
			return "ct/" + st.Worktree + "/" + *c.StackID
		}
		// A not-yet-assigned commit: its real id is minted during execution. The
		// placeholder only ever shows up in a dry-run plan.
		return "ct/" + st.Worktree + "/(new)"
	}

	// Any commit at or above the oldest unsubmitted commit will be rebuilt by the
	// trailer rewrite, so its SHA changes and a previously-open branch becomes
	// diverged — a force-push even though nothing about its content changed.
	firstRewrite := -1
	for i, c := range st.Commits {
		if c.State == StateUnsubmitted {
			firstRewrite = i
			break
		}
	}
	rewritten := func(i int) bool { return firstRewrite >= 0 && i >= firstRewrite }

	// Predicted PR numbers, position-indexed, for the nav table (0 = would be
	// created, rendered "#?").
	numbers := make([]int, len(st.Commits))
	for i, c := range st.Commits {
		if c.PR != nil {
			numbers[i] = c.PR.Number
		}
	}

	var plan Plan
	for i, c := range st.Commits {
		branch := branchOf(c)

		if c.State == StateUnsubmitted {
			plan.Assigns = append(plan.Assigns, AssignAction{
				Position: c.Position, ShortSHA: c.ShortSHA, Subject: c.Subject,
			})
		}

		// Push when the branch is out of sync, or when the rewrite will move this
		// commit's SHA out from under its (currently in-sync) remote branch.
		needsPush := false
		switch c.State {
		case StateUnsubmitted, StateUnpushed, StateDiverged:
			needsPush = true
		default:
			needsPush = rewritten(i)
		}
		if needsPush {
			plan.Pushes = append(plan.Pushes, PushAction{
				Position: c.Position, Branch: branch, Create: c.RemoteBranch == nil,
			})
		}

		// The base this PR should target: main for the bottom commit, else the
		// previous commit's branch.
		wantBase := st.MainBranch
		if i > 0 {
			wantBase = branchOf(st.Commits[i-1])
		}

		if c.PR == nil {
			plan.Creates = append(plan.Creates, CreateAction{
				Position: c.Position, Head: branch, Base: wantBase, Title: c.Subject,
			})
			continue
		}

		if c.PR.Base != wantBase {
			plan.Retargets = append(plan.Retargets, RetargetAction{
				Position: c.Position, Number: c.PR.Number, OldBase: c.PR.Base, NewBase: wantBase,
			})
		}
		if old := titleByNum[c.PR.Number]; old != c.Subject {
			plan.Retitles = append(plan.Retitles, RetitleAction{
				Position: c.Position, Number: c.PR.Number, OldTitle: old, NewTitle: c.Subject,
			})
		}
		region := renderNavRegion(numbers, i)
		if cur := bodyByNum[c.PR.Number]; spliceNav(cur, region) != cur {
			plan.Bodies = append(plan.Bodies, BodyAction{Position: c.Position, Number: c.PR.Number})
		}
	}
	return plan, nil
}

// prRef renders a commit's PR as "#N", or a fallback when it has none.
func prRef(c Commit) string {
	if c.PR != nil {
		return fmt.Sprintf("#%d", c.PR.Number)
	}
	return "its PR"
}

// Submit runs the full submit pipeline. In dry-run it stops after planning; in a
// live run it injects trailers, pushes branches, opens/retargets/retitles PRs,
// and splices the nav table, converging the remote to the local stack. It is
// idempotent: a partial run followed by a rerun finishes the job. On any mid-
// pipeline failure it returns the error together with a SubmitResult whose
// Executed log records what already completed.
func Submit(o SubmitOptions) (SubmitResult, error) {
	res := SubmitResult{DryRun: o.DryRun}

	// 2. Clean-tree guard. Staged or unstaged changes to tracked files make a
	// history rewrite unsafe; untracked files are fine.
	if err := ensureCleanTree(o.WorktreeDir); err != nil {
		return res, err
	}

	// 4a. gh must be usable — submit hard-fails where status soft-fails.
	if err := requireGH(); err != nil {
		return res, err
	}

	// 3. Submit always fetches; a failed fetch means the remote state we would
	// force-with-lease against is untrustworthy, so this is a hard error.
	if err := fetchOrigin(o.WorktreeDir); err != nil {
		return res, fmt.Errorf("git fetch origin failed: %w", err)
	}

	// 4b. Compute status (fetch already done).
	p := o.Params
	p.Fetch = false
	st, prs, err := gatherStatus(p)
	if err != nil {
		return res, err
	}
	st.Fetched = true
	res.Status = st

	if !st.GitHub.Available {
		return res, fmt.Errorf("GitHub is unavailable, submit needs it: %s", strings.Join(st.GitHub.Warnings, "; "))
	}

	// 5. Empty stack is a success, not an error.
	if len(st.Commits) == 0 {
		res.Nothing = true
		return res, nil
	}

	// 5/6-9 planning. Guards live here, so they also gate a dry-run.
	plan, err := planSubmit(st, prs)
	if err != nil {
		return res, err
	}
	res.Plan = plan

	if o.DryRun {
		return res, nil
	}

	return execute(o, p, st, res)
}

// execute performs the mutating half of the pipeline against an already-validated
// stack. It re-gathers state after the rewrite so pushes and PR actions see the
// new SHAs, and again after creating PRs so the nav table can reference every
// PR's number.
func execute(o SubmitOptions, p Params, st StackStatus, res SubmitResult) (SubmitResult, error) {
	dir := o.WorktreeDir

	// 6. Trailer injection (rewrites history without touching the work tree).
	lc, err := localCommits(dir, st.MainBranch)
	if err != nil {
		return res, err
	}
	assigns, err := injectTrailers(dir, st.Branch, lc)
	if err != nil {
		return res, fmt.Errorf("trailer injection: %w", err)
	}
	if len(assigns) > 0 {
		res.Executed = append(res.Executed, fmt.Sprintf("assigned %d ct-stack-id(s)", len(assigns)))
	}

	// Re-gather now that SHAs may have moved.
	st2, prs2, err := gatherStatus(p)
	if err != nil {
		return res, err
	}

	// 7. Push every out-of-sync branch. Computed from fresh git data (not the
	// plan) because force-with-lease needs the exact remote tip.
	commits2, err := localCommits(dir, st2.MainBranch)
	if err != nil {
		return res, err
	}
	remotes2, err := remoteBranches(dir, st2.Worktree)
	if err != nil {
		return res, err
	}
	for _, pc := range computePushes(commits2, remotes2, st2.Worktree) {
		if _, err := repo.Git(dir, pushArgs(pc.Branch, pc.SHA, pc.Expected, pc.Create)...); err != nil {
			return res, fmt.Errorf("pushing %s: %w", pc.Branch, err)
		}
		verb := "force-updated"
		if pc.Create {
			verb = "created"
		}
		res.Executed = append(res.Executed, fmt.Sprintf("%s branch %s", verb, pc.Branch))
	}

	// 8. Create missing PRs, retarget wrong bases, retitle changed subjects.
	plan2, err := planSubmit(st2, prs2)
	if err != nil {
		return res, err
	}
	for _, cr := range plan2.Creates {
		body, err := prCreateBody(dir, st2, cr.Position)
		if err != nil {
			return res, err
		}
		if err := ghCreatePR(dir, cr.Head, cr.Base, cr.Title, body, o.Draft); err != nil {
			return res, fmt.Errorf("creating PR for %s: %w", cr.Head, err)
		}
		res.Executed = append(res.Executed, fmt.Sprintf("created PR head %s base %s", cr.Head, cr.Base))
	}
	for _, rt := range plan2.Retargets {
		if err := ghEditBase(dir, rt.Number, rt.NewBase); err != nil {
			return res, fmt.Errorf("retargeting PR #%d: %w", rt.Number, err)
		}
		res.Executed = append(res.Executed, fmt.Sprintf("retargeted PR #%d -> %s", rt.Number, rt.NewBase))
	}
	for _, rt := range plan2.Retitles {
		if err := ghEditTitle(dir, rt.Number, rt.NewTitle); err != nil {
			return res, fmt.Errorf("retitling PR #%d: %w", rt.Number, err)
		}
		res.Executed = append(res.Executed, fmt.Sprintf("retitled PR #%d", rt.Number))
	}

	// 9. Nav table: re-gather so every PR (including just-created ones) has a
	// number, then splice the region into each body and edit only what changed.
	st3, prs3, err := gatherStatus(p)
	if err != nil {
		return res, err
	}
	bodyByNum := map[int]string{}
	for _, r := range prs3 {
		bodyByNum[r.Number] = r.Body
	}
	numbers := make([]int, len(st3.Commits))
	for i, c := range st3.Commits {
		if c.PR != nil {
			numbers[i] = c.PR.Number
		}
	}
	for i, c := range st3.Commits {
		if c.PR == nil {
			continue
		}
		region := renderNavRegion(numbers, i)
		cur := bodyByNum[c.PR.Number]
		if want := spliceNav(cur, region); want != cur {
			if err := ghEditBody(dir, c.PR.Number, want); err != nil {
				return res, fmt.Errorf("updating body of PR #%d: %w", c.PR.Number, err)
			}
			res.Executed = append(res.Executed, fmt.Sprintf("updated nav table of PR #%d", c.PR.Number))
		}
	}

	// 10. Report the converged status.
	stFinal, _, err := gatherStatus(p)
	if err != nil {
		return res, err
	}
	stFinal.Fetched = true
	res.Status = stFinal
	return res, nil
}

// pushCmd is a single branch push the executor will run: force-with-lease when
// Create is false, a create otherwise.
type pushCmd struct {
	Branch   string
	SHA      string
	Expected string // remote tip to lease against; empty for a create
	Create   bool
}

// computePushes derives the branch pushes needed to bring the remote in line with
// the local stack: a create when no remote branch exists for the id, a
// force-update when the remote tip differs, and nothing when it already matches.
func computePushes(commits []LocalCommit, remotes map[string]string, worktree string) []pushCmd {
	var cmds []pushCmd
	for _, c := range commits {
		if !validStackID(c.StackID) {
			continue // unsubmitted commits are handled by the rewrite, before this
		}
		branch := "ct/" + worktree + "/" + c.StackID
		remoteSHA, has := remotes[c.StackID]
		switch {
		case !has:
			cmds = append(cmds, pushCmd{Branch: branch, SHA: c.SHA, Create: true})
		case remoteSHA != c.SHA:
			cmds = append(cmds, pushCmd{Branch: branch, SHA: c.SHA, Expected: remoteSHA})
		}
	}
	return cmds
}

// pushArgs builds the argv for a single branch push. The force-with-lease guards
// the remote ref: an empty expected value asserts the branch does not yet exist
// (create), otherwise it must still point at the tip we last fetched.
func pushArgs(branch, sha, expected string, create bool) []string {
	lease := "--force-with-lease=refs/heads/" + branch + ":" + expected
	return []string{"push", lease, "origin", sha + ":refs/heads/" + branch}
}

// prCreateBody builds the initial body for a new PR: the commit's message body
// (minus the ct-stack-id trailer) plus a nav region. The region's numbers are
// filled in by the later nav pass once every PR exists, so any "#?" here is
// transient.
func prCreateBody(dir string, st StackStatus, position int) (string, error) {
	var sha string
	for _, c := range st.Commits {
		if c.Position == position {
			sha = c.SHA
			break
		}
	}
	body, err := commitBody(dir, sha)
	if err != nil {
		return "", err
	}
	numbers := make([]int, len(st.Commits))
	for i, c := range st.Commits {
		if c.PR != nil {
			numbers[i] = c.PR.Number
		}
	}
	region := renderNavRegion(numbers, position-1)
	return spliceNav(body, region), nil
}

// commitBody returns a commit's message body (everything after the subject),
// with any ct-stack-id trailer line removed so it never leaks into the PR body.
func commitBody(dir, sha string) (string, error) {
	out, err := repo.Git(dir, "show", "-s", "--format=%b", sha)
	if err != nil {
		return "", err
	}
	var kept []string
	for _, ln := range strings.Split(out, "\n") {
		if strings.HasPrefix(ln, "ct-stack-id:") {
			continue
		}
		kept = append(kept, ln)
	}
	return strings.TrimRight(strings.Join(kept, "\n"), "\n"), nil
}

// ensureCleanTree refuses to proceed when the worktree has staged or unstaged
// changes to tracked files. Untracked files (status code "??") are allowed — a
// history rewrite never touches them.
func ensureCleanTree(dir string) error {
	out, err := repo.Git(dir, "status", "--porcelain")
	if err != nil {
		return err
	}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" || strings.HasPrefix(line, "?? ") {
			continue
		}
		return fmt.Errorf("worktree has uncommitted changes; commit or stash them before submitting")
	}
	return nil
}
