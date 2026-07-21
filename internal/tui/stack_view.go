package tui

import (
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

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

// stackView backs the read-only stack overlay. It stamps the target worktree and
// the params a re-fetch/submit/restack reuses, holds the pre-split body lines and
// scroll offset, and tracks two transient flags: working (a pipeline call is in
// flight, so the body shows a one-line "working…") and confirmRestack (a restack
// dry-run plan is on screen awaiting enter, which runs it for real).
type stackView struct {
	repoName, wtName, key string
	params                stack.Params

	body   []string
	offset int

	working        bool
	confirmRestack bool
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
	err    error
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

func (m Model) restackStackCmd(key string, p stack.Params, dryRun bool) tea.Cmd {
	restack := m.stackRestack
	return func() tea.Msg {
		res, err := restack(stack.RestackOptions{Params: p, DryRun: dryRun})
		return stackRestackMsg{key: key, res: res, dryRun: dryRun, err: err}
	}
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
	m.stackView.offset = 0
	if msg.err != nil {
		m.stackView.body = stackErrorBody("status failed: "+msg.err.Error(), nil)
		return
	}
	m.stackView.body = renderStackBody(stack.Render(msg.status))
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
	m.stackView.offset = 0
	if msg.err != nil {
		m.stackView.body = stackErrorBody("submit failed: "+msg.err.Error(), msg.res.Executed)
		return
	}
	m.stackView.body = renderStackBody(stack.Render(msg.res.Status))
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
	m.stackView.offset = 0
	switch {
	case msg.err != nil:
		m.stackView.body = stackErrorBody("restack failed: "+msg.err.Error(), msg.res.Executed)
	case msg.res.Nothing:
		m.stackView.body = renderStackBody(stack.Render(msg.res.Status))
	case msg.dryRun:
		m.stackView.body = renderStackBody(stack.RenderRestackPlan(msg.res))
		m.stackView.confirmRestack = true
	default:
		m.stackView.body = renderStackBody(stack.Render(msg.res.Status))
	}
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

// handleStack routes keys while the stack overlay is open, mirroring the usage
// overlay's modal behavior: esc/q close, r re-fetches, j/k scroll (clamped, so
// they no-op when the body fits), and — in the restack-confirm state — enter runs
// the restack for real. Everything else is swallowed so no key leaks beneath.
func (m Model) handleStack(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	sv := m.stackView
	avail := m.stackViewport()
	maxOff := max(0, len(sv.body)-avail)
	switch msg.String() {
	case "esc", "q":
		// In the restack-confirm state esc cancels without executing; either way
		// the overlay closes.
		m.stackOpen = false
		m.stackView = stackView{}
	case "enter":
		if sv.confirmRestack && !sv.working {
			m.stackView.working = true
			m.stackView.confirmRestack = false
			return m, m.restackStackCmd(sv.key, sv.params, false)
		}
	case "r":
		if !sv.working {
			m.stackView.working = true
			m.stackView.confirmRestack = false
			m.markStackLoading(sv.key)
			return m, m.fetchStackCmd(sv.key, sv.params)
		}
	case "j", "down":
		m.stackView.offset = clamp(sv.offset+1, 0, maxOff)
	case "k", "up":
		m.stackView.offset = clamp(sv.offset-1, 0, maxOff)
	case "g":
		m.stackView.offset = 0
	case "G":
		m.stackView.offset = maxOff
	}
	return m, nil
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

// renderStack draws the read-only stack overlay: a centered box titled
// "STACK <repo> / <wt>" over the windowed Render/plan body, a one-line "working…"
// while a pipeline call is in flight, and a footer that switches to
// "enter run · esc cancel" while a restack plan awaits confirmation.
func (m Model) renderStack(h int) string {
	sv := m.stackView
	innerW := clamp(m.width-8, 40, 84)

	rows := []string{header("stack "+sv.repoName+" / "+sv.wtName, -1), ""}

	body := sv.body
	switch {
	case sv.working:
		body = []string{dimStyle.Render("working…")}
	case len(body) == 0:
		body = []string{dimStyle.Render("(no stack data)")}
	}

	avail := m.stackViewport()
	start := clamp(sv.offset, 0, max(0, len(body)-1))
	end := min(len(body), start+avail)
	for i := start; i < end; i++ {
		rows = append(rows, "  "+ansi.Truncate(body[i], innerW-2, ""))
	}
	for i := end - start; i < avail; i++ {
		rows = append(rows, "")
	}

	rows = append(rows, "")
	if sv.confirmRestack && !sv.working {
		rows = append(rows, "  "+keyhint("enter", "run")+"   "+keyhint("esc", "cancel"))
	} else {
		rows = append(rows, "  "+keyhint("j/k", "scroll")+"   "+keyhint("r", "refresh")+"   "+keyhint("esc", "close"))
	}

	boxStr := box(rows, innerW, len(rows), true)
	return centerBlock(boxStr, m.width, h)
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
// red "⟳"; then the CI ladder — a yellow "…" for any pending check, a green "✓"
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
		len(stk.Orphans) > 0 || !stk.BaseChainOK || stk.NextAction == "escalate" {
		return "!", errStyle, true
	}
	if stk.NextAction == "restack" {
		return "⟳", errStyle, true
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
		return "…", stackWaitStyle, true
	}
	if allOpen && allPassing {
		return "✓", aheadStyle, true
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

// stackDetailSegment spells out the stack facts for the detail line: a
// single-commit stack reads "PR #42 open · checks ✓" (or its next action when it
// has no PR yet); a multi-commit stack reads "stack 3 · 1 merged · next: restack".
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
	if stk.Size == 1 && len(st.Commits) == 1 {
		c := st.Commits[0]
		if c.PR != nil {
			return fmt.Sprintf("PR #%d %s · checks %s", c.PR.Number, c.State, checksMark(c.PR.Checks.Summary))
		}
		return "stack 1 · next: " + stk.NextAction
	}
	seg := fmt.Sprintf("stack %d", stk.Size)
	if merged := stk.Counts[stack.StateMerged]; merged > 0 {
		seg += fmt.Sprintf(" · %d merged", merged)
	}
	seg += " · next: " + stk.NextAction
	return seg
}

// checksMark renders a check summary as a compact glyph for the detail line.
func checksMark(summary string) string {
	switch summary {
	case "passing":
		return "✓"
	case "pending":
		return "…"
	case "failing":
		return "✗"
	default:
		return "—"
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

// stackRestackReason is the palette hint on the "restack" row: how many landed
// commits sit below the survivors.
func stackRestackReason(st stack.StackStatus) string {
	if n := st.Stack.Counts[stack.StateMerged]; n > 0 {
		return fmt.Sprintf("%d landed below", n)
	}
	return "restack needed"
}
