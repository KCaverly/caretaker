// Package stack computes the read-only status of a stacked-PR workflow, where
// each commit in a worktree branch becomes its own GitHub PR (identified by a
// `ct-stack-id` commit trailer) and PRs are chained bottom-to-top. This package
// only *observes*: it gathers local git state, last-fetched remote refs, and
// GitHub PR metadata, then reconciles them into a StackStatus. Nothing here
// submits, restacks, or mutates anything.
//
// The gathering (gitdata.go, ghdata.go) is deliberately kept separate from the
// reconciliation (reconcile.go): every subprocess lives in a gather function so
// the reconciler is a pure function over plain data, exhaustively unit-testable
// without a repo or a network.
package stack

// State is a single commit's lifecycle position within its stack. The string
// values are the stable JSON contract (schema 1); callers switch on them, so
// they must not change without bumping the schema.
type State string

const (
	// StateUnsubmitted: the commit carries no ct-stack-id trailer, so it has
	// never entered the stack workflow. A valid state, not an error.
	StateUnsubmitted State = "unsubmitted"
	// StateUnpushed: the commit has a trailer but no remote branch
	// ct/<worktree>/<id> exists in the last-fetched refs.
	StateUnpushed State = "unpushed"
	// StateDiverged: the remote branch exists but its tip differs from the local
	// commit SHA (the commit was amended/rebased after it was last pushed).
	StateDiverged State = "diverged"
	// StateMissingPR: the remote branch is in sync but no PR has it as head.
	StateMissingPR State = "missing-pr"
	// StateOpen: an open PR tracks this commit and the remote tip matches locally.
	StateOpen State = "open"
	// StateMerged: a PR for this commit's branch is MERGED, yet the commit is
	// still in <main>..HEAD. Squash merges never reuse the commit's SHA, so this
	// is detected by trailer->branch->PR, never by SHA — it signals the stack
	// needs a restack to drop the landed commit.
	StateMerged State = "merged"
	// StateClosed: a PR for this commit's branch was closed without merging — an
	// escalation the human must resolve.
	StateClosed State = "closed"
	// StateDuplicateID: the same trailer id appears on two local commits, or the
	// trailer value is malformed (see gitdata.go on why git can concatenate two
	// trailers without a separator). An escalation: ids must be unique per commit.
	StateDuplicateID State = "duplicate-id"
)

// Checks is the collapsed CI summary for a PR: a single-word summary plus the
// names of any failing checks (empty when nothing is failing).
type Checks struct {
	Summary string   `json:"summary"` // one of: passing, failing, pending, none
	Failing []string `json:"failing"`
}

// PR is the GitHub pull-request view attached to a commit in the JSON output. It
// is nil for commits that have no PR (unsubmitted/unpushed/missing-pr).
type PR struct {
	Number    int     `json:"number"`
	URL       string  `json:"url"`
	Base      string  `json:"base"`      // baseRefName the PR targets
	Draft     bool    `json:"draft"`     // isDraft
	Review    string  `json:"review"`    // reviewDecision (APPROVED, REVIEW_REQUIRED, "", …)
	Mergeable string  `json:"mergeable"` // MERGEABLE, CONFLICTING, UNKNOWN, "" when unknown
	MergedAt  *string `json:"merged_at"` // RFC3339 when merged, else nil
	Checks    Checks  `json:"checks"`
}

// Commit is one commit in the stack, bottom-first (Position 1 is the oldest
// commit, the bottom of the stack).
type Commit struct {
	Position     int     `json:"position"`
	SHA          string  `json:"sha"`
	ShortSHA     string  `json:"short_sha"`
	Subject      string  `json:"subject"`
	StackID      *string `json:"stack_id"`      // trailer value, nil when absent
	RemoteBranch *string `json:"remote_branch"` // ct/<wt>/<id>, nil when unpushed
	RemoteInSync bool    `json:"remote_in_sync"`
	State        State   `json:"state"`
	PR           *PR     `json:"pr"`
}

// Orphan is an open PR whose ct/<worktree>/ head branch matches no local commit
// — surfaced for the human but never acted on by the engine.
type Orphan struct {
	Number  int    `json:"number"`
	URL     string `json:"url"`
	Head    string `json:"head"`
	StackID string `json:"stack_id"`
}

// Stack is the stack-level rollup: its size, whether the PR base chain is
// well-formed, the single next-action hint, per-state counts (only non-zero
// states appear), and any orphan PRs.
type Stack struct {
	Size        int           `json:"size"`
	BaseChainOK bool          `json:"base_chain_ok"`
	NextAction  string        `json:"next_action"`
	Counts      map[State]int `json:"counts"`
	Orphans     []Orphan      `json:"orphans"`
}

// GitHub records whether the GitHub half of the status could be gathered. When
// Available is false, PR-derived fields are absent but the local stack shape is
// still fully rendered; Warnings explain why (gh missing, unauthenticated, …).
type GitHub struct {
	Available bool     `json:"available"`
	Warnings  []string `json:"warnings"`
}

// MergeHint is the exact squash subject/body the merging agent should pass to
// `gh pr merge --squash` so a multi-commit squash keeps this commit's message
// verbatim (trailer included) instead of concatenating stale messages. It is
// present only when next_action is "merge".
type MergeHint struct {
	Number  int    `json:"number"`
	Subject string `json:"subject"`
	Body    string `json:"body"` // commit message body, ct-stack-id trailer included
}

// StackStatus is the top-level JSON contract (schema 1). Additive fields do not
// bump the schema; renames or removals do.
type StackStatus struct {
	Schema      int        `json:"schema"`
	GeneratedAt string     `json:"generated_at"` // RFC3339 UTC
	Repo        string     `json:"repo"`         // repo directory name
	Worktree    string     `json:"worktree"`     // worktree name
	Branch      string     `json:"branch"`       // worktree branch
	MainBranch  string     `json:"main_branch"`
	Fetched     bool       `json:"fetched"`
	GitHub      GitHub     `json:"github"`
	Stack       Stack      `json:"stack"`
	Commits     []Commit   `json:"commits"`
	MergeHint   *MergeHint `json:"merge_hint,omitempty"`
}

// Params configures a status computation. Every git and gh subprocess runs in
// WorktreeDir; MainBranch is the primary worktree's branch, resolved by the
// caller (the CLI), so this package never has to re-discover it.
type Params struct {
	RepoName     string // repo directory name -> JSON "repo"
	WorktreeName string // worktree leaf name -> ct/<name>/ prefix and JSON "worktree"
	WorktreeDir  string // where git/gh run
	Branch       string // worktree branch short name
	MainBranch   string // primary worktree's branch
	Fetch        bool   // run `git fetch origin` first
}
