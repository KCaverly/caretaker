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
	if msg.err != nil {
		m.stackView.status = nil
		m.stackView.body = stackErrorBody("status failed: "+msg.err.Error(), nil)
		return
	}
	st := msg.status
	m.stackView.status = &st
	m.stackView.body = nil
	m.stackView.cursor = clampCursor(m.stackView.cursor, len(st.Commits))
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
	m.stackView.cursor = clampCursor(m.stackView.cursor, len(st.Commits))
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
		m.stackView.cursor = clampCursor(m.stackView.cursor, len(st.Commits))
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
		m.stackView.cursor = clampCursor(m.stackView.cursor, len(st.Commits))
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
	m.stackView.cursor = clampCursor(m.stackView.cursor, len(st.Commits))
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
// esc/q close and r re-fetches in every state. The rest split on the render path:
// with a structured status the cursor moves (j/k), submit (s), restack (R), diff
// (v), and open PR (o) act on the stack, and g/G jump the cursor; enter is inert
// here and only confirms a pending restack; with a text
// body (an error or the restack dry-run plan) j/k scroll it and — in the
// restack-confirm state — enter runs the restack for real.
func (m Model) handleStack(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	sv := m.stackView
	avail := m.stackViewport()
	maxOff := max(0, len(sv.body)-avail)
	n := 0
	if sv.status != nil {
		n = len(sv.status.Commits)
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
			m.stackView.working = true
			return m, m.mergeStackCmd(sv.key, sv.params)
		}
	case "v":
		// Jump to the deck's read-only diff of everything the branch carries.
		if sv.status != nil {
			if it, ok := m.activeByKey(sv.key); ok {
				m.stackOpen = false
				m.stackView = stackView{}
				m.screen = screenPicker
				m.focus = focusActive
				for i, a := range m.active {
					if wsKey(a.repo.Name, a.view.WT.Name) == sv.key {
						m.activeCursor = i
						break
					}
				}
				return m.openDiff(it)
			}
		}
	case "o":
		// Open the selected commit's PR in the browser.
		if sv.status != nil && n > 0 {
			c := sv.status.Commits[clamp(sv.cursor, 0, n-1)]
			if c.PR != nil && c.PR.URL != "" {
				return m, openURLCmd(c.PR.URL)
			}
			return m, m.flashCmd("no PR for this commit")
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
	closeFooter := keyhint("j/k", "scroll") + "   " + keyhint("r", "refresh") + "   " + keyhint("esc", "close")
	switch {
	case sv.working:
		return m.renderStackText([]string{dimStyle.Render("working…")}, closeFooter, h)
	case sv.confirmRestack:
		return m.renderStackText(sv.body, confirmFooter, h)
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
	innerW := clamp(m.width-8, 40, 84)

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

	boxStr := box(rows, innerW, len(rows), true)
	return centerBlock(boxStr, m.width, h)
}

// renderStackStatus draws the structured stack screen: a titled header, a chain
// row from main through each commit's subject, a cursored (and, for a tall stack,
// scrollable) commit list with per-commit PR facts, a one-line rollup summary,
// and an action footer whose submit/restack hints appear only when the rollup
// calls for them. It shares renderStack's centered box container.
func (m Model) renderStackStatus(st stack.StackStatus, cursor, h int) string {
	innerW := clamp(m.width-8, 40, 84)
	trunc := func(s string) string { return ansi.Truncate(s, innerW, "") }

	var rows []string

	// Title: "STACK  <repo> / <wt>" left, "base: <main>" right-justified.
	left := headerStyle.Render("STACK") + "  " + repoHdrStyle.Render(st.Repo) +
		dimStyle.Render(" / ") + nameStyle.Render(st.Worktree)
	right := dimStyle.Render("base: " + st.MainBranch)
	rows = append(rows, trunc(stackJustify(left, right, innerW)), "")

	// Commit list, windowed to keep the cursor visible. Each row gives the whole
	// subject the width its facts don't need, with the facts flush right.
	if len(st.Commits) == 0 {
		rows = append(rows, trunc(dimStyle.Render("(no commits ahead of main)")))
	} else {
		chrome := 6
		budget := clamp(m.height-barHeight-chrome, 1, stackMaxBodyRows)
		listH := min(len(st.Commits), budget)
		start, end := windowBounds(len(st.Commits), cursor, listH)
		for i := start; i < end; i++ {
			rows = append(rows, m.stackCommitRow(st.Commits[i], i == cursor, innerW))
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
	parts := []string{keyhint("j/k", "move")}
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
	parts = append(parts, keyhint("v", "diff"), keyhint("o", "open PR"),
		keyhint("esc", "deck"), keyhint("r", "refresh"))
	rows = append(rows, trunc("  "+strings.Join(parts, "   ")))

	boxStr := box(rows, innerW, len(rows), true)
	return centerBlock(boxStr, m.width, h)
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

// glyphFor returns the styled single-glyph state marker for a commit list row: a
// green ✓ for a landed/open PR (a dim ○ when that PR is a draft), a dim ○ for
// work not yet in review, a yellow … for a diverged commit, and a red ✗ for a
// conflicting PR, closed PR, or duplicate-id escalation.
func glyphFor(c stack.Commit) string {
	if c.PR != nil && c.PR.Mergeable == "CONFLICTING" {
		return errStyle.Render("✗")
	}
	switch c.State {
	case stack.StateMerged, stack.StateOpen:
		if c.PR != nil && c.PR.Draft {
			return dimStyle.Render("○")
		}
		return aheadStyle.Render("✓")
	case stack.StateUnsubmitted, stack.StateUnpushed, stack.StateMissingPR:
		return dimStyle.Render("○")
	case stack.StateDiverged:
		return stackWaitStyle.Render("…")
	case stack.StateClosed, stack.StateDuplicateID:
		return errStyle.Render("✗")
	default:
		return dimStyle.Render("○")
	}
}

// facts renders a commit's PR/state facts for a list row, dim with a colored
// check mark; factsPlain is the unstyled variant for the selection bar.
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
	default:
		if s := c.PR.Checks.Summary; s != "" && s != "none" {
			return dimStyle.Render(base+" · checks ") + coloredCheckMark(s)
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
	default:
		if s := c.PR.Checks.Summary; s != "" && s != "none" {
			return base + " · checks " + checksMark(s)
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

// coloredCheckMark renders a checks summary as a colored glyph: green ✓ passing,
// yellow … pending, red ✗ failing, reusing checksMark for the glyph itself.
func coloredCheckMark(summary string) string {
	switch summary {
	case "passing":
		return aheadStyle.Render(checksMark(summary))
	case "pending":
		return stackWaitStyle.Render(checksMark(summary))
	case "failing":
		return errStyle.Render(checksMark(summary))
	default:
		return dimStyle.Render(checksMark(summary))
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
		len(stk.Orphans) > 0 || !stk.BaseChainOK || stk.NextAction == "escalate" ||
		stk.NextAction == "resolve-conflicts" {
		return "!", errStyle, true
	}
	if stk.NextAction == "restack" {
		return "⟳", errStyle, true
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
			return fmt.Sprintf("PR #%d %s · checks %s", c.PR.Number, c.State, checksMark(c.PR.Checks.Summary))
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
