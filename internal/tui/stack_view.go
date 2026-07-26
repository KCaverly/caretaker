package tui

import (
	"fmt"
	"os/exec"
	"runtime"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/KCaverly/caretaker/internal/repo"
	"github.com/KCaverly/caretaker/internal/stack"
)

// stackFreshFor bounds how long a cached stack status steers the deck before a
// passive refresh re-fetches it. The deck's `r` ignores this window.
const stackFreshFor = 5 * time.Minute

// stackEntry is one worktree's cached `ct stack status`, keyed by wsKey. loading
// marks a fetch in flight (so the render path never draws a half-state and the
// kick logic never double-issues); err records a failed fetch so the deck simply
// shows nothing rather than a wrong glyph.
type stackEntry struct {
	status    stack.StackStatus
	err       error
	loading   bool
	fetchedAt time.Time
}

// stackView backs the stack screen. It stamps the target worktree and the params
// a re-fetch/submit/restack reuses. When status is non-nil it drives the
// structured chain view (a cursor over the commit rows); otherwise the pre-split
// body lines and scroll offset drive the text-window path (errors and the restack
// dry-run plan). It tracks two transient flags: working (a pipeline call is in
// flight, so the body shows a one-line "working…") and confirmRestack (a restack
// dry-run plan is on screen awaiting enter, which runs it for real).
type stackView struct {
	repoName, wtName, key string
	params                stack.Params

	status *stack.StackStatus
	cursor int

	body   []string
	offset int

	working        bool
	confirmRestack bool
	confirmReuse   bool

	// split mode: `d` collapses the list to a narrow left column and opens the
	// cursored commit's patch in a right pane. splitFocus routes keys to the list
	// or the diff pane; diffCache holds each commit's fetched/rendered patch keyed
	// by full SHA (nil until a fetch is kicked; cleared on refresh).
	split      bool
	splitFocus splitPane
	diffCache  map[string]stackCommitDiff
}

// splitPane selects which half of split mode owns the keyboard.
type splitPane int

const (
	paneStack splitPane = iota
	paneDiff
)

// uncommittedKey is the diffCache key for the synthetic uncommitted row. Every
// other key is a full hex SHA, so a leading NUL cannot collide with one.
const uncommittedKey = "\x00uncommitted"

// stackSplitDivider is the width joinColumns spends on the " │ " between the two
// columns; the layout budget has to account for it or the rows overrun the
// terminal.
const stackSplitDivider = 3

// stackSplitMinLeft and stackSplitMinRight are the narrowest each column may be
// squeezed to before the preview stops being worth drawing: enough for a
// truncated commit subject on the left, and enough for a diff line to carry more
// than its leading `+` on the right.
const (
	stackSplitMinLeft  = 24
	stackSplitMinRight = 33
)

// stackSplitMinWidth is the narrowest terminal the preview will draw in. Below
// it the split is not merely cramped: the two columns plus the divider cannot be
// packed into the width at all, and the old arithmetic silently emitted rows
// wider than the terminal (a 30-column window drew 37-column rows, which wrap
// and shear the whole layout). Narrower than this, the screen shows the plain
// stack list instead.
const stackSplitMinWidth = stackSplitMinLeft + stackSplitDivider + stackSplitMinRight

// stackUntrackedCap bounds how many untracked files the uncommitted row renders
// the contents of. Each one costs a git subprocess, and a stack worktree
// realistically carries a handful; the surplus is still counted in the section
// rule so a `git init`-shaped explosion is visible rather than silently cut.
const stackUntrackedCap = 20

// stackRow is one selectable line in the stack list. Rows are the commits
// bottom-first, then — when the worktree is dirty — a synthetic row for the
// uncommitted work sitting above the tip. Keeping the cursor on rows rather than
// on status.Commits is what lets that trailing row behave like any other: it
// takes the cursor, drives the preview pane, and windows with the rest.
type stackRow struct {
	commit *stack.Commit // nil marks the uncommitted row
}

// uncommitted reports whether r is the synthetic trailing row.
func (r stackRow) uncommitted() bool { return r.commit == nil }

// diffKey is the row's key into the per-row diff cache.
func (r stackRow) diffKey() string {
	if r.uncommitted() {
		return uncommittedKey
	}
	return r.commit.SHA
}

// stackRows builds the cursor's row list: every commit in the loaded status,
// then the uncommitted row when the worktree currently has uncommitted work.
//
// The dirty flag is read from the deck's live view rather than cached on
// stackView because it changes underneath the screen — an agent editing files in
// that worktree flips it — and a stale copy would leave a row selectable after
// the tree went clean, or hide one that just appeared.
//
// Commits are bottom-first (position 1 is closest to main), and the list renders
// in that order, so uncommitted work belongs last: it sits above the tip.
func (m Model) stackRows() []stackRow {
	sv := m.stackView
	if sv.status == nil {
		return nil
	}
	rows := make([]stackRow, 0, len(sv.status.Commits)+1)
	for i := range sv.status.Commits {
		rows = append(rows, stackRow{commit: &sv.status.Commits[i]})
	}
	if it, ok := m.activeByKey(sv.key); ok && it.view.Dirty {
		rows = append(rows, stackRow{})
	}
	return rows
}

// stackRowAt returns the row under the cursor, clamped, and whether there is one.
func (m Model) stackRowAt(cursor int) (stackRow, bool) {
	rows := m.stackRows()
	if len(rows) == 0 {
		return stackRow{}, false
	}
	return rows[clamp(cursor, 0, len(rows)-1)], true
}

// splitShown reports whether the preview is actually on screen. The split flag
// alone is not enough: a terminal too narrow to lay out two columns falls back
// to the plain list, and the key routing has to agree with the renderer about
// that or keys would drive a pane nobody can see.
func (m Model) splitShown() bool {
	return m.stackView.split && m.stackView.status != nil && m.width >= stackSplitMinWidth
}

// stackCommitDiff is one commit's per-commit diff in the split pane's cache:
// loading while the fetch is in flight, err on a failed fetch, else the rendered
// scope (reusing the diff viewer's scopeContent) and its scroll offset.
type stackCommitDiff struct {
	loading bool
	err     error
	scope   scopeContent
	offset  int
}

// --- messages ---

// stackStatusMsg carries one passive/overlay status fetch back to the UI
// goroutine, keyed by wsKey so the cache and (if open) the overlay update
// together. It also lands from the overlay's `r` re-fetch.
type stackStatusMsg struct {
	key    string
	status stack.StackStatus
	err    error
}

// stackSubmitMsg carries a submit pipeline result; stackRestackMsg carries a
// restack result, with dryRun distinguishing the plan phase (which only re-renders
// the overlay) from the real run (which also refreshes the deck cache).
type stackSubmitMsg struct {
	key string
	res stack.SubmitResult
	err error
}

type stackRestackMsg struct {
	key    string
	res    stack.RestackResult
	dryRun bool
	reuse  bool
	err    error
}

type stackMergeMsg struct {
	key string
	res stack.MergeResult
	err error
}

// stackCommitDiffMsg carries one per-commit diff fetch back to the UI goroutine.
// key is the overlay's wsKey (so a fetch that lands after the overlay closed or
// switched worktrees is dropped) and sha is the commit it renders.
type stackCommitDiffMsg struct {
	key, sha string
	body     string
	stat     []repo.FileStat
	err      error

	// Uncommitted-row only: the untracked paths whose contents body carries, and
	// how many more were left unrendered by stackUntrackedCap.
	untracked      []string
	untrackedExtra int
}

// --- params ---

// stackParams builds a stack.Params for a worktree, mirroring the CLI's
// resolveStackParams from the deck's already-resolved facts (the main branch is
// the base ahead/behind was measured against). It reports false for the primary
// tree, a detached branch, or a worktree with no known base. Fetch is always
// false: passive display never blocks the UI on a network fetch.
func stackParams(it activeItem) (stack.Params, bool) {
	v := it.view
	if v.WT.IsMain || v.BaseBranch == "" || v.WT.Branch == "" {
		return stack.Params{}, false
	}
	return stack.Params{
		RepoName:     it.repo.Name,
		WorktreeName: v.WT.Name,
		WorktreeDir:  v.WT.Path,
		Branch:       v.WT.Branch,
		MainBranch:   v.BaseBranch,
		Fetch:        false,
	}, true
}

// --- passive cache ---

// kickStackFetches issues one status fetch per stackable active worktree, keyed
// by wsKey. It skips entries already loading and — unless force is set (the deck's
// `r`) — entries fresher than stackFreshFor. Worktrees with nothing ahead of main
// carry no stack, so they are skipped outright. It mutates the cache (marking the
// kicked entries loading) and returns the commands to run.
func (m *Model) kickStackFetches(force bool) []tea.Cmd {
	if m.stackFetch == nil {
		return nil
	}
	var cmds []tea.Cmd
	for _, it := range m.active {
		if it.view.Ahead == 0 {
			continue
		}
		p, ok := stackParams(it)
		if !ok {
			continue
		}
		key := wsKey(it.repo.Name, it.view.WT.Name)
		e, exists := m.stackInfo[key]
		if e.loading {
			continue
		}
		if !force && exists && e.err == nil && time.Since(e.fetchedAt) < stackFreshFor {
			continue
		}
		m.markStackLoading(key)
		cmds = append(cmds, m.fetchStackCmd(key, p))
	}
	return cmds
}

// markStackLoading flags a cache entry as fetch-in-flight so the render path draws
// nothing for it and a concurrent kick won't re-issue.
func (m *Model) markStackLoading(key string) {
	if m.stackInfo == nil {
		m.stackInfo = map[string]stackEntry{}
	}
	e := m.stackInfo[key]
	e.loading = true
	m.stackInfo[key] = e
}

// --- commands ---

func (m Model) fetchStackCmd(key string, p stack.Params) tea.Cmd {
	fetch := m.stackFetch
	return func() tea.Msg {
		st, err := fetch(p)
		return stackStatusMsg{key: key, status: st, err: err}
	}
}

func (m Model) submitStackCmd(key string, p stack.Params) tea.Cmd {
	submit := m.stackSubmit
	return func() tea.Msg {
		res, err := submit(stack.SubmitOptions{Params: p})
		return stackSubmitMsg{key: key, res: res, err: err}
	}
}

func (m Model) restackStackCmd(key string, p stack.Params, dryRun, reuse bool) tea.Cmd {
	restack := m.stackRestack
	reuseFn := m.stackReuse
	return func() tea.Msg {
		var res stack.RestackResult
		var err error
		if reuse {
			res, err = reuseFn(stack.ReuseOptions{Params: p, DryRun: dryRun})
		} else {
			res, err = restack(stack.RestackOptions{Params: p, DryRun: dryRun})
		}
		return stackRestackMsg{key: key, res: res, dryRun: dryRun, reuse: reuse, err: err}
	}
}

func (m Model) mergeStackCmd(key string, p stack.Params) tea.Cmd {
	merge := m.stackMerge
	return func() tea.Msg {
		res, err := merge(stack.MergeOptions{Params: p})
		return stackMergeMsg{key: key, res: res, err: err}
	}
}

// fetchCommitDiffCmd runs one per-commit diff fetch off the UI goroutine: the
// commit's patch and numstat via repo.DiffCommit/NumstatCommit. Any git error
// short-circuits into the message's err, which the handler stores so the pane
// shows a red line rather than a stale/blank diff.
func (m Model) fetchCommitDiffCmd(key, dir, sha string) tea.Cmd {
	return func() tea.Msg {
		body, err := repo.DiffCommit(dir, sha)
		if err != nil {
			return stackCommitDiffMsg{key: key, sha: sha, err: err}
		}
		stat, err := repo.NumstatCommit(dir, sha)
		if err != nil {
			return stackCommitDiffMsg{key: key, sha: sha, err: err}
		}
		return stackCommitDiffMsg{key: key, sha: sha, body: body, stat: stat}
	}
}

// fetchUncommittedDiffCmd runs the uncommitted row's fetch off the UI goroutine:
// the staged+unstaged patch against HEAD and its numstat, plus the contents of
// the untracked files (which `git diff HEAD` never reports) appended to the same
// body. Staged and unstaged are deliberately one view — it mirrors what the deck
// already counts as the worktree's uncommitted work.
//
// Untracked rendering is capped; the surplus is reported so the section rule can
// say how much was left out.
func (m Model) fetchUncommittedDiffCmd(key, dir string) tea.Cmd {
	return func() tea.Msg {
		wt := repo.Worktree{Path: dir}
		body, err := repo.DiffUncommitted(wt)
		if err != nil {
			return stackCommitDiffMsg{key: key, sha: uncommittedKey, err: err}
		}
		stat, err := repo.NumstatUncommitted(wt)
		if err != nil {
			return stackCommitDiffMsg{key: key, sha: uncommittedKey, err: err}
		}
		untracked, err := repo.UntrackedFiles(wt)
		if err != nil {
			return stackCommitDiffMsg{key: key, sha: uncommittedKey, err: err}
		}
		extra := 0
		if len(untracked) > stackUntrackedCap {
			extra = len(untracked) - stackUntrackedCap
			untracked = untracked[:stackUntrackedCap]
		}
		// A per-path read failure is already swallowed inside DiffUntracked, so
		// the tracked patch still renders even if a new file vanished mid-fetch.
		newFiles, err := repo.DiffUntracked(dir, untracked)
		if err != nil {
			return stackCommitDiffMsg{key: key, sha: uncommittedKey, err: err}
		}
		return stackCommitDiffMsg{
			key: key, sha: uncommittedKey, body: body + newFiles, stat: stat,
			untracked: untracked, untrackedExtra: extra,
		}
	}
}

func (m Model) archivePreflightCmd(it activeItem, p stack.Params) tea.Cmd {
	return func() tea.Msg {
		p.Fetch = true
		st, err := stack.Status(p)
		if err == nil && st.Stack.NextAction != "complete" {
			err = fmt.Errorf("stack is no longer complete (next action: %s)", st.Stack.NextAction)
		}
		fingerprint := ""
		dirty := false
		if err == nil {
			fingerprint, dirty, err = archiveWorktreeFingerprint(it.view.WT.Path)
		}
		return archivePreflightMsg{it: it, fingerprint: fingerprint, dirty: dirty, err: err}
	}
}

func archiveWorktreeFingerprint(dir string) (string, bool, error) {
	status, err := repo.Git(dir, "status", "--porcelain=v1", "-z", "--untracked-files=all")
	if err != nil {
		return "", false, err
	}
	diff, err := repo.Git(dir, "diff", "--binary", "HEAD")
	if err != nil {
		return "", false, err
	}
	untracked, err := repo.Git(dir, "ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return "", false, err
	}
	var hashes strings.Builder
	for _, path := range strings.Split(untracked, "\x00") {
		if path == "" {
			continue
		}
		hash, err := repo.Git(dir, "hash-object", "--", path)
		if err != nil {
			return "", false, err
		}
		fmt.Fprintf(&hashes, "%s\x00%s\x00", path, strings.TrimSpace(hash))
	}
	return status + "\x00" + diff + "\x00" + hashes.String(), status != "", nil
}

// --- overlay entry ---

// enterStackOverlay opens the overlay for a worktree in its working state; the
// caller issues the matching fetch/submit/restack command.
func (m Model) enterStackOverlay(key, repoName, wtName string, p stack.Params) Model {
	m.stackOpen = true
	m.stackView = stackView{repoName: repoName, wtName: wtName, key: key, params: p, working: true}
	return m
}

// --- message handling ---

// applyStackStatus records a status fetch in the cache and, when the overlay is
// open on the same worktree, re-renders it with the fresh (or errored) status.
func (m *Model) applyStackStatus(msg stackStatusMsg) {
	if m.stackInfo == nil {
		m.stackInfo = map[string]stackEntry{}
	}
	m.stackInfo[msg.key] = stackEntry{status: msg.status, err: msg.err, fetchedAt: time.Now()}
	if !m.stackOpen || m.stackView.key != msg.key {
		return
	}
	m.stackView.working = false
	m.stackView.confirmRestack = false
	m.stackView.confirmReuse = false
	m.stackView.offset = 0
	// A fresh status invalidates every per-commit diff: SHAs may have changed
	// under a restack/amend, so stale patches must not linger behind the split.
	m.stackView.diffCache = nil
	if msg.err != nil {
		m.stackView.status = nil
		m.stackView.body = stackErrorBody("status failed: "+msg.err.Error(), nil)
		return
	}
	st := msg.status
	m.stackView.status = &st
	m.stackView.body = nil
	m.clampStackCursor()
}

// applyStackSubmit refreshes the deck cache from a submit result and, when the
// overlay is open on the same worktree, shows the post-submit status (or the
// error plus whatever steps executed).
func (m *Model) applyStackSubmit(msg stackSubmitMsg) {
	if m.stackInfo == nil {
		m.stackInfo = map[string]stackEntry{}
	}
	if msg.err == nil {
		m.stackInfo[msg.key] = stackEntry{status: msg.res.Status, fetchedAt: time.Now()}
	} else if e, ok := m.stackInfo[msg.key]; ok {
		e.loading = false
		m.stackInfo[msg.key] = e
	}
	if !m.stackOpen || m.stackView.key != msg.key {
		return
	}
	m.stackView.working = false
	m.stackView.confirmRestack = false
	m.stackView.confirmReuse = false
	m.stackView.offset = 0
	if msg.err != nil {
		m.stackView.status = nil
		m.stackView.body = stackErrorBody("submit failed: "+msg.err.Error(), msg.res.Executed)
		return
	}
	st := msg.res.Status
	m.stackView.status = &st
	m.stackView.body = nil
	m.clampStackCursor()
}

// applyStackRestack handles both restack phases. The dry-run shows the plan and
// arms confirmRestack (enter runs it for real); the real run refreshes the deck
// cache and shows the post-restack status. Errors show the message plus any steps
// that executed before the failure.
func (m *Model) applyStackRestack(msg stackRestackMsg) {
	if !msg.dryRun && msg.err == nil {
		if m.stackInfo == nil {
			m.stackInfo = map[string]stackEntry{}
		}
		m.stackInfo[msg.key] = stackEntry{status: msg.res.Status, fetchedAt: time.Now()}
	}
	if !m.stackOpen || m.stackView.key != msg.key {
		return
	}
	m.stackView.working = false
	m.stackView.confirmRestack = false
	m.stackView.confirmReuse = false
	m.stackView.offset = 0
	if !msg.dryRun {
		// A real restack rewrites the stack's SHAs, so every cached patch is
		// keyed to a commit that no longer exists. Drop them rather than let a
		// resurrected SHA serve a pre-restack diff.
		m.stackView.diffCache = nil
	}
	switch {
	case msg.err != nil:
		m.stackView.status = nil
		verb := "restack"
		if msg.reuse {
			verb = "reuse"
		}
		m.stackView.body = stackErrorBody(verb+" failed: "+msg.err.Error(), msg.res.Executed)
	case msg.res.Nothing:
		st := msg.res.Status
		m.stackView.status = &st
		m.stackView.body = nil
		m.clampStackCursor()
	case msg.dryRun:
		m.stackView.status = nil
		if msg.reuse {
			m.stackView.body = renderStackBody(stack.RenderReusePlan(msg.res))
		} else {
			m.stackView.body = renderStackBody(stack.RenderRestackPlan(msg.res))
		}
		m.stackView.confirmRestack = true
		m.stackView.confirmReuse = msg.reuse
	default:
		st := msg.res.Status
		m.stackView.status = &st
		m.stackView.body = nil
		m.clampStackCursor()
	}
}

func (m *Model) applyStackMerge(msg stackMergeMsg) {
	if msg.err == nil {
		m.stackInfo[msg.key] = stackEntry{status: msg.res.Status, fetchedAt: time.Now()}
	}
	if !m.stackOpen || m.stackView.key != msg.key {
		return
	}
	m.stackView.working = false
	m.stackView.offset = 0
	if msg.err != nil {
		m.stackView.status = nil
		m.stackView.body = stackErrorBody("merge failed: "+msg.err.Error(), msg.res.Executed)
		return
	}
	st := msg.res.Status
	m.stackView.status = &st
	m.stackView.body = nil
	m.clampStackCursor()
}

// applyStackCommitDiff records one per-commit diff fetch into the split pane's
// cache. A fetch that lands after the overlay closed or switched worktrees is
// dropped by key. On success it renders the patch into a single-section
// scopeContent (titled with the commit subject) via the diff viewer's builder,
// reusing its J/K file-header indexing and +/− colouring.
func (m *Model) applyStackCommitDiff(msg stackCommitDiffMsg) {
	if !m.stackOpen || m.stackView.key != msg.key {
		return
	}
	if m.stackView.diffCache == nil {
		m.stackView.diffCache = map[string]stackCommitDiff{}
	}
	if msg.err != nil {
		m.stackView.diffCache[msg.sha] = stackCommitDiff{err: msg.err}
		return
	}
	title, meta := "commit", ""
	if msg.sha == uncommittedKey {
		title = uncommittedLabel
		if msg.untrackedExtra > 0 {
			meta = fmt.Sprintf("+%d more untracked", msg.untrackedExtra)
		}
	} else if m.stackView.status != nil {
		for _, c := range m.stackView.status.Commits {
			if c.SHA == msg.sha {
				if c.Subject != "" {
					title = c.Subject
				}
				break
			}
		}
	}
	var b diffBuilder
	files, add, del := appendDiffSection(&b, title, meta, msg.stat, msg.untracked, msg.body, m.width)
	scope := finishScope(b, files, add, del)
	// Carry the scroll position across a re-fetch. Commits are only ever fetched
	// once so this is inert for them, but the uncommitted row revalidates on every
	// landing, and snapping back to the top each time would make it unreadable
	// while an agent is writing to the worktree.
	prev := m.stackView.diffCache[msg.sha]
	m.stackView.diffCache[msg.sha] = stackCommitDiff{
		scope:  scope,
		offset: clamp(prev.offset, 0, max(0, len(scope.lines)-1)),
	}
}

// ensureCommitDiff kicks a per-commit diff fetch for sha when the cache has no
// entry yet, marking it loading so the render path draws a "loading…" line and a
// concurrent request won't re-issue. It returns nil when sha is empty or already
// cached (loaded or loading).
func (m *Model) ensureCommitDiff(sha string) tea.Cmd {
	if sha == "" {
		return nil
	}
	sv := &m.stackView
	if sv.diffCache == nil {
		sv.diffCache = map[string]stackCommitDiff{}
	}
	if _, ok := sv.diffCache[sha]; ok {
		return nil
	}
	sv.diffCache[sha] = stackCommitDiff{loading: true}
	return m.fetchCommitDiffCmd(sv.key, sv.params.WorktreeDir, sha)
}

// ensureRowDiff kicks the fetch behind whichever row the preview is showing.
//
// The two row kinds want opposite caching. A commit's patch is immutable, so a
// cached entry is reused and nothing is re-issued. Uncommitted work is the
// opposite: it changes under the screen constantly — that is the point of
// watching it — so landing on the row always revalidates. To keep that from
// flashing, the previously fetched patch stays on screen while the new one is in
// flight; only the very first landing shows "loading…".
func (m *Model) ensureRowDiff(r stackRow) tea.Cmd {
	sv := &m.stackView
	if !r.uncommitted() {
		return m.ensureCommitDiff(r.diffKey())
	}
	if sv.diffCache == nil {
		sv.diffCache = map[string]stackCommitDiff{}
	}
	if _, ok := sv.diffCache[uncommittedKey]; !ok {
		sv.diffCache[uncommittedKey] = stackCommitDiff{loading: true}
	}
	return m.fetchUncommittedDiffCmd(sv.key, sv.params.WorktreeDir)
}

// ensureCursorDiff kicks the fetch for the row under the cursor.
func (m *Model) ensureCursorDiff() tea.Cmd {
	r, ok := m.stackRowAt(m.stackView.cursor)
	if !ok {
		return nil
	}
	return m.ensureRowDiff(r)
}

// ensureSplitDiff re-kicks the cursored commit's diff fetch when the split pane
// is on screen, and is a no-op otherwise. Every status-bearing result — a
// refresh, submit, restack, or merge — either drops the diff cache outright or
// moves the commits out from under it (a restack rewrites SHAs), so without this
// the pane is left looking up a key nothing will ever populate and sits on
// "loading diff…" until the user happens to move the cursor. Callers run it
// after the matching apply, once the new status and cursor are in place.
func (m *Model) ensureSplitDiff() tea.Cmd {
	if !m.stackOpen || !m.splitShown() {
		return nil
	}
	return m.ensureCursorDiff()
}

// clampStackCursor keeps the cursor inside the current row list after a status
// swap. It must count rows rather than commits: with a dirty worktree the last
// selectable row is the uncommitted one, and clamping to the commit count would
// yank the cursor off it on every passive refresh.
func (m *Model) clampStackCursor() {
	m.stackView.cursor = clampCursor(m.stackView.cursor, len(m.stackRows()))
}

// clampCursor keeps a commit-row cursor inside a list of n rows, collapsing to 0
// for an empty list.
func clampCursor(c, n int) int {
	if n == 0 {
		return 0
	}
	return clamp(c, 0, n-1)
}

// renderStackBody splits a renderer's block into scrollable lines.
func renderStackBody(s string) []string {
	return strings.Split(strings.TrimRight(s, "\n"), "\n")
}

// stackErrorBody builds the overlay body for a failed pipeline call: the error
// line, then any executed steps (for submit/restack, so a partial run is legible).
func stackErrorBody(msg string, executed []string) []string {
	body := []string{msg}
	if len(executed) > 0 {
		body = append(body, "", "executed:")
		for _, step := range executed {
			body = append(body, "  "+step)
		}
	}
	return body
}

// --- key routing ---

// handleStack routes keys while the stack screen is open, mirroring the usage
// overlay's modal behavior and swallowing every other key so none leaks beneath.
// esc/q leave and r re-fetches in every state. The rest split on the render path:
// with a structured status the cursor moves (j/k), submit (s), restack (R) and
// open PR (o) act on the stack, and g/G jump the cursor; d toggles the diff
// preview; enter is inert here and only confirms a pending restack; with a text
// body (an error or the restack dry-run plan) the shared movement keys scroll it
// and — in the restack-confirm state — enter runs the restack for real. In split
// mode handleStackSplit intercepts first so its navigation/focus keys don't
// double-fire against the shared switch.
func (m Model) handleStack(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	sv := m.stackView
	avail := m.stackViewport()
	maxOff := max(0, len(sv.body)-avail)
	n := len(m.stackRows())
	if m.splitShown() && !sv.working && !sv.confirmRestack {
		handled, mm, cmd := m.handleStackSplit(msg, n)
		if handled {
			return mm, cmd
		}
		// Fell through (a paneStack action key): act on the whole stack below.
		m = mm.(Model)
	}
	switch msg.String() {
	case "esc", "q":
		// In the restack-confirm state esc cancels without executing; either way
		// the screen closes.
		m.stackOpen = false
		m.stackView = stackView{}
	case "enter":
		// The only enter action is confirming a pending restack. On a plain status
		// there is nothing per-row to open — every commit belongs to the one
		// worktree — so enter is intentionally inert there.
		if sv.confirmRestack && !sv.working {
			m.stackView.working = true
			m.stackView.confirmRestack = false
			return m, m.restackStackCmd(sv.key, sv.params, false, sv.confirmReuse)
		}
	case "d":
		// Toggle the split diff-preview pane. Entering kicks the cursored commit's
		// diff fetch; the in-split toggle-off is handled by handleStackSplit.
		if sv.status != nil && n > 0 && !sv.working && !sv.confirmRestack {
			// Refuse loudly rather than opening a preview the terminal cannot
			// lay out — silently doing nothing reads as a broken key.
			if m.width < stackSplitMinWidth {
				return m, m.flashCmd("terminal too narrow for the preview")
			}
			m.stackView.split = true
			m.stackView.splitFocus = paneStack
			return m, m.ensureCursorDiff()
		}
	case "r":
		if !sv.working {
			m.stackView.working = true
			m.stackView.confirmRestack = false
			m.markStackLoading(sv.key)
			return m, m.fetchStackCmd(sv.key, sv.params)
		}
	case "s":
		// Submit the stack, but only when the rollup carries submit-able work.
		if sv.status != nil && !sv.working {
			if stackHasSubmitWork(*sv.status) {
				m.stackView.working = true
				m.stackView.confirmRestack = false
				return m, m.submitStackCmd(sv.key, sv.params)
			}
			return m, m.flashCmd("nothing to submit")
		}
	case "R":
		// Restack, dry-run first: the plan lands as a text body and arms the
		// confirm state, exactly as the palette's restack row does.
		if sv.status != nil && !sv.working {
			if !stackCanRestack(*sv.status) {
				return m, m.flashCmd("nothing to restack")
			}
			m.stackView.working = true
			m.stackView.confirmRestack = false
			return m, m.restackStackCmd(sv.key, sv.params, true, false)
		}
	case "u":
		if sv.status != nil && !sv.working && sv.status.Stack.NextAction == "complete" {
			m.stackView.working = true
			m.stackView.confirmRestack = false
			return m, m.restackStackCmd(sv.key, sv.params, true, true)
		}
	case "a":
		if sv.status != nil && !sv.working && sv.status.Stack.NextAction == "complete" {
			if it, ok := m.activeByKey(sv.key); ok {
				m.stackView.working = true
				return m, m.archivePreflightCmd(it, sv.params)
			}
		}
	case "M":
		if sv.status != nil && !sv.working {
			if !stackCanMerge(*sv.status) {
				return m, m.flashCmd("PR is not mergeable into main")
			}
			return m.requestStackMerge(sv.key, sv.params, *sv.status)
		}
	case "o":
		// Open the selected commit's PR in the browser.
		if sv.status != nil && n > 0 {
			return m.openRowPR(sv.cursor)
		}
	case "j", "down":
		if sv.status != nil {
			m.stackView.cursor = clamp(sv.cursor+1, 0, max(0, n-1))
		} else {
			m.stackView.offset = clamp(sv.offset+1, 0, maxOff)
		}
	case "k", "up":
		if sv.status != nil {
			m.stackView.cursor = clamp(sv.cursor-1, 0, max(0, n-1))
		} else {
			m.stackView.offset = clamp(sv.offset-1, 0, maxOff)
		}
	case "g":
		if sv.status != nil {
			m.stackView.cursor = 0
		} else {
			m.stackView.offset = 0
		}
	case "G":
		if sv.status != nil {
			m.stackView.cursor = max(0, n-1)
		} else {
			m.stackView.offset = maxOff
		}
	}
	return m, nil
}

// handleStackSplit routes keys while split mode is active. handled is true for
// every key it fully owns (the d toggle-off, tab focus flip, list navigation,
// and — via handleStackDiffPane — diff scrolling and n/p). It returns handled
// false for esc/q, which leave the screen entirely, and for a paneStack action
// key (s/R/M/a/u/r/o), leaving handleStack's shared switch to run the whole-stack
// action; those keys make no state change here, so the returned model is
// unchanged.
func (m Model) handleStackSplit(msg tea.KeyPressMsg, n int) (bool, tea.Model, tea.Cmd) {
	sv := m.stackView
	switch msg.String() {
	case "esc", "q":
		// Leaving is the shared switch's job. esc steps all the way out of the
		// stack screen rather than unwinding the preview first: one key, one
		// concept. Returning handled=false hands it down before the focus
		// dispatch below can swallow it.
		return false, m, nil
	case "d":
		// d owns the preview in both directions — it is the key that opened it.
		m.stackView.split = false
		m.stackView.splitFocus = paneStack
		return true, m, nil
	case "tab", "shift+tab":
		if sv.splitFocus == paneStack {
			m.stackView.splitFocus = paneDiff
		} else {
			m.stackView.splitFocus = paneStack
		}
		return true, m, nil
	}

	if sv.splitFocus == paneDiff {
		mm, cmd := m.handleStackDiffPane(msg, n)
		return true, mm, cmd
	}

	// paneStack focus: navigation moves the cursor and prefetches the new commit's
	// diff; action keys fall through to the shared whole-stack switch.
	switch msg.String() {
	case "j", "down":
		m.stackView.cursor = clamp(sv.cursor+1, 0, max(0, n-1))
		return true, m, m.ensureCursorDiff()
	case "k", "up":
		m.stackView.cursor = clamp(sv.cursor-1, 0, max(0, n-1))
		return true, m, m.ensureCursorDiff()
	case "g":
		m.stackView.cursor = 0
		return true, m, m.ensureCursorDiff()
	case "G":
		m.stackView.cursor = max(0, n-1)
		return true, m, m.ensureCursorDiff()
	case "s", "R", "M", "a", "u", "r", "o":
		return false, m, nil
	}
	return true, m, nil
}

// openRowPR opens the PR of the commit under cursor, flashing instead when the
// row carries none — an unsubmitted commit, or the uncommitted row, which has no
// PR by definition and never will until the work is committed and submitted.
func (m Model) openRowPR(cursor int) (tea.Model, tea.Cmd) {
	r, ok := m.stackRowAt(cursor)
	if !ok {
		return m, nil
	}
	if r.uncommitted() {
		return m, m.flashCmd("uncommitted work has no PR")
	}
	if r.commit.PR != nil && r.commit.PR.URL != "" {
		return m, openURLCmd(r.commit.PR.URL)
	}
	return m, m.flashCmd("no PR for this commit")
}

// handleStackDiffPane routes keys while split mode's diff pane has focus: the
// movement keys scroll the cached diff (clamped to its content), ]/[ jump between
// file headers, n/p walk to the neighbouring commit (staying in the diff pane and
// prefetching its diff), and o opens the cursored commit's PR. A missing/loading
// cache entry makes the scroll ops no-ops. Everything else is swallowed.
func (m Model) handleStackDiffPane(msg tea.KeyPressMsg, n int) (tea.Model, tea.Cmd) {
	sv := m.stackView
	sha := ""
	if r, ok := m.stackRowAt(sv.cursor); ok {
		sha = r.diffKey()
	}
	cd := sv.diffCache[sha]
	avail := max(1, (m.height-barHeight)-3)
	maxOff := max(0, len(cd.scope.lines)-avail)
	setOff := func(o int) {
		if m.stackView.diffCache == nil {
			return
		}
		e, ok := m.stackView.diffCache[sha]
		if !ok {
			return
		}
		e.offset = clamp(o, 0, maxOff)
		m.stackView.diffCache[sha] = e
	}
	switch msg.String() {
	case "j", "down":
		setOff(cd.offset + 1)
	case "k", "up":
		setOff(cd.offset - 1)
	case "ctrl+d":
		setOff(cd.offset + avail/2)
	case "ctrl+u":
		setOff(cd.offset - avail/2)
	case "g":
		setOff(0)
	case "G":
		setOff(maxOff)
	case "]", "J":
		setOff(nextFileLine(cd.scope.fileLines, cd.offset, maxOff))
	case "[", "K":
		setOff(prevFileLine(cd.scope.fileLines, cd.offset))
	case "n":
		m.stackView.cursor = clamp(sv.cursor+1, 0, max(0, n-1))
		return m, m.ensureCursorDiff()
	case "p":
		m.stackView.cursor = clamp(sv.cursor-1, 0, max(0, n-1))
		return m, m.ensureCursorDiff()
	case "o":
		if sv.status != nil && n > 0 {
			return m.openRowPR(sv.cursor)
		}
	}
	return m, nil
}

// stackDiffWheel scrolls split mode's focused diff pane by three lines per wheel
// notch, clamped to the cursored commit's cached content. A missing cache entry
// makes it a no-op.
func (m Model) stackDiffWheel(msg tea.MouseWheelMsg) (tea.Model, tea.Cmd) {
	mo := msg.Mouse()
	delta := 3
	if mo.Button == tea.MouseWheelUp {
		delta = -3
	} else if mo.Button != tea.MouseWheelDown {
		return m, nil
	}
	sv := m.stackView
	r, ok := m.stackRowAt(sv.cursor)
	if !ok || !m.splitShown() || m.stackView.diffCache == nil {
		return m, nil
	}
	sha := r.diffKey()
	cd, ok := m.stackView.diffCache[sha]
	if !ok {
		return m, nil
	}
	avail := max(1, (m.height-barHeight)-3)
	maxOff := max(0, len(cd.scope.lines)-avail)
	cd.offset = clamp(cd.offset+delta, 0, maxOff)
	m.stackView.diffCache[sha] = cd
	return m, nil
}

// openURLCmd opens url in the platform browser off the UI goroutine. Success is
// silent — the browser is the feedback; a launch failure flashes as a sticky
// error via the shared actionDoneMsg path.
func openURLCmd(url string) tea.Cmd {
	return func() tea.Msg {
		var name string
		var args []string
		switch runtime.GOOS {
		case "darwin":
			name = "open"
		case "windows":
			name, args = "rundll32", []string{"url.dll,FileProtocolHandler"}
		default:
			name = "xdg-open"
		}
		if err := exec.Command(name, append(args, url)...).Start(); err != nil {
			return actionDoneMsg{err: fmt.Errorf("open PR: %w", err)}
		}
		return nil
	}
}

// activeByKey finds the active item whose repo/worktree matches key, so the stack
// screen's open/diff actions can route through the deck's activate/openDiff.
func (m Model) activeByKey(key string) (activeItem, bool) {
	for _, it := range m.active {
		if wsKey(it.repo.Name, it.view.WT.Name) == key {
			return it, true
		}
	}
	return activeItem{}, false
}

// --- rendering ---

// stackChromeRows is the number of fixed rows renderStack draws around the
// scrollable body inside the box: the title, its blank spacer, a blank spacer
// above the footer, and the footer legend.
const stackChromeRows = 4

// stackMaxBodyRows caps the overlay's scrollable body so a large stack scrolls
// rather than growing an unwieldy box.
const stackMaxBodyRows = 18

// stackViewport is the number of scrollable body rows the overlay shows, shared
// by the render window and the key handler's scroll clamp so they agree.
func (m Model) stackViewport() int {
	budget := max(3, m.height-barHeight-stackChromeRows)
	return min(budget, stackMaxBodyRows)
}

// renderStack draws the stack screen, selecting among five states: a one-line
// "working…" while a pipeline call is in flight; the restack dry-run plan with an
// "enter run · esc cancel" confirm footer; the structured chain view (when a
// status is loaded); the text-window error body; and a "(no stack data)"
// placeholder. The working, confirm, error, and placeholder states share the
// text-window renderer; the structured view is its own layout.
func (m Model) renderStack(h int) string {
	sv := m.stackView
	confirmFooter := keyhint("enter", "run") + "   " + keyhint("esc", "cancel")
	closeFooter := keyhint("↑↓ / j k", "scroll") + "   " + keyhint("r", "refresh") + "   " + keyhint("esc", "return")
	switch {
	case sv.working:
		return m.renderStackText([]string{dimStyle.Render("working…")}, closeFooter, h)
	case sv.confirmRestack:
		return m.renderStackText(sv.body, confirmFooter, h)
	case m.splitShown():
		return m.renderStackSplit(*sv.status, sv.cursor, h)
	case sv.status != nil:
		return m.renderStackStatus(*sv.status, sv.cursor, h)
	case len(sv.body) > 0:
		return m.renderStackText(sv.body, closeFooter, h)
	default:
		return m.renderStackText([]string{dimStyle.Render("(no stack data)")}, closeFooter, h)
	}
}

// renderStackText draws the shared text-window path: the "STACK <repo> / <wt>"
// title, the body windowed by the scroll offset, and the given footer, inside the
// same centered box as the structured view.
func (m Model) renderStackText(body []string, footer string, h int) string {
	sv := m.stackView
	innerW := panelInnerWidth(m.width, 84)

	rows := []string{header("stack "+sv.repoName+" / "+sv.wtName, -1), ""}

	avail := m.stackViewport()
	start := clamp(sv.offset, 0, max(0, len(body)-1))
	end := min(len(body), start+avail)
	for i := start; i < end; i++ {
		rows = append(rows, "  "+ansi.Truncate(body[i], innerW-2, ""))
	}
	for i := end - start; i < avail; i++ {
		rows = append(rows, "")
	}

	rows = append(rows, "", "  "+footer)

	return renderPanel(rows, innerW, m.width, h)
}

// renderStackStatus draws the structured stack screen: a titled header, a chain
// row from main through each commit's subject, a cursored (and, for a tall stack,
// scrollable) commit list with per-commit PR facts, a one-line rollup summary,
// and an action footer whose submit/restack hints appear only when the rollup
// calls for them. It shares renderStack's centered box container.
func (m Model) renderStackStatus(st stack.StackStatus, cursor, h int) string {
	innerW := panelInnerWidth(m.width, 84)
	trunc := func(s string) string { return ansi.Truncate(s, innerW, "") }

	var rows []string

	// Title: "STACK  <repo> / <wt>" left, "base: <main>" right-justified.
	left := headerStyle.Render("STACK") + "  " + repoHdrStyle.Render(st.Repo) +
		dimStyle.Render(" / ") + nameStyle.Render(st.Worktree)
	right := dimStyle.Render("base: " + st.MainBranch)
	rows = append(rows, trunc(stackJustify(left, right, innerW)), "")

	// Commit list, windowed to keep the cursor visible. Each row gives the whole
	// subject the width its facts don't need, with the facts flush right.
	list := m.stackRows()
	if len(list) == 0 {
		rows = append(rows, trunc(dimStyle.Render("(no commits ahead of main)")))
	} else {
		chrome := 6
		budget := clamp(m.height-barHeight-chrome, 1, stackMaxBodyRows)
		listH := min(len(list), budget)
		start, end := windowBounds(len(list), cursor, listH)
		for i := start; i < end; i++ {
			rows = append(rows, m.stackListRow(list[i], i == cursor, innerW))
		}
	}
	rows = append(rows, "")

	// Rollup summary (or a note when GitHub is unavailable).
	if !st.GitHub.Available {
		rows = append(rows, trunc(dimStyle.Render("github unavailable — PR status omitted")))
	} else {
		rows = append(rows, trunc(dimStyle.Render(fmt.Sprintf("base %s · next: %s",
			okWord(st.Stack.BaseChainOK), st.Stack.NextAction))))
	}
	rows = append(rows, "")

	// Action footer: move always, submit/restack conditionally, then the rest.
	// refresh trails so it is the first to drop if the row must be truncated.
	parts := []string{keyhint("↑↓ / j k", "move")}
	if stackHasSubmitWork(st) {
		parts = append(parts, keyhint("s", "submit"))
	}
	if st.Stack.NextAction == "complete" {
		parts = append(parts, keyhint("a", "archive"), keyhint("u", "reuse"))
	} else if stackCanRestack(st) {
		parts = append(parts, keyhint("R", "restack"))
	}
	if stackCanMerge(st) {
		parts = append(parts, keyhint("M", "merge"))
	}
	parts = append(parts, keyhint("d", "preview"), keyhint("o", "open PR"),
		keyhint("esc", "return"), keyhint("r", "refresh"))
	rows = append(rows, trunc("  "+strings.Join(parts, "   ")))

	return renderPanel(rows, innerW, m.width, h)
}

// renderStackSplit draws split mode edge-to-edge (full m.width, like renderDiff,
// not the centered panel): a header naming the repo/worktree, base, and the
// active pane; a faint rule; a two-column body pairing the narrow commit list
// (left) with the cursored commit's patch (right); and a focus-dependent footer.
func (m Model) renderStackSplit(st stack.StackStatus, cursor, h int) string {
	commits := m.stackRows()
	focus := m.stackView.splitFocus

	paneName := func(name string, active bool) string {
		if active {
			return helpKeyStyle.Render(name)
		}
		return dimStyle.Render(name)
	}
	hdr := headerStyle.Render("STACK") + "  " + repoHdrStyle.Render(st.Repo) +
		dimStyle.Render("/") + nameStyle.Render(st.Worktree) +
		dimStyle.Render("   base: "+st.MainBranch) + dimStyle.Render("   [split] ") +
		paneName("list", focus == paneStack) + dimStyle.Render(" · ") +
		paneName("diff", focus == paneDiff)
	rule := diffRuleStyle.Render(strings.Repeat("─", max(1, m.width)))

	avail := max(1, h-3)
	// The three parts must sum to exactly m.width. Deriving rightW from leftW
	// rather than clamping both independently is what guarantees that: the old
	// pair of clamps could each satisfy its own floor and still overrun.
	maxLeft := max(stackSplitMinLeft, m.width-stackSplitDivider-stackSplitMinRight)
	leftW := clamp(min(38, m.width/3), stackSplitMinLeft, maxLeft)
	rightW := max(1, m.width-leftW-stackSplitDivider)

	// Left column: the windowed commit list, one compact row each.
	left := make([]string, avail)
	for i := range left {
		left[i] = strings.Repeat(" ", leftW)
	}
	if len(commits) > 0 {
		start, end := windowBounds(len(commits), cursor, avail)
		row := 0
		for i := start; i < end && row < avail; i++ {
			left[row] = m.stackListRowCompact(commits[i], i == cursor, leftW)
			row++
		}
	}

	// Right column: the cursored commit's cached patch, windowed by its offset.
	right := make([]string, avail)
	switch {
	case len(commits) == 0:
		right[0] = dimStyle.Render("(no commits ahead of main)")
	default:
		cd, have := m.stackView.diffCache[commits[clamp(cursor, 0, len(commits)-1)].diffKey()]
		switch {
		case !have || cd.loading:
			right[0] = dimStyle.Render("loading diff…")
		case cd.err != nil:
			right[0] = errStyle.Render("diff failed: " + cd.err.Error())
		case cd.scope.files == 0:
			// The uncommitted row can legitimately empty out under the cursor —
			// the work gets committed or reverted while the pane is open — and
			// "no changes in this commit" would read as nonsense there.
			if commits[clamp(cursor, 0, len(commits)-1)].uncommitted() {
				right[0] = dimStyle.Render("(nothing uncommitted)")
			} else {
				right[0] = dimStyle.Render("(no changes in this commit)")
			}
		default:
			start := clamp(cd.offset, 0, max(0, len(cd.scope.lines)-1))
			end := min(len(cd.scope.lines), start+avail)
			row := 0
			for i := start; i < end && row < avail; i++ {
				right[row] = ansi.Truncate(cd.scope.lines[i], rightW, "")
				row++
			}
		}
	}

	// Two keys, two concepts: d owns the preview (it opened it, it closes it),
	// esc leaves the stack screen entirely. Neither is a second name for the
	// other, which is what the old "d close · esc list" pairing implied.
	//
	// esc is deliberately not labelled "list": tab's hints already name the
	// panes, so an esc sitting beside them under a pane's name read as a third
	// way to move focus rather than as the way out.
	var footer string
	if focus == paneStack {
		footer = "  " + strings.Join([]string{
			keyhint("↑↓ / j k", "commit"), keyhint("tab", "diff"),
			keyhint("o", "PR"), keyhint("d", "close preview"),
			keyhint("esc", "return"),
		}, helpStyle.Render(" · "))
	} else {
		footer = "  " + strings.Join([]string{
			keyhint("↑↓ / j k", "scroll"), keyhint("] / [", "file"),
			keyhint("n / p", "commit"), keyhint("tab", "list"),
			keyhint("d", "close preview"), keyhint("esc", "return"),
		}, helpStyle.Render(" · "))
	}

	out := make([]string, 0, avail+3)
	out = append(out, ansi.Truncate(hdr, m.width, ""), rule)
	out = append(out, joinColumns(left, right, leftW)...)
	out = append(out, ansi.Truncate(footer, m.width, ""))
	return strings.Join(out, "\n")
}

// joinColumns zips the left and right split columns row-for-row, padding the
// left cell to leftW and separating the two with a dim ` │ ` divider. Missing
// rows on either side render as blanks so the two columns stay aligned.
func joinColumns(left, right []string, leftW int) []string {
	n := len(left)
	if len(right) > n {
		n = len(right)
	}
	out := make([]string, n)
	for i := 0; i < n; i++ {
		l, r := "", ""
		if i < len(left) {
			l = left[i]
		}
		if i < len(right) {
			r = right[i]
		}
		out[i] = padLine(l, leftW) + " " + dimStyle.Render("│") + " " + r
	}
	return out
}

// stackJustify lays left and right on one row innerW wide with the gap between
// them, falling back to left alone when the two won't fit.
func stackJustify(left, right string, innerW int) string {
	gap := innerW - lipgloss.Width(left) - lipgloss.Width(right)
	if gap < 1 {
		return left
	}
	return left + strings.Repeat(" ", gap) + right
}

// stackGlyphUncommitted marks the synthetic uncommitted row. It matches the
// marker the deck already uses for a dirty worktree, so the two surfaces agree
// on what "there is work here that isn't committed" looks like.
const stackGlyphUncommitted = "✷"

// uncommittedLabel is the subject text of the uncommitted row.
const uncommittedLabel = "uncommitted"

// uncommittedFacts spells out the uncommitted row's right-hand facts from the
// deck's already-computed diffstat. Add/Del cover tracked edits only — the
// diffstat git can count — so a worktree carrying nothing but new files reads
// "untracked only" rather than a bare "+0 −0".
func (m Model) uncommittedFacts() string {
	it, ok := m.activeByKey(m.stackView.key)
	if !ok {
		return uncommittedLabel
	}
	if it.view.Add+it.view.Del == 0 {
		return "untracked only"
	}
	return fmt.Sprintf("+%d −%d", it.view.Add, it.view.Del)
}

// stackListRow renders one row of the full-width stack list, dispatching between
// a commit and the synthetic uncommitted row.
func (m Model) stackListRow(r stackRow, selected bool, innerW int) string {
	if !r.uncommitted() {
		return m.stackCommitRow(*r.commit, selected, innerW)
	}
	factsP := m.uncommittedFacts()
	factsW := lipgloss.Width(factsP)
	name := ansi.Truncate(uncommittedLabel, max(4, innerW-2-factsW-2), "")
	gap := max(1, innerW-2-lipgloss.Width(name)-factsW)
	if selected {
		return selBar("▸ "+name+strings.Repeat(" ", gap)+factsP, innerW)
	}
	return stackWaitStyle.Render(stackGlyphUncommitted) + " " + nameStyle.Render(name) +
		strings.Repeat(" ", gap) + dimStyle.Render(factsP)
}

// stackListRowCompact is stackListRow for the split view's narrow left column.
func (m Model) stackListRowCompact(r stackRow, selected bool, width int) string {
	if !r.uncommitted() {
		return m.stackCommitRowCompact(*r.commit, selected, width)
	}
	subj := ansi.Truncate(uncommittedLabel, max(1, width-2), "")
	if selected {
		return selBar("▸ "+subj, width)
	}
	return padLine(stackWaitStyle.Render(stackGlyphUncommitted)+" "+nameStyle.Render(subj), width)
}

// stackCommitRow lays out one commit line: a state glyph (or the ▸ cursor
// marker), the subject given all the width its facts leave, and the PR/state
// facts flush right. The selected row is drawn as a full-width selection bar.
func (m Model) stackCommitRow(c stack.Commit, selected bool, innerW int) string {
	factsP := factsPlain(c)
	factsW := lipgloss.Width(factsP)
	name := ansi.Truncate(c.Subject, max(4, innerW-2-factsW-2), "")
	gap := max(1, innerW-2-lipgloss.Width(name)-factsW)
	if selected {
		return selBar("▸ "+name+strings.Repeat(" ", gap)+factsP, innerW)
	}
	return glyphFor(c) + " " + nameStyle.Render(name) + strings.Repeat(" ", gap) + facts(c)
}

// stackCommitRowCompact lays out one commit for the split view's narrow left
// column: a status glyph, a space, the truncated subject, and — when the commit
// carries a PR — a trailing dim "#<num>". The selected row is a full-width
// selection bar; the whole row is padded/truncated to width.
func (m Model) stackCommitRowCompact(c stack.Commit, selected bool, width int) string {
	num := ""
	if c.PR != nil {
		num = fmt.Sprintf("#%d", c.PR.Number)
	}
	numW := lipgloss.Width(num)
	subjW := max(1, width-2-numW-1)
	subj := ansi.Truncate(c.Subject, subjW, "")
	if selected {
		text := "▸ " + subj
		if num != "" {
			text += " " + num
		}
		return selBar(text, width)
	}
	row := glyphFor(c) + " " + nameStyle.Render(subj)
	if num != "" {
		gap := max(1, width-2-lipgloss.Width(subj)-numW)
		row += strings.Repeat(" ", gap) + dimStyle.Render(num)
	}
	return padLine(row, width)
}

const (
	stackGlyphReady     = "✓"
	stackGlyphPending   = "…"
	stackGlyphAttention = "!"
	stackGlyphRestack   = "↻"
	stackGlyphInactive  = "○"
)

// glyphFor returns one overall status marker for a commit. Priority is human
// attention, restack, pending, healthy, then inactive; the facts column spells
// out the underlying state so selected rows remain understandable without it.
func glyphFor(c stack.Commit) string {
	if c.PR != nil && (c.PR.Mergeable == "CONFLICTING" || c.PR.Checks.Summary == "failing") {
		return errStyle.Render(stackGlyphAttention)
	}
	switch c.State {
	case stack.StateClosed, stack.StateDuplicateID:
		return errStyle.Render(stackGlyphAttention)
	case stack.StateDiverged:
		return stackWaitStyle.Render(stackGlyphRestack)
	case stack.StateMerged:
		return aheadStyle.Render(stackGlyphReady)
	case stack.StateOpen:
		if c.PR != nil && c.PR.Draft {
			return dimStyle.Render(stackGlyphInactive)
		}
		if c.PR != nil && c.PR.Checks.Summary == "pending" {
			return stackWaitStyle.Render(stackGlyphPending)
		}
		if c.PR != nil && c.PR.Checks.Summary == "passing" {
			return aheadStyle.Render(stackGlyphReady)
		}
		return dimStyle.Render(stackGlyphInactive)
	case stack.StateUnsubmitted, stack.StateUnpushed, stack.StateMissingPR:
		return dimStyle.Render(stackGlyphInactive)
	default:
		return dimStyle.Render(stackGlyphInactive)
	}
}

// facts renders a commit's PR/state facts for a list row, with check state
// written as a word; factsPlain is the unstyled variant for the selection bar.
func facts(c stack.Commit) string {
	if c.PR == nil {
		return dimStyle.Render(factsPlain(c))
	}
	base := fmt.Sprintf("PR #%d %s", c.PR.Number, prWord(c))
	if c.PR.Mergeable == "CONFLICTING" {
		return errStyle.Render(fmt.Sprintf("PR #%d · conflicts", c.PR.Number))
	}
	switch c.State {
	case stack.StateMerged:
		return dimStyle.Render(base + " · landed")
	case stack.StateClosed:
		return errStyle.Render(base)
	case stack.StateDiverged:
		return dimStyle.Render(base+" · ") + stackWaitStyle.Render("restack needed")
	default:
		if s := c.PR.Checks.Summary; s != "" && s != "none" {
			return dimStyle.Render(base+" · checks ") + coloredCheckWord(s)
		}
		return dimStyle.Render(base)
	}
}

// factsPlain is the unstyled fact string, used for the selection bar (which
// styles the whole row) and as the source facts() colors.
func factsPlain(c stack.Commit) string {
	if c.PR == nil {
		switch c.State {
		case stack.StateUnsubmitted:
			return "unsubmitted"
		case stack.StateUnpushed:
			return "unpushed"
		case stack.StateMissingPR:
			return "pushed · no PR"
		default:
			return string(c.State)
		}
	}
	base := fmt.Sprintf("PR #%d %s", c.PR.Number, prWord(c))
	if c.PR.Mergeable == "CONFLICTING" {
		return fmt.Sprintf("PR #%d · conflicts", c.PR.Number)
	}
	switch c.State {
	case stack.StateMerged:
		return base + " · landed"
	case stack.StateClosed:
		return base
	case stack.StateDiverged:
		return base + " · restack needed"
	default:
		if s := c.PR.Checks.Summary; s != "" && s != "none" {
			return base + " · checks " + s
		}
		return base
	}
}

// prWord names a PR's state for the fact string: its lifecycle for a
// merged/closed/diverged commit, else draft vs open.
func prWord(c stack.Commit) string {
	switch c.State {
	case stack.StateMerged:
		return "merged"
	case stack.StateClosed:
		return "closed"
	case stack.StateDiverged:
		return "diverged"
	default:
		if c.PR != nil && c.PR.Draft {
			return "draft"
		}
		return "open"
	}
}

// coloredCheckWord reinforces the leading overall glyph without introducing a
// second, potentially contradictory symbol in the facts column.
func coloredCheckWord(summary string) string {
	switch summary {
	case "passing":
		return aheadStyle.Render(summary)
	case "pending":
		return stackWaitStyle.Render(summary)
	case "failing":
		return errStyle.Render(summary)
	default:
		return dimStyle.Render(summary)
	}
}

// okWord renders a boolean base-chain health as its summary word.
func okWord(ok bool) string {
	if ok {
		return "ok"
	}
	return "broken"
}

// --- deck glyph + detail derivation ---

// stackGlyph returns the deck row's stack glyph for a worktree (styled string and
// its display width), or width 0 when nothing should show — no cache entry, a
// fetch in flight, a fetch error, or a rollup the derivation leaves blank.
func (m Model) stackGlyph(key string) (string, int) {
	e, ok := m.stackInfo[key]
	if !ok || e.loading || e.err != nil {
		return "", 0
	}
	g, style, show := deckStackGlyph(e.status)
	if !show {
		return "", 0
	}
	return style.Render(g), lipgloss.Width(g)
}

// deckStackGlyph maps a cached rollup to the deck's single stack glyph. It returns
// show=false (nothing to draw) when GitHub is unavailable, the stack is empty or
// entirely unsubmitted, or the state matches no glyph. Escalations (closed PR,
// duplicate id, orphan, broken base chain) win as a red "!"; a needed restack is a
// red "↻"; then the CI ladder — a yellow "…" for any pending check, a green "✓"
// when every commit is open with checks passing.
func deckStackGlyph(st stack.StackStatus) (string, lipgloss.Style, bool) {
	if !st.GitHub.Available {
		return "", lipgloss.Style{}, false
	}
	stk := st.Stack
	if stk.Size == 0 || stk.Counts[stack.StateUnsubmitted] == stk.Size {
		return "", lipgloss.Style{}, false
	}
	if stk.Counts[stack.StateClosed] > 0 || stk.Counts[stack.StateDuplicateID] > 0 ||
		len(stk.Orphans) > 0 || !stk.BaseChainOK || stk.NextAction == "escalate" ||
		stk.NextAction == "resolve-conflicts" {
		return "!", errStyle, true
	}
	if stk.NextAction == "restack" {
		return stackGlyphRestack, errStyle, true
	}
	if stk.NextAction == "complete" {
		return "✓", aheadStyle, true
	}

	anyPending, allOpen, allPassing := false, true, true
	for _, c := range st.Commits {
		if c.State != stack.StateOpen {
			allOpen = false
		}
		if c.PR != nil {
			switch c.PR.Checks.Summary {
			case "pending":
				anyPending = true
			case "failing":
				allPassing = false
			}
		}
	}
	if anyPending {
		return stackGlyphPending, stackWaitStyle, true
	}
	if allOpen && allPassing {
		return stackGlyphReady, aheadStyle, true
	}
	return "", lipgloss.Style{}, false
}

// stackDetailSeg returns the selected worktree's stack segment for the expanded
// detail line, or "" when there is nothing to add.
func (m Model) stackDetailSeg(key string) string {
	e, ok := m.stackInfo[key]
	if !ok || e.loading || e.err != nil {
		return ""
	}
	return stackDetailSegment(e.status)
}

// stackDetailSegment spells out the useful stack outcome for the detail line.
// A single-commit stack names its PR; larger stacks omit the redundant size and
// translate the workflow's machine-facing next action into a short human state.
// It returns "" when GitHub is unavailable or the stack is empty/unsubmitted, so
// the deck stays byte-identical without data.
func stackDetailSegment(st stack.StackStatus) string {
	if !st.GitHub.Available {
		return ""
	}
	stk := st.Stack
	if stk.Size == 0 || stk.Counts[stack.StateUnsubmitted] == stk.Size {
		return ""
	}
	if stk.NextAction == "complete" {
		return "stack complete"
	}
	if stk.Size == 1 && len(st.Commits) == 1 {
		c := st.Commits[0]
		if c.PR != nil {
			return fmt.Sprintf("PR #%d %s · checks %s", c.PR.Number, c.State, checksLabel(c.PR.Checks.Summary))
		}
		return stackActionLabel(stk.NextAction)
	}
	var seg string
	if merged := stk.Counts[stack.StateMerged]; merged > 0 {
		seg = fmt.Sprintf("%d merged", merged)
	}
	action := stackActionLabel(stk.NextAction)
	if seg != "" && action != "" {
		return seg + " · " + action
	}
	return seg + action
}

func stackActionLabel(action string) string {
	switch action {
	case "merge":
		return "ready to merge"
	case "wait":
		return "waiting on checks"
	case "restack":
		return "restack needed"
	case "complete":
		return "stack complete"
	case "resolve-conflicts":
		return "resolve conflicts"
	case "submit":
		return "submit needed"
	case "escalate":
		return "needs attention"
	default:
		return action
	}
}

func checksLabel(summary string) string {
	switch summary {
	case "passing":
		return "passing"
	case "pending":
		return "pending"
	case "failing":
		return "failing"
	default:
		return "unknown"
	}
}

// --- palette helpers ---

// stackHasSubmitWork reports whether a cached rollup carries submit-able commits
// (unsubmitted, unpushed, diverged, or missing-pr) — the gate for the palette's
// "submit stack" row.
func stackHasSubmitWork(st stack.StackStatus) bool {
	c := st.Stack.Counts
	return c[stack.StateUnsubmitted]+c[stack.StateUnpushed]+c[stack.StateDiverged]+c[stack.StateMissingPR] > 0
}

// stackCanRestack reports when the restack pipeline has a landed prefix to drop
// and rebase away. A conflict is restackable only in that cascade shape; an
// ordinary conflicting PR has no landed prefix, so restack would be a no-op.
func stackCanRestack(st stack.StackStatus) bool {
	if st.Stack.NextAction == "restack" || st.Stack.NextAction == "complete" {
		return true
	}
	return st.Stack.NextAction == "resolve-conflicts" &&
		st.Stack.Counts[stack.StateMerged] > 0
}

func stackCanMerge(st stack.StackStatus) bool {
	if st.Stack.NextAction != "merge" || st.MergeHint == nil {
		return false
	}
	for _, c := range st.Commits {
		if c.State == stack.StateOpen && c.PR != nil {
			return c.PR.Number == st.MergeHint.Number && c.PR.Base == st.MainBranch &&
				c.PR.Mergeable == "MERGEABLE"
		}
	}
	return false
}

// stackRestackReason is the palette hint on the "restack" row: how many landed
// commits sit below the survivors.
func stackRestackReason(st stack.StackStatus) string {
	if st.Stack.NextAction == "complete" {
		return "keep worktree"
	}
	if n := st.Stack.Counts[stack.StateMerged]; n > 0 {
		return fmt.Sprintf("%d landed below", n)
	}
	return "restack needed"
}
