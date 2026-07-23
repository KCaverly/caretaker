// Package tui contains the Bubble Tea deck that powers the ct command.
package tui

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"charm.land/bubbles/v2/textarea"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/sahilm/fuzzy"

	"github.com/KCaverly/caretaker/internal/agent"
	"github.com/KCaverly/caretaker/internal/config"
	"github.com/KCaverly/caretaker/internal/plasma"
	"github.com/KCaverly/caretaker/internal/repo"
	"github.com/KCaverly/caretaker/internal/session"
	"github.com/KCaverly/caretaker/internal/stack"
	"github.com/KCaverly/caretaker/internal/state"
	"github.com/KCaverly/caretaker/internal/usage"
)

// barHeight is the number of rows the top chrome occupies: the status bar and
// a light separator line directly beneath it.
const barHeight = 2

// screen is the active view: the picker, one of the session views, or setup.
type screen int

const (
	screenPicker screen = iota
	screenEditor
	screenAgent
	screenTerminal
	screenSetup
)

// next cycles among the session views (editor → agent → terminal → editor).
func (s screen) next() screen {
	switch s {
	case screenEditor:
		return screenAgent
	case screenAgent:
		return screenTerminal
	default:
		return screenEditor
	}
}

// prev cycles among the session views in reverse (editor → terminal → agent →
// editor), mirroring next.
func (s screen) prev() screen {
	switch s {
	case screenTerminal:
		return screenAgent
	case screenAgent:
		return screenEditor
	default:
		return screenTerminal
	}
}

// workspaceRef is the currently-activated workspace and its live sessions.
type workspaceRef struct {
	repo, worktree, key, path string
	ws                        *session.Workspace
}

type focus int

const (
	focusNew focus = iota
	focusActive
)

type mode int

const (
	modeNormal mode = iota
	modeCreateName
	modeConfirmRemove
	modeConfirmArchive
	modeConfirmQuit // guarding ctrl+c while a hosted agent is busy
	modeConfirmStop // guarding "d" while the target worktree has a busy agent
	modeConfirmAgentClose
	modeConfirmAgentRestart
	modeConfirmMerge
	modeConfirmTermClose
)

// confirmOption is one selectable row of a destructive-confirm panel: a
// human-readable label, the mnemonic key that fires it directly (so the old
// muscle memory — `x` then `y`/`b` — keeps working), and whether it performs a
// destructive action and should render red.
type confirmOption struct {
	label  string // "remove worktree, keep branch"
	key    string // mnemonic that fires it directly: "b", "y", "v", "esc", "n"
	danger bool   // render red (both label and mnemonic)
}

// confirmState carries everything the centered confirm panel draws for the
// three destructive prompts. The active picker mode (modeConfirmRemove/Quit/
// Stop) still decides which confirm is live and which handler resolves the
// keys; this only holds the panel's content, so it replaces the old
// status-line sentence entirely. cursor starts on the SAFE option.
type confirmState struct {
	title       string            // "REMOVE WORKTREE", "QUIT CT", "STOP WORKSPACE"
	context     []string          // pre-styled context lines drawn above the options
	options     []confirmOption   // vertical, arrow-selectable
	cursor      int               // index into options; starts on the safe row
	agent       *boardRow         // captured board target; immune to live status reordering
	target      *activeItem       // captured worktree target; immune to deck reordering
	fingerprint string            // exact porcelain status disclosed before archive
	merge       *stackMergeTarget // captured stack target; preserves its originating overlay
	term        *termCloseTarget  // captured terminal target; immune to hidden-surface focus changes
}

type stackMergeTarget struct {
	key    string
	params stack.Params
}

type termCloseTarget struct {
	key      string
	index    int
	worktree string
}

// confirmActive reports whether one of the three destructive-confirm panels is
// up, so the view can layer it over the deck and the key router can keep it
// modal.
func (m Model) confirmActive() bool {
	return m.mode == modeConfirmRemove || m.mode == modeConfirmArchive || m.mode == modeConfirmQuit || m.mode == modeConfirmStop ||
		m.mode == modeConfirmAgentClose || m.mode == modeConfirmAgentRestart || m.mode == modeConfirmMerge ||
		m.mode == modeConfirmTermClose
}

// confirmIsNav reports whether key moves the confirm panel's cursor rather than
// resolving it. Unrecognized keys are contained by every confirmation handler.
func confirmIsNav(key string) bool {
	switch key {
	case "up", "down", "k", "j", "ctrl+p", "ctrl+n":
		return true
	}
	return false
}

// confirmMove returns the cursor position after applying a navigation key,
// clamped to the option list. Shared by all three confirm handlers.
func (m Model) confirmMove(key string) int {
	cur := m.confirm.cursor
	switch key {
	case "up", "k", "ctrl+p":
		if cur > 0 {
			cur--
		}
	case "down", "j", "ctrl+n":
		if cur < len(m.confirm.options)-1 {
			cur++
		}
	}
	return cur
}

// confirmSelectedKey maps an "enter" press to the mnemonic of the option under
// the cursor, so the arrow-selectable rows resolve through the exact same key
// paths as the direct mnemonics.
func (m Model) confirmSelectedKey() string {
	if m.confirm.cursor < 0 || m.confirm.cursor >= len(m.confirm.options) {
		return ""
	}
	return m.confirm.options[m.confirm.cursor].key
}

// attnLevel ranks an agent's attention state, highest first when sorting.
// attnWaiting is derived live from the polled agent status and never stored;
// attnDone is an unread marker recorded when a transition is observed and
// cleared when the user views the workspace's agents.
type attnLevel int

const (
	attnNone    attnLevel = iota // nothing pending
	attnDone                     // busy → idle while unviewed (✓)
	attnWaiting                  // live status "waiting" — needs input/permission (!)
)

// attnEntry is one stored unread marker (attnDone) for an agent, keyed by pid
// in Model.attention.
type attnEntry struct {
	level     attnLevel
	key       string // workspace key the agent belongs to
	startedAt int64  // unix ms of the owning process's start, from AgentStatus.StartedAt; 0 = unknown
}

// boardRow is one row of the agent board: a non-navigable worktree group
// header, a selectable agent row, or the trailing "+ new agent" row.
type boardRow struct {
	isAgent bool // navigable agent row
	isNew   bool // navigable "+ new agent" row

	key      string // workspace key ("repo/branch")
	repo     string // for activate()
	worktree string
	path     string

	agentIdx int // index in ws.Agents
	pid      int
	label    string // display name
	provider agent.Provider
	status   string    // right-hand status column
	attn     attnLevel // includes derived waiting
	num      int       // 1-based quick-jump number (first 9 agent rows)
}

// Focus order of the new-agent form's fields.
const (
	formFieldPrompt = iota
	formFieldProvider
	formFieldWhere
	formFieldCount
)

// activeItem is a worktree shown in the "active" section, with its owning repo.
type activeItem struct {
	repo repo.Repo
	view WorktreeView
}

// scopeContent is one diff scope's fully-rendered payload: the pre-styled lines
// (built once at load, not per frame), the indices of the file-header lines
// within them (for the J/K jumps), and the summary counts the header shows for
// this scope (file count and +/− line totals).
type scopeContent struct {
	lines     []string
	fileLines []int
	files     int
	add, del  int
}

// diffState backs the read-only diff viewer. It holds the target worktree, the
// async-fetch state, and both scope renderings — the "full" scope (default)
// which shows the vs-base section followed by the uncommitted section, and the
// "uncommitted" scope which narrows to just the latter. Both are built up front
// on diffMsg arrival so the `u` toggle is instant. The active scope is selected
// by scopeUncommitted; offset scrolls it (reset to 0 on toggle). ahead/base feed
// the header summary and the vs-base section rule.
type diffState struct {
	repoName, wtName, key string
	base                  string // primary-worktree branch to diff against ("" when none)
	ahead                 int    // commits the branch carries beyond base

	loading          bool
	err              error
	scopeUncommitted bool // false = full view (the default)

	full        scopeContent
	uncommitted scopeContent
	offset      int
}

// active returns the scope currently on screen.
func (d diffState) active() scopeContent {
	if d.scopeUncommitted {
		return d.uncommitted
	}
	return d.full
}

// Model is the ct UI: a pinned status bar plus the active screen (picker or an
// embedded nvim/claude/terminal session).
type Model struct {
	ctrl  *Controller
	mgr   *session.Manager
	state *state.State

	// keys are the reserved keystrokes (not forwarded to embedded sessions),
	// fully defaulted by config.Load before they reach here.
	keys     config.Keys
	iconMode string

	screen     screen
	current    *workspaceRef
	helpOpen   bool
	helpOffset int

	// hintSeen records that the user has typed into a session at least once,
	// which retires the one-line "f1 help" hint shown beneath session bodies.
	// Reserved as a row until then (see sessionSize / sessionFooterH).
	hintSeen bool

	groups []Group

	focus focus
	mode  mode

	// confirm backs the centered panel drawn for the destructive-confirm modes.
	// Populated on entry (beginRemove, the quit guard, the stop guard) and
	// cleared when the prompt resolves; the zero value while no confirm is up.
	confirm confirmState

	// "new" section
	filter      textinput.Model
	repoMatches []repo.Repo
	newCursor   int

	// create flow
	nameInput   textinput.Model
	pendingRepo repo.Repo

	// "active" section
	active       []activeItem
	activeCursor int
	recentRank   map[string]int // worktree key -> recency rank (1..3); absent = none

	// first-run setup
	configPath string

	// last screen visited per worktree key, so switching back lands where you left
	lastScreens map[string]screen

	// agent board overlay (unified agent switcher + notifications)
	boardOpen   bool
	boardCursor int // index into the board's navigable rows
	rootInput   textinput.Model

	// command palette overlay: a fuzzy-searchable list of every ct action, each
	// row showing its live keybinding, built fresh on each keystroke. The cursor
	// indexes the filtered list; the input holds the fuzzy query.
	paletteOpen   bool
	paletteCursor int
	paletteInput  textinput.Model

	// read-only diff viewer overlay: a full-body scrollable view of everything
	// the selected worktree's branch carries beyond main — committed and
	// uncommitted. diffView holds its target, fetch state, and pre-rendered
	// content; it is the zero value while closed. Opens over the picker only.
	diffOpen bool
	diffView diffState

	// Stacked-PR surfacing. stackInfo caches each active worktree's `ct stack
	// status` (keyed by wsKey) so the deck can draw a passive glyph/detail without
	// ever running a subprocess on the render path; the stackFetch/Submit/Restack
	// funcs are the pipeline entry points (= stack.Status/Submit/Restack), injected
	// so tests stub them. stackView backs the read-only overlay; it is the zero
	// value while closed.
	stackInfo      map[string]stackEntry
	stackFetch     func(stack.Params) (stack.StackStatus, error)
	stackSubmit    func(stack.SubmitOptions) (stack.SubmitResult, error)
	stackRestack   func(stack.RestackOptions) (stack.RestackResult, error)
	stackReuse     func(stack.ReuseOptions) (stack.ReuseResult, error)
	stackArchive   func(stack.Params) (stack.ArchiveResult, error)
	stackMerge     func(stack.MergeOptions) (stack.MergeResult, error)
	stackAutoMerge bool
	stackOpen      bool
	stackView      stackView
	termPaneBusy   func(*session.Session) bool

	// Codex exposes its stable conversation history in a transcript overlay.
	// Caretaker tracks its default Ctrl+T toggle per hosted session so the wheel
	// can drive that overlay without switching Codex into jittery raw mode.
	codexTranscript map[*session.Session]bool

	// live agent statuses from `claude agents --json`, keyed by pid
	agentStatus     map[int]AgentStatus
	agentPrevStatus map[int]string    // pid → status from previous poll, for transition detection
	busySince       map[int]time.Time // pid → when its current busy stretch began
	pollFails       int               // consecutive status-poll failures, for the one-shot notice

	// stored unread markers (done) keyed by agent pid; waiting badges are
	// derived live from agentStatus and never stored here
	attention map[int]attnEntry

	// removed tombstones pids of agents ct has deleted, mapping each to the
	// StartedAt it carried at removal (0 if never polled). The Claude status
	// poll is a replace-all snapshot, and `claude agents --json` keeps listing a
	// killed agent for one or more polls when it was blocked on a permission
	// prompt — long enough to resurrect the live "waiting" status (and every
	// badge derived from it) that clearAgentTracking just cleared. Suppressing a
	// tombstoned pid in the poll keeps a deleted agent gone; each tombstone is
	// dropped once the poll stops corroborating it, so a recycled pid is never
	// held down.
	removed map[int]int64

	// Plan usage-limit gauges: one snapshot and session-window sample ring per
	// provider. The gauges degrade silently — failures only leave that
	// provider's snapshot to go stale, which hides its bar segment on its own.
	usageSnap      usage.Snapshot
	usageHave      bool
	codexUsageSnap usage.Snapshot
	codexUsageHave bool
	usageOpen      bool // usage overlay visibility (agent screen only)
	usageHist      []usageSample
	codexUsageHist []usageSample
	usageThreshold int // percent at/above which the bar segment shows

	// prompt input for the board's new-agent form. Unlike the other compact
	// inputs, this grows with a multi-line task description.
	promptInput textarea.Model

	// home workspace path/key, cached on first open for pathToKey lookups
	homeWSPath string
	homeWSKey  string

	// new-agent form (sub-state of the agent board)
	formOpen     bool
	formFocus    int // formFieldPrompt..formFieldWhere
	formProvider agent.Provider
	formLocation int // 0 = active worktree, 1 = home worktree

	status        string
	statusLevel   statusLevel // errors stick; info auto-expires
	statusAt      time.Time   // when a transient status was set (for auto-expiry)
	width, height int

	// groupsLoaded flips on the first applied deck load, so the picker can
	// show a scanning indicator instead of a wrong "no repos" message.
	groupsLoaded bool

	// Ambient plasma panel on the right of the deck. plasma is nil when the
	// panel is disabled; plasmaWidthPct is its configured share of the
	// terminal width. plasmaTicking tracks whether an animation tick is in
	// flight: the tick disarms itself whenever the deck stops being drawn
	// and Update re-arms it on return, so at most one is ever pending and no
	// timer runs while other screens are up.
	plasma         *plasma.Field
	plasmaWidthPct int
	plasmaTicking  bool

	// agentProviders is the normalized provider palette. Legacy/direct configs
	// remain Claude-only through the controller's normalization.
	agentProviders       []agent.Provider
	defaultAgentProvider agent.Provider
}

// statusLevel classifies the footer status so styling and expiry don't rely
// on sniffing the text for the word "error".
type statusLevel int

const (
	statusInfo  statusLevel = iota // transient; auto-expires
	statusError                    // sticks until the next action clears it
)

// transientStatusTTL is how long a non-error status message lingers before the
// status poll clears it.
const transientStatusTTL = 4 * time.Second

// New builds the model.
func New(ctrl *Controller, mgr *session.Manager) Model {
	filter := textinput.New()
	filter.Placeholder = "filter repos…"
	filter.Prompt = "› "
	filter.Focus()

	name := textinput.New()
	name.Placeholder = "branch-name"
	name.Prompt = "› "

	rootInput := textinput.New()
	rootInput.Placeholder = "~/repos"
	rootInput.Prompt = "› "

	promptInput := textarea.New()
	promptInput.Placeholder = ""
	promptInput.Prompt = ""
	promptInput.ShowLineNumbers = false
	promptInput.DynamicHeight = true
	promptInput.MinHeight = 3
	promptInput.MaxHeight = 8
	promptInput.SetHeight(promptInput.MinHeight)
	promptStyles := promptInput.Styles()
	promptStyles.Focused.Base = lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(cAccent).
		Padding(0, 1)
	promptStyles.Blurred.Base = lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(cFaint).
		Padding(0, 1)
	promptStyles.Focused.Placeholder = dimStyle
	promptStyles.Blurred.Placeholder = dimStyle
	promptInput.SetStyles(promptStyles)

	paletteInput := textinput.New()
	paletteInput.Placeholder = "run a command…"
	paletteInput.Prompt = "› "

	// A nil field simply hides the panel. Construction only fails on variant
	// names config validation didn't check, i.e. when width is 0 — which
	// disables the panel anyway.
	var plasmaField *plasma.Field
	pc := ctrl.PlasmaConfig()
	if pc.Width > 0 {
		plasmaField, _ = plasma.New(plasma.Options{
			Pattern: pc.Pattern, Palette: pc.Palette, Charset: pc.Charset,
			Speed: pc.Speed,
		})
	}

	providers := ctrl.EnabledAgentProviders()
	defaultProvider := ctrl.DefaultAgentProvider()
	if !providerIn(defaultProvider, providers) {
		defaultProvider = providers[0]
	}

	return Model{
		ctrl: ctrl, mgr: mgr, state: state.Load(),
		keys:           ctrl.Keys(),
		iconMode:       ctrl.DisplayIcons(),
		usageThreshold: ctrl.UsageThreshold(),
		plasma:         plasmaField, plasmaWidthPct: pc.Width,
		filter: filter, nameInput: name, rootInput: rootInput,
		promptInput:    promptInput,
		paletteInput:   paletteInput,
		agentProviders: providers, defaultAgentProvider: defaultProvider,
		formProvider:    defaultProvider,
		focus:           focusNew,
		agentPrevStatus: map[int]string{},
		attention:       map[int]attnEntry{},
		removed:         map[int]int64{},
		codexTranscript: map[*session.Session]bool{},
		stackInfo:       map[string]stackEntry{},
		stackFetch:      stack.Status,
		stackSubmit:     stack.Submit,
		stackRestack:    stack.Restack,
		stackReuse:      stack.Reuse,
		stackArchive:    stack.ArchiveCleanup,
		stackMerge:      stack.Merge,
		stackAutoMerge:  ctrl.StackAutoMerge(),
		termPaneBusy:    func(s *session.Session) bool { return s.HasForegroundProcess() },
	}
}

func providerIn(provider agent.Provider, providers []agent.Provider) bool {
	for _, candidate := range providers {
		if candidate == provider {
			return true
		}
	}
	return false
}

// normalizedProvider migrates live sessions created by legacy specs (which
// carry no provider metadata) to Claude at the UI boundary.
func normalizedProvider(provider agent.Provider) agent.Provider {
	if provider.Valid() {
		return provider
	}
	return agent.Claude
}

func providerName(provider agent.Provider) string {
	name := normalizedProvider(provider).String()
	if name == "" {
		return "Agent"
	}
	return strings.ToUpper(name[:1]) + name[1:]
}

func (m Model) claudeEnabled() bool {
	return providerIn(agent.Claude, m.agentProviders)
}

func (m Model) codexEnabled() bool {
	return providerIn(agent.Codex, m.agentProviders)
}

func (m Model) usageEnabled() bool {
	return m.claudeEnabled() || m.codexEnabled()
}

// EnterSetup switches the model into first-run setup mode, prompting the user
// to choose a repos root before caretaker starts normally.
func (m Model) EnterSetup(configPath string) Model {
	m.screen = screenSetup
	m.configPath = configPath
	m.rootInput.Focus()
	return m
}

// --- messages ---

type loadedMsg struct {
	groups []Group
	err    error
	seq    uint64 // issue-time load generation; stale results are dropped
}

type createdMsg struct {
	wt  repo.Worktree
	err error
}

type actionDoneMsg struct{ err error }

type archivePreflightMsg struct {
	it          activeItem
	fingerprint string
	dirty       bool
	err         error
}

type dirtyMsg struct{}

// diffMsg carries one diff fetch's results back to the UI goroutine. It stamps
// the target worktree key so a result that lands after the user closed or
// switched the diff is dropped (mirrors loadedMsg's seq-guard philosophy; a key
// match suffices since only one diff is open at a time). It carries both scopes'
// data — the committed body+numstat are populated only when the branch has a
// base to diff against — so both scope renderings can be built at once.
type diffMsg struct {
	key             string
	committedBody   string
	committedStat   []repo.FileStat
	uncommittedBody string
	uncommittedStat []repo.FileStat
	untracked       []string
	err             error
}

// statusTickMsg fires on the status-poll timer; statusMsg carries the result of
// one `claude agents --json` poll.
type statusTickMsg struct{}

// statusExpireMsg is the dedicated one-shot flash-expiry tick scheduled by
// flashCmd. It carries the statusAt it was armed for so a newer flash set before
// it fires is never cleared early: only the flash that scheduled this tick may
// expire it. This is what keeps a flash on an idle deck (whose agent-status poll
// stretches to 30s) from lingering ~26s past its 4s TTL.
type statusExpireMsg struct{ at time.Time }

type statusMsg struct {
	byPid map[int]AgentStatus
	err   error
}

// agentEventMsg carries one passive provider event for a hosted session. The
// command is re-armed after each message, leaving Bubble Tea as the sole owner
// of Model, SessionID persistence, and notification state.
type agentEventMsg struct {
	session *session.Session
	event   agent.Event
	open    bool
}

// plasmaTickMsg advances the deck's plasma panel one frame. Purely cosmetic,
// so unlike the poll ticks it is only kept armed while the deck body is
// actually being drawn (see Update / deckShown).
type plasmaTickMsg struct{}

// plasmaTickInterval paces the plasma at ~6–7 fps. The default speed is slow
// enough that motion between frames stays sub-cell, so this reads as liquid
// without the redraw cost of a real frame rate; the deck is also the one
// screen with no PTY content, so each redraw is cheap.
const plasmaTickInterval = 150 * time.Millisecond

// schedulePlasmaTick arms one plasma frame tick.
func schedulePlasmaTick() tea.Cmd {
	return tea.Tick(plasmaTickInterval, func(time.Time) tea.Msg { return plasmaTickMsg{} })
}

// usageTickMsg fires on the usage-poll timer; usageMsg carries the result of
// one plan-usage fetch.
type usageTickMsg struct{}

type usageMsg struct {
	provider agent.Provider
	snap     usage.Snapshot
	err      error
}

// usageSample is one successful poll's five-hour utilization, kept in
// Model.usageHist so the overlay can estimate the burn rate.
type usageSample struct {
	at   time.Time
	util float64
}

// Plan-usage cadence and freshness bounds. The poll is slow (the window moves
// in minutes, not seconds); a snapshot older than usageStaleAfter no longer
// steers the bar segment, and samples beyond usageHistoryWindow no longer
// weigh on the burn-rate estimate.
const (
	usagePollInterval  = 60 * time.Second
	usageStaleAfter    = 5 * time.Minute
	usageHistoryWindow = time.Hour
)

// Init implements tea.Model.
func (m Model) Init() tea.Cmd {
	if m.screen == screenSetup {
		return tea.Batch(textinput.Blink, m.repaintCmd())
	}
	cmds := []tea.Cmd{m.loadCmd(), textinput.Blink, m.repaintCmd()}
	if m.claudeEnabled() {
		cmds = append(cmds, m.pollStatusCmd())
	}
	if m.usageEnabled() {
		cmds = append(cmds, m.pollUsageCmds(), m.scheduleUsageTick())
	}
	return tea.Batch(cmds...)
}

// pollStatusCmd runs one `claude agents --json` poll off the UI goroutine.
func (m Model) pollStatusCmd() tea.Cmd {
	ctrl := m.ctrl
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
		defer cancel()
		st, err := ctrl.AgentStatuses(ctx)
		return statusMsg{byPid: st, err: err}
	}
}

func (m Model) watchAgentEvents(s *session.Session) tea.Cmd {
	if s == nil || s.Events == nil {
		return nil
	}
	return func() tea.Msg {
		event, ok := <-s.Events
		return agentEventMsg{session: s, event: event, open: ok}
	}
}

func (m Model) watchWorkspaceAgents(ws *session.Workspace) tea.Cmd {
	if ws == nil {
		return nil
	}
	cmds := make([]tea.Cmd, 0, len(ws.Agents))
	for _, s := range ws.Agents {
		if cmd := m.watchAgentEvents(s); cmd != nil {
			cmds = append(cmds, cmd)
		}
	}
	return tea.Batch(cmds...)
}

// scheduleStatusTick re-arms the poll timer at statusTickInterval.
func (m Model) scheduleStatusTick() tea.Cmd {
	return tea.Tick(m.statusTickInterval(), func(time.Time) tea.Msg { return statusTickMsg{} })
}

// pollUsageCmds runs one plan-usage fetch per enabled provider off the UI
// goroutine. Provider failures are independent: a missing sign-in for one does
// not suppress the other provider's snapshot.
func (m Model) pollUsageCmds() tea.Cmd {
	var cmds []tea.Cmd
	if m.claudeEnabled() {
		cmds = append(cmds, m.pollUsageCmd(agent.Claude))
	}
	if m.codexEnabled() {
		cmds = append(cmds, m.pollUsageCmd(agent.Codex))
	}
	return tea.Batch(cmds...)
}

func (m Model) pollUsageCmd(provider agent.Provider) tea.Cmd {
	ctrl := m.ctrl
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		var (
			snap usage.Snapshot
			err  error
		)
		switch provider {
		case agent.Codex:
			cfg, cfgErr := ctrl.providerConfig(agent.Codex)
			if cfgErr != nil {
				err = cfgErr
			} else {
				snap, err = usage.FetchCodex(ctx, cfg.Command, cfg.Args)
			}
		default:
			snap, err = usage.Fetch(ctx)
		}
		return usageMsg{provider: provider, snap: snap, err: err}
	}
}

// scheduleUsageTick re-arms the usage poll at usagePollInterval.
func (m Model) scheduleUsageTick() tea.Cmd {
	return tea.Tick(usagePollInterval, func(time.Time) tea.Msg { return usageTickMsg{} })
}

// usageFresh reports whether the latest snapshot is recent enough to steer
// the bar segment; the overlay keeps showing whatever it has regardless.
func (m Model) usageState(provider agent.Provider) (usage.Snapshot, bool, []usageSample) {
	if normalizedProvider(provider) == agent.Codex {
		return m.codexUsageSnap, m.codexUsageHave, m.codexUsageHist
	}
	return m.usageSnap, m.usageHave, m.usageHist
}

func (m Model) usageFresh(provider agent.Provider) bool {
	snap, have, _ := m.usageState(provider)
	return have && time.Since(snap.FetchedAt) <= usageStaleAfter
}

func (m *Model) applyUsage(provider agent.Provider, snap usage.Snapshot) {
	provider = normalizedProvider(provider)
	_, _, hist := m.usageState(provider)
	if window := sessionUsageWindow(snap); window != nil {
		hist = append(hist, usageSample{at: snap.FetchedAt, util: window.Utilization})
	}
	cutoff := time.Now().Add(-usageHistoryWindow)
	for len(hist) > 0 && hist[0].at.Before(cutoff) {
		hist = hist[1:]
	}
	if provider == agent.Codex {
		m.codexUsageSnap, m.codexUsageHave, m.codexUsageHist = snap, true, hist
		return
	}
	m.usageSnap, m.usageHave, m.usageHist = snap, true, hist
}

func sessionUsageWindow(snap usage.Snapshot) *usage.Window {
	for _, named := range snap.Windows() {
		if named.Session {
			return named.Window
		}
	}
	return nil
}

// statusTickInterval picks the agent-poll cadence: 2s while any agent is
// active (busy/waiting), 5s while ct hosts idle sessions, and 30s when ct hosts
// nothing at all — the slow tick only keeps a lazy watch for claude sessions
// started outside ct in known worktrees, so their deck badges still appear
// (just up to 30s late) without ct spawning a subprocess every 5s for an empty
// deck.
func (m Model) statusTickInterval() time.Duration {
	interval := 5 * time.Second
	if m.mgr == nil || m.mgr.Count() == 0 {
		interval = 30 * time.Second
	}
	for _, st := range m.agentStatus {
		if st.Status == "busy" || st.Status == "waiting" {
			return 2 * time.Second
		}
	}
	return interval
}

// loadCmd issues one deck refresh. The generation stamp is taken at issue
// time, so when several loads overlap (activate, then a quick stop, then a
// refresh) only the newest one's result is applied — a slow stale scan can't
// clobber fresher state.
func (m Model) loadCmd() tea.Cmd {
	ctrl := m.ctrl
	seq := ctrl.loadSeq.Add(1)
	return func() tea.Msg {
		g, err := ctrl.Load()
		return loadedMsg{groups: g, err: err, seq: seq}
	}
}

// repaintCmd blocks until a session's screen changes, then asks for a
// re-render. WaitDirty applies the leading-edge coalescing window so a burst of
// pty output (a build, a streaming agent) collapses into a capped repaint rate
// instead of re-serialising every vt buffer as fast as frames complete, while a
// lone keystroke echo still repaints with no added latency.
func (m Model) repaintCmd() tea.Cmd {
	mgr := m.mgr
	return func() tea.Msg {
		mgr.WaitDirty()
		return dirtyMsg{}
	}
}

func (m Model) sessionSize() (int, int) {
	return m.width, max(1, m.height-barHeight-m.sessionFooterH())
}

// dismissHint retires the session help hint on the user's first keystroke into
// a session and hands the reclaimed row back to the workspace's sessions. It's
// a no-op once the hint has already been dismissed.
func (m Model) dismissHint() Model {
	if m.hintSeen {
		return m
	}
	m.hintSeen = true
	if m.current != nil {
		w, h := m.sessionSize()
		m.mgr.ResizeWorkspace(m.current.key, w, h)
	}
	return m
}

// toggleZoom maximizes the active terminal pane or restores the split layout,
// resizing the panes to match. Shared by the zoom key and a click on the bar's
// zoom toggle. A no-op without a current workspace.
func (m Model) toggleZoom() (tea.Model, tea.Cmd) {
	if m.current == nil {
		return m, nil
	}
	key := m.current.key
	w, h := m.sessionSize()
	m.mgr.ZoomTermPane(key)
	m.mgr.ResizeTermPanes(key, w, h)
	return m, nil
}

func (m Model) activeSession() *session.Session {
	if m.current == nil || m.current.ws == nil {
		return nil
	}
	switch m.screen {
	case screenEditor:
		return m.current.ws.Editor
	case screenTerminal:
		return m.current.ws.ActiveTermSession()
	case screenAgent:
		return m.current.ws.ActiveAgentSession()
	default:
		return nil
	}
}

// Update implements tea.Model. After every message it re-declares which
// sessions are on screen so the manager can drop repaint wakeups from
// invisible ones (off-screen agents streaming output would otherwise trigger
// a full re-render each pty read).
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	mm, cmd := m.update(msg)
	if model, ok := mm.(Model); ok {
		model.syncVisible()
		// Arm the plasma tick whenever the deck panel (re)appears; the tick
		// disarms itself when it fires off-deck, keeping at most one pending.
		if !model.plasmaTicking && model.plasmaShown() && model.plasma.Animated() {
			model.plasmaTicking = true
			cmd = tea.Batch(cmd, schedulePlasmaTick())
		}
		return model, cmd
	}
	return mm, cmd
}

// plasmaShown reports whether the plasma panel is being drawn right now:
// the deck is the visible body (mirrors View's branch order — overlays win
// over the picker screen) and the terminal is wide enough for the split.
func (m Model) plasmaShown() bool {
	return m.screen == screenPicker && !m.helpOpen && !m.boardOpen && !m.usageOpen &&
		!m.paletteOpen && !m.diffOpen && !m.stackOpen && m.plasmaWidth() > 0
}

// syncVisible pushes the currently visible session set to the manager.
func (m Model) syncVisible() {
	if m.mgr == nil {
		return
	}
	m.mgr.SetVisible(m.visibleSessions()...)
}

// visibleSessions returns the sessions whose output is currently drawn:
// nothing while the picker, setup, or a full-body overlay (help, board,
// usage) is shown; the editor or focused agent on their screens; and on the
// terminal screen either the focused pane (zoomed/single) or every pane in
// the split layout. Mirrors the branch structure of View.
func (m Model) visibleSessions() []*session.Session {
	// m.diffOpen is redundant today (the diff only opens over the picker, which
	// already returns nil), but it is listed so a future entry point that opens
	// the diff over a session can't leak that session's output onto the screen.
	if m.helpOpen || m.boardOpen || m.usageOpen || m.paletteOpen || m.diffOpen || m.stackOpen || m.screen == screenPicker || m.screen == screenSetup {
		return nil
	}
	if m.current == nil || m.current.ws == nil {
		return nil
	}
	ws := m.current.ws
	switch m.screen {
	case screenEditor:
		return []*session.Session{ws.Editor}
	case screenAgent:
		return []*session.Session{ws.ActiveAgentSession()}
	case screenTerminal:
		if ws.TermZoomed || len(ws.Terms) == 1 || ws.TermLayout == nil {
			return []*session.Session{ws.ActiveTermSession()}
		}
		return ws.Terms
	}
	return nil
}

func (m Model) overlayOpen() bool {
	return m.helpOpen || m.boardOpen || m.usageOpen || m.paletteOpen || m.diffOpen || m.stackOpen
}

func (m Model) update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		// The deck inputs live in the left column, which the plasma panel
		// narrows; size them to that column or they'd overflow the box border.
		inputW := max(10, m.width-m.plasmaWidth()-12)
		m.filter.SetWidth(inputW)
		m.nameInput.SetWidth(inputW)
		m.rootInput.SetWidth(clamp(m.width-14, 20, 52))
		m.promptInput.MaxHeight = clamp(m.height-barHeight-10, 3, 8)
		m.promptInput.SetWidth(clamp(m.width-16, 20, 80))
		m.paletteInput.SetWidth(clamp(m.width-16, 20, 52))
		// Only the current workspace is resized; background ones are brought up
		// to date by Activate when they next become current.
		if m.current != nil {
			w, h := m.sessionSize()
			m.mgr.ResizeWorkspace(m.current.key, w, h)
		}
		return m, nil

	case loadedMsg:
		if msg.seq != m.ctrl.loadSeq.Load() {
			return m, nil // superseded by a newer in-flight load
		}
		if msg.err != nil {
			m.setError("load error: " + msg.err.Error())
			return m, nil
		}
		m.groups = msg.groups
		m.groupsLoaded = true
		m.recomputeMatches()
		m.recomputeActive()
		m.computeRecentRanks()
		// Refresh the deck's stack cache for the active worktrees, respecting the
		// freshness window (the deck's `r` re-kicks with force).
		return m, tea.Batch(m.kickStackFetches(false)...)

	case createdMsg:
		if msg.err != nil {
			m.setError("create error: " + msg.err.Error())
			return m, m.loadCmd()
		}
		return m.activate(msg.wt.Repo, msg.wt.Name, msg.wt.Path)

	case actionDoneMsg:
		if msg.err != nil {
			m.setError(msg.err.Error())
		}
		return m, m.loadCmd()

	case archivePreflightMsg:
		if msg.err != nil {
			m.stackView.working = false
			m.setError("archive: " + msg.err.Error())
			return m, nil
		}
		return m.beginArchive(msg), nil

	case dirtyMsg:
		return m, m.repaintCmd()

	case diffMsg:
		// Drop a stale result: the diff was closed, or a newer one opened for a
		// different worktree, before this fetch returned.
		if !m.diffOpen || msg.key != m.diffView.key {
			return m, nil
		}
		if msg.err != nil {
			m.setError("diff error: " + msg.err.Error())
			m.diffOpen = false
			m.diffView = diffState{}
			return m, nil
		}
		m.diffView.loading = false
		buildDiffContent(&m.diffView, msg, m.width)
		return m, nil

	case stackStatusMsg:
		m.applyStackStatus(msg)
		return m, nil

	case stackSubmitMsg:
		m.applyStackSubmit(msg)
		return m, nil

	case stackRestackMsg:
		m.applyStackRestack(msg)
		return m, nil

	case stackMergeMsg:
		m.applyStackMerge(msg)
		return m, nil

	case plasmaTickMsg:
		if !m.plasmaShown() {
			m.plasmaTicking = false // dormant; Update re-arms on return
			return m, nil
		}
		m.plasma.Advance(plasmaTickInterval.Seconds())
		return m, schedulePlasmaTick()

	case agentEventMsg:
		if !msg.open {
			key, _, ok := m.agentLocation(msg.session)
			if !ok {
				return m, nil
			}
			pid := msg.session.Pid()
			if status, tracked := m.agentStatus[pid]; tracked && normalizedProvider(status.Provider) == agent.Codex {
				previous := status.Status
				status.Status, status.WaitingFor = "idle", ""
				m.applyProviderTransition(pid, key, previous, status)
				m.agentStatus[pid] = status
				if m.agentPrevStatus == nil {
					m.agentPrevStatus = map[int]string{}
				}
				m.agentPrevStatus[pid] = status.Status
			}
			return m, nil
		}
		key, path, ok := m.agentLocation(msg.session)
		if !ok {
			// A final buffered event from a replaced/closed session must not
			// mutate the replacement that may now reuse its pid.
			return m, nil
		}
		pid := msg.session.Pid()
		cmds := []tea.Cmd{m.watchAgentEvents(msg.session)}
		if msg.event.Kind == agent.ThreadStarted && msg.event.ThreadID != "" && msg.session.SessionID != msg.event.ThreadID {
			msg.session.SessionID = msg.event.ThreadID
			cmds = append(cmds, m.saveAgents(key))
		}

		if m.agentStatus == nil {
			m.agentStatus = map[int]AgentStatus{}
		}
		if m.agentPrevStatus == nil {
			m.agentPrevStatus = map[int]string{}
		}
		previous := m.agentStatus[pid]
		status := previous
		status.Provider = agent.Codex
		status.Cwd = path
		if status.StartedAt == 0 {
			status.StartedAt = time.Now().UnixMilli()
		}
		if status.Status == "" {
			status.Status = "idle"
		}
		statusChanged := false
		switch msg.event.Kind {
		case agent.ThreadStatusChanged:
			status.WaitingFor = ""
			switch {
			case msg.event.WaitingOnApproval:
				status.Status, status.WaitingFor = "waiting", "permission prompt"
			case msg.event.WaitingOnUserInput:
				status.Status, status.WaitingFor = "waiting", "input needed"
			case msg.event.Status == "active":
				status.Status = "busy"
			default:
				status.Status = "idle"
			}
			statusChanged = true
		case agent.TurnStarted:
			status.Status, status.WaitingFor = "busy", ""
			statusChanged = true
		case agent.TurnCompleted:
			if msg.event.Status == "inProgress" {
				status.Status = "busy"
			} else {
				status.Status, status.WaitingFor = "idle", ""
			}
			statusChanged = true
		case agent.Error:
			status.Status, status.WaitingFor = "idle", ""
			statusChanged = true
			if msg.event.Message != "" {
				cmds = append(cmds, m.flashCmd("Codex agent error: "+msg.event.Message))
			}
		case agent.Disconnected:
			status.Status, status.WaitingFor = "idle", ""
			statusChanged = true
			cmds = append(cmds, m.flashCmd("Codex status connection lost"))
		}
		if statusChanged {
			m.applyProviderTransition(pid, key, previous.Status, status)
			m.agentStatus[pid] = status
			m.agentPrevStatus[pid] = status.Status
		}
		return m, tea.Batch(cmds...)

	case statusTickMsg:
		if !m.claudeEnabled() {
			return m, nil
		}
		return m, m.pollStatusCmd()

	case statusExpireMsg:
		// Ignore a tick left over from a flash that a newer one has since
		// replaced (statusAt moved); otherwise the fresh flash would be cleared
		// early. maybeExpireStatus's own TTL check makes the surviving tick a
		// no-op if the status was already replaced by a non-transient one.
		if msg.at.Equal(m.statusAt) {
			m.maybeExpireStatus()
		}
		return m, nil

	case statusMsg:
		var flashTick tea.Cmd
		if msg.err != nil {
			// Surface a persistent outage once (transient, auto-expires): with
			// the poll dead, every badge/board feature silently freezes and the
			// user should know why.
			m.pollFails++
			if m.pollFails == 3 {
				flashTick = m.flashCmd("agent status unavailable (claude agents failing)")
			}
		}
		if msg.err == nil {
			m.pollFails = 0
			// Retire tombstones the poll no longer corroborates: the deleted
			// agent's pid is finally absent, or now carries a different StartedAt
			// (the pid was recycled to a new agent). Either way the suppression
			// below must stop so the fresh entry is honored. Done against the raw
			// poll before any entry is dropped from msg.byPid.
			for pid, startedAt := range m.removed {
				st, ok := msg.byPid[pid]
				if !ok || (startedAt != 0 && st.StartedAt != startedAt) {
					delete(m.removed, pid)
				}
			}
			// Claude polling is a replace-all snapshot for Claude only. Preserve
			// Codex statuses, which arrive independently from App Server events.
			nextStatus := make(map[int]AgentStatus, len(m.agentStatus)+len(msg.byPid))
			for pid, st := range m.agentStatus {
				if normalizedProvider(st.Provider) == agent.Codex {
					nextStatus[pid] = st
				}
			}
			for pid, st := range msg.byPid {
				// A tombstoned pid is an agent ct deleted that the daemon still
				// lists. Drop it from the snapshot entirely so it can neither
				// re-raise the "waiting" badge nor trigger the bell / done-marker
				// loops below, which also read msg.byPid.
				if _, tombstoned := m.removed[pid]; tombstoned {
					delete(msg.byPid, pid)
					continue
				}
				st.Provider = agent.Claude
				msg.byPid[pid] = st
				nextStatus[pid] = st
			}
			bell := false
			for pid, st := range msg.byPid {
				prev := m.agentPrevStatus[pid]
				if prev != "busy" || (st.Status != "idle" && st.Status != "waiting") {
					continue
				}
				// Skip the bell/marker if the user is already watching this agent.
				if m.watchingAgent(pid) {
					continue
				}
				if st.Status == "waiting" {
					bell = true // the ! badge is derived live from agentStatus
					continue
				}
				if key, ok := m.pathToKey(st.Cwd); ok {
					m.recordAttention(pid, attnDone, key, st.StartedAt)
					bell = true
				}
			}
			// Mark non-bg agents that disappeared between polls while busy
			// (e.g. the process crashed or was killed). Use the last-known Cwd so
			// we can map the pid to its workspace.
			for pid, prev := range m.agentPrevStatus {
				if _, stillTracked := msg.byPid[pid]; stillTracked {
					continue
				}
				if last, ok := m.agentStatus[pid]; !ok || normalizedProvider(last.Provider) != agent.Claude {
					continue
				}
				if prev != "busy" {
					continue
				}
				if last, ok := m.agentStatus[pid]; ok {
					if key, ok := m.pathToKey(last.Cwd); ok {
						m.recordAttention(pid, attnDone, key, last.StartedAt)
						bell = true
					}
				}
			}
			if bell {
				fmt.Fprint(os.Stderr, "\a")
			}
			// Track when each agent's current busy stretch began, for the
			// board's elapsed-time column. Accurate to one poll interval.
			if m.busySince == nil {
				m.busySince = map[int]time.Time{}
			}
			for pid, st := range nextStatus {
				if st.Status == "busy" {
					if _, ok := m.busySince[pid]; !ok {
						m.busySince[pid] = time.Now()
					}
				} else {
					delete(m.busySince, pid)
				}
			}
			for pid := range m.busySince {
				if _, ok := nextStatus[pid]; !ok {
					delete(m.busySince, pid)
				}
			}
			// Drop stored attention entries whose pid has been recycled: if the
			// polled process at pid started at a different, known time than the
			// entry's known startedAt, the pid now belongs to a different agent
			// and the marker no longer applies.
			for pid, e := range m.attention {
				if e.startedAt == 0 {
					continue
				}
				if st, ok := nextStatus[pid]; ok && st.StartedAt != 0 && st.StartedAt != e.startedAt {
					delete(m.attention, pid)
				}
			}
			m.agentPrevStatus = make(map[int]string, len(nextStatus))
			for pid, st := range nextStatus {
				m.agentPrevStatus[pid] = st.Status
			}
			m.agentStatus = nextStatus
		}
		return m, tea.Batch(m.scheduleStatusTick(), flashTick)

	case usageTickMsg:
		if !m.usageEnabled() {
			return m, nil
		}
		return m, tea.Batch(m.pollUsageCmds(), m.scheduleUsageTick())

	case usageMsg:
		// Not signed into Claude Code: the feature simply doesn't exist here.
		// Stop polling — no retries, no error, nothing to show.
		if errors.Is(msg.err, usage.ErrNoCredentials) {
			return m, nil
		}
		if msg.err != nil {
			// Degrade silently: this provider's snapshot goes stale and its bar
			// segment hides on its own. The shared timer continues polling both.
			return m, nil
		}
		m.applyUsage(msg.provider, msg.snap)
		return m, nil

	case tea.MouseClickMsg:
		return m.handleMouseClick(msg)
	case tea.MouseReleaseMsg:
		if m.overlayOpen() {
			return m, nil
		}
		m.forwardMouse(msg)
		return m, nil
	case tea.MouseWheelMsg:
		// The diff viewer owns the wheel while open: it scrolls the diff rather
		// than falling through to the deck lists or a session beneath.
		if m.diffOpen {
			return m.diffWheel(msg)
		}
		if m.overlayOpen() {
			return m, nil
		}
		if m.screen == screenPicker {
			return m.deckWheel(msg)
		}
		m.forwardMouse(msg)
		return m, nil
	case tea.MouseMotionMsg:
		if m.overlayOpen() {
			return m, nil
		}
		m.forwardMouse(msg)
		return m, nil

	case tea.KeyPressMsg:
		return m.handleKey(msg)

	case tea.PasteMsg:
		// Bubble Tea v2 delivers bracketed paste as its own message type, not a
		// KeyPressMsg, so it needs routing of its own — see handlePaste.
		return m.handlePaste(msg)

	default:
		// Route everything else (notably the cursor-blink tick from
		// textinput.Blink, started in Init) to whichever text input is
		// currently focused, so its blink loop keeps re-arming itself.
		// KeyPressMsg, paste, and mouse messages never reach here — they have
		// explicit cases above.
		mm, cmd := m.routeToFocusedInputs(msg)
		return mm, cmd
	}
}

// routeToFocusedInputs forwards msg to every currently-focused input and
// batches their commands. It backs both the default branch (where the
// cursor-blink tick must reach the focused input so its blink loop re-arms)
// and the non-session path of the paste router.
func (m Model) routeToFocusedInputs(msg tea.Msg) (Model, tea.Cmd) {
	var cmds []tea.Cmd
	if m.promptInput.Focused() {
		var cmd tea.Cmd
		m.promptInput, cmd = m.promptInput.Update(msg)
		cmds = append(cmds, cmd)
	}
	for _, in := range []*textinput.Model{&m.filter, &m.nameInput, &m.rootInput, &m.paletteInput} {
		if !in.Focused() {
			continue
		}
		var cmd tea.Cmd
		*in, cmd = in.Update(msg)
		cmds = append(cmds, cmd)
	}
	return m, tea.Batch(cmds...)
}

// handlePaste routes a bracketed-paste message. Pastes need the same routing
// keys get: without a case of their own they fall to the default branch and
// reach only a focused textinput, so on a session screen (where none is
// focused) the paste silently vanishes — the bug this fixes.
//
//   - On overlays (help/board/usage), setup, and the picker the paste behaves
//     like typed input: it goes to whichever input is focused (the picker
//     filter, the name/root/prompt inputs), and is otherwise swallowed — it
//     never leaks into a session drawn beneath an overlay.
//   - On a bare session screen it is handed to the active program through the
//     emulator's paste-aware path (Session.Paste), which honors the child's
//     bracketed-paste mode.
func (m Model) handlePaste(msg tea.PasteMsg) (tea.Model, tea.Cmd) {
	onSession := m.screen != screenPicker && m.screen != screenSetup &&
		!m.helpOpen && !m.boardOpen && !m.usageOpen && !m.paletteOpen && !m.diffOpen && !m.stackOpen &&
		!m.confirmActive()
	if onSession {
		if s := m.activeSession(); s != nil {
			m = m.dismissHint() // first input into a session retires the hint
			s.Paste(msg.Content)
		}
		return m, nil
	}
	// The diff and stack overlays have no input field, so a paste is swallowed
	// rather than routed to the deck filter (which stays focused across screens).
	if m.diffOpen || m.stackOpen {
		return m, nil
	}
	// The palette owns its input exclusively while open. Routing by focus here
	// would also hand the paste to the deck filter, which stays focused across
	// screens — the query must not be duplicated into it.
	if m.paletteOpen {
		var cmd tea.Cmd
		m.paletteInput, cmd = m.paletteInput.Update(msg)
		m.paletteCursor = 0
		return m, cmd
	}
	if m.confirmActive() {
		return m, nil
	}
	mm, cmd := m.routeToFocusedInputs(msg)
	// Mirror handleNewKey: keep the repo matches in sync when the paste lands
	// in the picker's filter.
	if mm.filter.Focused() {
		mm.recomputeMatches()
	}
	return mm, cmd
}

// watchingAgent reports whether the user is on the agent screen with the agent
// running as pid focused.
func (m Model) watchingAgent(pid int) bool {
	return m.screen == screenAgent && m.current != nil && m.current.ws != nil &&
		m.current.ws.ActiveAgentSession() != nil &&
		m.current.ws.ActiveAgentSession().Pid() == pid
}

// recordAttention stores an unread marker for pid unless an equal or higher one
// is already recorded.
func (m Model) recordAttention(pid int, level attnLevel, key string, startedAt int64) {
	if cur, ok := m.attention[pid]; ok && cur.level >= level {
		return
	}
	m.attention[pid] = attnEntry{level: level, key: key, startedAt: startedAt}
}

// clearWorkspaceAttention drops the stored unread markers for every agent in
// the current workspace. Live waiting badges are derived from agentStatus and
// clear themselves once the agent's polled status changes.
func (m Model) clearWorkspaceAttention() {
	if m.current == nil {
		return
	}
	for pid, e := range m.attention {
		if e.key == m.current.key {
			delete(m.attention, pid)
		}
	}
}

// agentAttn returns pid's effective attention level: a live "waiting" status
// wins, then any stored unread marker.
func (m Model) agentAttn(pid int) attnLevel {
	if pid == 0 {
		return attnNone
	}
	if m.agentStatus[pid].Status == "waiting" {
		return attnWaiting
	}
	return m.attention[pid].level
}

// worktreeAttn returns the highest attention level among key's agents, for the
// deck's per-worktree badge: live-waiting agents mapped to key via their Cwd,
// then stored unread markers.
func (m Model) worktreeAttn(key string) attnLevel {
	level := attnNone
	for pid, st := range m.agentStatus {
		if st.Status != "waiting" || m.watchingAgent(pid) {
			continue
		}
		if k, ok := m.pathToKey(st.Cwd); ok && k == key {
			level = attnWaiting
		}
	}
	for _, e := range m.attention {
		if e.key == key && e.level > level {
			level = e.level
		}
	}
	return level
}

// applyProviderTransition updates elapsed/attention state for one structured
// provider event, matching the busy→waiting/idle semantics of Claude polling.
func (m *Model) applyProviderTransition(pid int, key, previous string, status AgentStatus) {
	if m.busySince == nil {
		m.busySince = map[int]time.Time{}
	}
	if status.Status == "busy" {
		if _, ok := m.busySince[pid]; !ok {
			m.busySince[pid] = time.Now()
		}
	} else {
		delete(m.busySince, pid)
	}
	if previous != "busy" || (status.Status != "idle" && status.Status != "waiting") || m.watchingAgent(pid) {
		return
	}
	if status.Status == "idle" {
		m.recordAttention(pid, attnDone, key, status.StartedAt)
	}
	fmt.Fprint(os.Stderr, "\a")
}

// attnSummary counts, for the bar badge: worktrees with a live-waiting agent
// and worktrees with unread completions.
func (m Model) attnSummary() (waiting, done int) {
	waitKeys := map[string]bool{}
	for pid, st := range m.agentStatus {
		if st.Status != "waiting" || m.watchingAgent(pid) {
			continue
		}
		if key, ok := m.pathToKey(st.Cwd); ok {
			waitKeys[key] = true
		}
	}
	doneKeys := map[string]bool{}
	for _, e := range m.attention {
		if e.level == attnDone {
			doneKeys[e.key] = true
		}
	}
	return len(waitKeys), len(doneKeys)
}

// buildBoard constructs the agent board: every agent of every open workspace,
// grouped under non-navigable worktree header rows, with worktrees (and agents
// within them) needing attention sorted first and a trailing "+ new agent" row.
// nav maps cursor positions to indices in rows.
func (m Model) buildBoard() (rows []boardRow, nav []int) {
	type group struct {
		key, repo, worktree, path string
		agents                    []boardRow
		attn                      attnLevel
		opened                    int64
	}

	seen := map[string]bool{}
	var groups []group
	addGroup := func(key, repoName, wtName, path string) {
		if key == "" || seen[key] {
			return
		}
		seen[key] = true
		ws, ok := m.mgr.Workspace(key)
		if !ok {
			if m.current == nil || m.current.key != key || m.current.ws == nil {
				return
			}
			ws = m.current.ws
		}
		g := group{key: key, repo: repoName, worktree: wtName, path: path}
		if m.state != nil {
			g.opened = m.state.Opened(key)
		}
		for i, a := range ws.Agents {
			pid := a.Pid()
			attn := m.agentAttn(pid)
			provider := normalizedProvider(a.Provider)
			g.agents = append(g.agents, boardRow{
				isAgent: true, key: key, repo: repoName, worktree: wtName, path: path,
				agentIdx: i, pid: pid, label: agentTitle(provider, a.Title), provider: provider,
				status: m.boardStatus(pid, attn), attn: attn,
			})
			if attn > g.attn {
				g.attn = attn
			}
		}
		if len(g.agents) == 0 {
			return
		}
		sort.SliceStable(g.agents, func(i, j int) bool { return g.agents[i].attn > g.agents[j].attn })
		groups = append(groups, g)
	}

	if m.current != nil {
		addGroup(m.current.key, m.current.repo, m.current.worktree, m.current.path)
	}
	for _, it := range m.active {
		addGroup(wsKey(it.repo.Name, it.view.WT.Name), it.repo.Name, it.view.WT.Name, it.view.WT.Path)
	}

	// The home workspace isn't a git worktree, so it never appears in m.active.
	if m.homeWSKey != "" {
		addGroup(m.homeWSKey, "~", "config", m.homeWSPath)
	}

	isCurrent := func(key string) bool { return m.current != nil && m.current.key == key }
	sort.SliceStable(groups, func(i, j int) bool {
		if groups[i].attn != groups[j].attn {
			return groups[i].attn > groups[j].attn // attention floats to the top
		}
		if ci, cj := isCurrent(groups[i].key), isCurrent(groups[j].key); ci != cj {
			return ci
		}
		if groups[i].opened != groups[j].opened {
			return groups[i].opened > groups[j].opened // then most recently opened
		}
		return groups[i].key < groups[j].key
	})

	num := 0
	for _, g := range groups {
		rows = append(rows, boardRow{key: g.key, repo: g.repo, worktree: g.worktree, path: g.path, attn: g.attn})
		for _, r := range g.agents {
			num++
			if num <= 9 {
				r.num = num
			}
			nav = append(nav, len(rows))
			rows = append(rows, r)
		}
	}
	rows = append(rows, boardRow{isNew: true})
	nav = append(nav, len(rows)-1)
	return rows, nav
}

// boardStatus renders the right-hand column for an agent row: the live polled
// status.
func (m Model) boardStatus(pid int, attn attnLevel) string {
	st := m.agentStatus[pid]
	switch st.Status {
	case "busy":
		// Elapsed time changes triage: a 12-minute agent means something
		// different from a 20-second one. Shown once it's meaningful (>=5s).
		if since, ok := m.busySince[pid]; ok {
			if d := time.Since(since); d >= 5*time.Second {
				return "working · " + humanDur(d)
			}
		}
		return "working"
	case "waiting":
		if st.WaitingFor != "" {
			return "waiting · " + st.WaitingFor
		}
		return "waiting"
	case "idle":
		if attn == attnDone {
			return "ready · new output"
		}
		return "idle"
	default:
		return ""
	}
}

// openBoard toggles the agent board. The cursor starts on the first row needing
// attention, falling back to the current workspace's focused agent.
func (m Model) openBoard() (tea.Model, tea.Cmd) {
	if m.boardOpen {
		m.boardOpen = false
		m.formOpen = false
		return m, nil
	}
	m.boardOpen = true
	m.formOpen = false
	m.boardCursor = 0
	rows, nav := m.buildBoard()
	for i, ri := range nav {
		if rows[ri].isAgent && rows[ri].attn > attnNone {
			m.boardCursor = i
			return m, nil
		}
	}
	for i, ri := range nav {
		r := rows[ri]
		if r.isAgent && m.current != nil && r.key == m.current.key &&
			m.current.ws != nil && r.agentIdx == m.current.ws.ActiveAgent {
			m.boardCursor = i
			break
		}
	}
	return m, nil
}

// handleBoard routes key events while the agent board is open.
func (m Model) handleBoard(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch m.mode {
	case modeConfirmAgentClose:
		return m.handleConfirmAgentKey(msg, false)
	case modeConfirmAgentRestart:
		return m.handleConfirmAgentKey(msg, true)
	}
	if m.formOpen {
		return m.handleBoardForm(msg)
	}
	rows, nav := m.buildBoard()
	rowAt := func(i int) (boardRow, bool) {
		if i < 0 || i >= len(nav) {
			return boardRow{}, false
		}
		return rows[nav[i]], true
	}
	switch msg.String() {
	case "esc", "q", m.keys.Palette:
		m.boardOpen = false
		return m, nil
	case m.keys.Attention:
		// The jump works from within the board too: focusBoardAgent closes it.
		return m.jumpAttention()
	case "up", "k", "ctrl+p":
		if m.boardCursor > 0 {
			m.boardCursor--
		}
		return m, nil
	case "down", "j", "ctrl+n":
		if m.boardCursor < len(nav)-1 {
			m.boardCursor++
		}
		return m, nil
	case "n":
		return m.openNewAgentForm(), nil
	case "r":
		if r, ok := rowAt(m.boardCursor); ok && r.isAgent {
			if m.boardAgentActive(r) {
				return m.beginAgentRestartConfirm(r), nil
			}
			return m.restartBoardAgent(r)
		}
		return m, nil
	case "d":
		if r, ok := rowAt(m.boardCursor); ok && r.isAgent {
			return m.beginAgentCloseConfirm(r), nil
		}
		return m, nil
	case "enter":
		if r, ok := rowAt(m.boardCursor); ok {
			if r.isNew {
				return m.openNewAgentForm(), nil
			}
			return m.focusBoardAgent(r)
		}
		return m, nil
	}
	// A digit jumps straight to that agent (matching the numbers shown).
	if s := msg.String(); len(s) == 1 && s[0] >= '1' && s[0] <= '9' {
		want := int(s[0] - '0')
		for _, ri := range nav {
			if rows[ri].isAgent && rows[ri].num == want {
				return m.focusBoardAgent(rows[ri])
			}
		}
	}
	return m, nil
}

func (m Model) boardAgentActive(r boardRow) bool {
	switch m.agentStatus[r.pid].Status {
	case "busy", "waiting":
		return true
	default:
		return false
	}
}

func (m Model) closeBoardAgent(r boardRow) (tea.Model, tea.Cmd) {
	m.mgr.CloseAgent(r.key, r.agentIdx)
	m.clearAgentTracking(r.pid)
	save := m.saveAgents(r.key)
	_, nav := m.buildBoard()
	m.boardCursor = clamp(m.boardCursor, 0, max(0, len(nav)-1))
	return m, tea.Batch(save, m.flashCmd("closed agent "+r.label))
}

// restartBoardAgent transactionally replaces the selected process in place.
// Manager.ReplaceAgent starts the replacement before swapping it into the pool,
// so a bad command leaves the current conversation alive and focused.
func (m Model) restartBoardAgent(r boardRow) (tea.Model, tea.Cmd) {
	ws, ok := m.mgr.Workspace(r.key)
	if !ok || r.agentIdx < 0 || r.agentIdx >= len(ws.Agents) {
		return m, nil
	}
	old := ws.Agents[r.agentIdx]
	provider := normalizedProvider(old.Provider)
	if provider == agent.Claude && r.key == "~/config" {
		if err := m.ctrl.EnsureHomeDirTrusted(); err != nil {
			m.setError("trust setup error: " + err.Error())
			return m, nil
		}
	}
	var (
		spec session.Spec
		err  error
	)
	if old.SessionID == "" {
		spec, err = m.ctrl.NewProviderAgentSpec(provider, old.Title, "")
	} else {
		spec, err = m.ctrl.RestoreProviderAgentSpec(provider, old.SessionID, old.Title, "")
	}
	if err != nil {
		m.setError("restart error: " + err.Error())
		return m, nil
	}
	spec, err = m.prepareAgentSpec(r.path, spec)
	if err != nil {
		m.setError("restart error: " + err.Error())
		return m, nil
	}
	w, h := m.sessionSize()
	replacement, err := m.mgr.ReplaceAgent(r.key, r.path, r.agentIdx, spec, w, h)
	if err != nil {
		m.setError("restart error: " + err.Error())
		return m, nil
	}
	m.clearAgentTracking(r.pid)
	return m, tea.Batch(m.saveAgents(r.key), m.watchAgentEvents(replacement), m.flashCmd(providerName(provider)+" agent restarted"))
}

// focusBoardAgent navigates directly to an agent's pane: it activates the
// agent's workspace (when it isn't already current), switches to the agent
// screen, focuses the agent, and clears the workspace's unread markers.
func (m Model) focusBoardAgent(r boardRow) (tea.Model, tea.Cmd) {
	if m.current != nil && m.current.key == r.key {
		m.boardOpen = false
		m.screen = screenAgent
		if m.current.ws != nil && r.agentIdx >= 0 && r.agentIdx < len(m.current.ws.Agents) {
			m.current.ws.ActiveAgent = r.agentIdx
		}
		m.clearWorkspaceAttention()
		return m, m.saveAgents(r.key)
	}
	mm, cmd := m.activate(r.repo, r.worktree, r.path)
	model := mm.(Model)
	model.boardOpen = false
	if model.current != nil && model.current.key == r.key {
		model.screen = screenAgent
		if model.current.ws != nil && r.agentIdx >= 0 && r.agentIdx < len(model.current.ws.Agents) {
			model.current.ws.ActiveAgent = r.agentIdx
		}
		model.clearWorkspaceAttention()
		cmd = tea.Batch(cmd, model.saveAgents(r.key))
	}
	return model, cmd
}

// jumpAttention drops the user straight into the session of the agent that most
// needs them, collapsing the board's open→scan→arrow→enter flow — the app's most
// common action — into one reserved chord. The candidate list is every agent
// with pending attention taken in board order, which already floats live-waiting
// worktrees above unread-done ones and sorts the most urgent agent first within
// each group, so buildBoard's ordering is reused rather than re-derived: the
// first candidate is always the most pressing.
//
// Repeated presses walk the queue. When the user is already parked on a
// candidate's agent screen the jump advances to the next candidate (wrapping);
// otherwise it lands on the first. With nothing pending it is a silent no-op —
// no flash, no error — so the key is safe to lean on. The jump itself routes
// through focusBoardAgent so workspace activation, attention-clearing, and
// persistence can never drift from the board's own enter behavior.
func (m Model) jumpAttention() (tea.Model, tea.Cmd) {
	rows, nav := m.buildBoard()
	var candidates []boardRow
	for _, ri := range nav {
		if r := rows[ri]; r.isAgent && r.attn > attnNone {
			candidates = append(candidates, r)
		}
	}
	if len(candidates) == 0 {
		return m, nil
	}
	// Default to the most pressing candidate. When already on the agent screen
	// and the focused agent is itself a candidate, advance past it so successive
	// presses cycle through the queue and wrap.
	target := 0
	if m.screen == screenAgent && m.current != nil && m.current.ws != nil {
		if a := m.current.ws.ActiveAgentSession(); a != nil {
			for i, c := range candidates {
				if c.pid == a.Pid() {
					target = (i + 1) % len(candidates)
					break
				}
			}
		}
	}
	// Land on a bare agent screen: clear any overlay open at the call site so the
	// jump doesn't leave one stacked over the destination. focusBoardAgent resets
	// boardOpen itself; the invocation from handleBoard relies on that.
	m.helpOpen = false
	m.usageOpen = false
	m.diffOpen = false
	return m.focusBoardAgent(candidates[target])
}

// --- command palette ---

// paletteCmd is one executable row of the command palette: a verb-phrase
// title, the live keybinding shown as a dim right-aligned hint (may be
// empty), and the action run when the row is chosen.
type paletteCmd struct {
	title string
	hint  string
	run   func(Model) (tea.Model, tea.Cmd)
}

// paletteCommands builds the palette's row list fresh on each keystroke (cheap,
// mirroring buildBoard's philosophy). Rows are availability-filtered at build
// time: a row appears only when it is executable right now, so the palette never
// offers an action that would no-op. The order groups related actions: open,
// create, view navigation, agents, terminal panes, then the misc/global row.
func (m Model) paletteCommands() []paletteCmd {
	var cmds []paletteCmd

	// 1. Open an existing workspace — one row per active worktree, hinted with
	// the deck's 1/2/3 recency jump digit when it has one.
	for _, it := range m.active {
		repoName, wtName, path := it.repo.Name, it.view.WT.Name, it.view.WT.Path
		hint := ""
		if r := m.recentRank[wsKey(repoName, wtName)]; r >= 1 && r <= 3 {
			hint = strconv.Itoa(r)
		}
		cmds = append(cmds, paletteCmd{
			title: "open " + repoName + "/" + wtName,
			hint:  hint,
			run: func(m Model) (tea.Model, tea.Cmd) {
				return m.activate(repoName, wtName, path)
			},
		})
	}

	// 1b. View a worktree's diff — one row per active worktree, right after the
	// open rows. Running it returns to the deck, moves the active cursor onto
	// that worktree, and opens the diff, so a later `x` from inside the diff
	// operates on the matching selected row.
	for _, it := range m.active {
		key := wsKey(it.repo.Name, it.view.WT.Name)
		cmds = append(cmds, paletteCmd{
			title: "view diff: " + it.repo.Name + "/" + it.view.WT.Name,
			run: func(m Model) (tea.Model, tea.Cmd) {
				idx := -1
				for i, a := range m.active {
					if wsKey(a.repo.Name, a.view.WT.Name) == key {
						idx = i
						break
					}
				}
				if idx < 0 {
					return m, nil // the worktree is gone; no-op
				}
				m.screen = screenPicker
				m.focus = focusActive
				m.activeCursor = idx
				return m.openDiff(m.active[idx])
			},
		})
	}

	// 1c. Stack verbs — one status row per stackable active worktree, plus a
	// restack row when the cached rollup calls for one and a submit row when it
	// carries submit-able work. Running a row opens the stack overlay and issues
	// the matching pipeline call (submit runs directly; restack shows its dry-run
	// plan first).
	for _, it := range m.active {
		p, ok := stackParams(it)
		if !ok {
			continue
		}
		key := wsKey(it.repo.Name, it.view.WT.Name)
		repoName, wtName := it.repo.Name, it.view.WT.Name
		cmds = append(cmds, paletteCmd{
			title: "stack status: " + repoName + "/" + wtName,
			run: func(m Model) (tea.Model, tea.Cmd) {
				m = m.enterStackOverlay(key, repoName, wtName, p)
				m.markStackLoading(key)
				return m, m.fetchStackCmd(key, p)
			},
		})
		e, cached := m.stackInfo[key]
		if !cached || e.loading || e.err != nil || time.Since(e.fetchedAt) >= stackFreshFor {
			continue
		}
		for _, commit := range e.status.Commits {
			if commit.State != stack.StateOpen || commit.PR == nil || commit.PR.URL == "" {
				continue
			}
			url := commit.PR.URL
			title := fmt.Sprintf("open PR #%d: %s/%s — %s", commit.PR.Number, repoName, wtName, commit.Subject)
			cmds = append(cmds, paletteCmd{title: title, run: func(m Model) (tea.Model, tea.Cmd) {
				return m, openURLCmd(url)
			}})
		}
		if e.status.Stack.NextAction == "complete" {
			cmds = append(cmds, paletteCmd{
				title: "archive worktree: " + repoName + "/" + wtName,
				hint:  "all PRs landed",
				run: func(m Model) (tea.Model, tea.Cmd) {
					return m, m.archivePreflightCmd(it, p)
				},
			})
		}
		if stackCanRestack(e.status) {
			title := "restack: " + repoName + "/" + wtName
			reuse := e.status.Stack.NextAction == "complete"
			if reuse {
				title = "reuse worktree: " + repoName + "/" + wtName
			}
			cmds = append(cmds, paletteCmd{
				title: title,
				hint:  stackRestackReason(e.status),
				run: func(m Model) (tea.Model, tea.Cmd) {
					m = m.enterStackOverlay(key, repoName, wtName, p)
					return m, m.restackStackCmd(key, p, true, reuse)
				},
			})
		}
		if stackCanMerge(e.status) {
			cmds = append(cmds, paletteCmd{
				title: "merge PR: " + repoName + "/" + wtName,
				hint:  fmt.Sprintf("#%d", e.status.MergeHint.Number),
				run: func(m Model) (tea.Model, tea.Cmd) {
					m = m.enterStackOverlay(key, repoName, wtName, p)
					m.stackView.working = false
					st := e.status
					m.stackView.status = &st
					return m.requestStackMerge(key, p, st)
				},
			})
		}
		if stackHasSubmitWork(e.status) {
			cmds = append(cmds, paletteCmd{
				title: "submit stack: " + repoName + "/" + wtName,
				run: func(m Model) (tea.Model, tea.Cmd) {
					m = m.enterStackOverlay(key, repoName, wtName, p)
					return m, m.submitStackCmd(key, p)
				},
			})
		}
	}

	// 2. Create a new worktree — one row per known repo. Runs from any screen by
	// returning to the deck first, then entering the same create flow the deck's
	// enter key uses.
	for _, g := range m.groups {
		r := g.Repo
		cmds = append(cmds, paletteCmd{
			title: "new worktree in " + r.Name + "…",
			run: func(m Model) (tea.Model, tea.Cmd) {
				m.screen = screenPicker
				return m.beginCreateFor(r)
			},
		})
	}

	// 3. View navigation — only meaningful with an active workspace.
	if m.current != nil {
		cmds = append(cmds,
			paletteCmd{title: "go to editor", hint: m.keys.GotoEditor, run: func(m Model) (tea.Model, tea.Cmd) { return m.gotoScreen(screenEditor) }},
			paletteCmd{title: "go to agent", hint: m.keys.GotoAgent, run: func(m Model) (tea.Model, tea.Cmd) { return m.gotoScreen(screenAgent) }},
			paletteCmd{title: "go to terminal", hint: m.keys.GotoTerm, run: func(m Model) (tea.Model, tea.Cmd) { return m.gotoScreen(screenTerminal) }},
		)
		if m.screen != screenPicker {
			cmds = append(cmds, paletteCmd{title: "back to deck", hint: m.keys.Picker, run: func(m Model) (tea.Model, tea.Cmd) {
				return m.returnToDeck()
			}})
		}
	}

	// 4. Agents — the attention jump (only while something is pending, mirroring
	// the bar badge), the board, the new-agent launchers, agent rotation (only
	// with a pool worth rotating), and the home workspace.
	if waiting, done := m.attnSummary(); waiting+done > 0 {
		cmds = append(cmds, paletteCmd{title: "jump to waiting agent", hint: m.keys.Attention, run: func(m Model) (tea.Model, tea.Cmd) { return m.jumpAttention() }})
	}
	cmds = append(cmds,
		paletteCmd{title: "open agent board", hint: m.keys.Palette, run: func(m Model) (tea.Model, tea.Cmd) { return m.openBoard() }},
		paletteCmd{title: "new agent…", run: func(m Model) (tea.Model, tea.Cmd) { return m.openNewAgentForm(), nil }},
	)
	rows, _ := m.buildBoard()
	for _, r := range rows {
		if !r.isAgent {
			continue
		}
		identity := r.repo + "/" + r.worktree + " — " + r.label
		target := r
		cmds = append(cmds,
			paletteCmd{title: "restart agent: " + identity, run: func(m Model) (tea.Model, tea.Cmd) {
				m.boardOpen = true
				if m.boardAgentActive(target) {
					return m.beginAgentRestartConfirm(target), nil
				}
				return m.restartBoardAgent(target)
			}},
			paletteCmd{title: "close agent: " + identity, run: func(m Model) (tea.Model, tea.Cmd) {
				m.boardOpen = true
				return m.beginAgentCloseConfirm(target), nil
			}},
		)
	}
	if m.current != nil && m.current.ws != nil && len(m.current.ws.Agents) >= 2 {
		cmds = append(cmds,
			paletteCmd{title: "next agent", hint: m.keys.NextAgent, run: func(m Model) (tea.Model, tea.Cmd) { return m.rotateAgent(+1) }},
			paletteCmd{title: "previous agent", hint: m.keys.PrevAgent, run: func(m Model) (tea.Model, tea.Cmd) { return m.rotateAgent(-1) }},
		)
	}
	cmds = append(cmds, paletteCmd{title: "open home workspace", hint: m.keys.GlobalConfig, run: func(m Model) (tea.Model, tea.Cmd) { return m.activateGlobalConfig() }})

	// 5. Terminal panes — each switches to the terminal screen first, so the
	// palette can split/zoom/close from any screen (strictly more capable than
	// the chords, which are terminal-screen only).
	if m.current != nil {
		cmds = append(cmds,
			paletteCmd{title: "split terminal right", hint: m.keys.TermSplitV, run: func(m Model) (tea.Model, tea.Cmd) {
				m.screen = screenTerminal
				return m.splitTerm(session.SplitV)
			}},
			paletteCmd{title: "split terminal below", hint: m.keys.TermSplitH, run: func(m Model) (tea.Model, tea.Cmd) {
				m.screen = screenTerminal
				return m.splitTerm(session.SplitH)
			}},
			paletteCmd{title: "zoom terminal pane", hint: m.keys.TermZoom, run: func(m Model) (tea.Model, tea.Cmd) {
				m.screen = screenTerminal
				return m.toggleZoom()
			}},
			paletteCmd{title: "close terminal pane", hint: m.keys.TermClose, run: func(m Model) (tea.Model, tea.Cmd) {
				m.screen = screenTerminal
				return m.closeTermPane()
			}},
		)
		ws := m.current.ws
		if ws != nil && len(ws.Terms) > 1 && !ws.TermZoomed {
			w, h := m.sessionSize()
			bounds := session.ComputePaneBounds(ws.TermLayout, 0, 0, w, h)
			for _, item := range []struct {
				name string
				hint string
				dir  session.FocusDir
			}{
				{"left", m.keys.TermFocusLeft, session.FocusLeft},
				{"down", m.keys.TermFocusDown, session.FocusDown},
				{"up", m.keys.TermFocusUp, session.FocusUp},
				{"right", m.keys.TermFocusRight, session.FocusRight},
			} {
				if session.FocusPaneDir(bounds, ws.ActiveTerm, item.dir) < 0 {
					continue
				}
				dir := item.dir
				cmds = append(cmds, paletteCmd{
					title: "focus terminal pane " + item.name, hint: item.hint,
					run: func(m Model) (tea.Model, tea.Cmd) {
						m.screen = screenTerminal
						w, h := m.sessionSize()
						m.mgr.FocusTermPaneDir(m.current.key, dir, w, h)
						return m, nil
					},
				})
			}
		}
	}

	// Lower-frequency expert actions remain searchable without displacing the
	// palette's common open/create/navigation rows from its initial viewport.
	if m.current != nil {
		cmds = append(cmds,
			paletteCmd{title: "cycle view next", hint: m.keys.Cycle, run: func(m Model) (tea.Model, tea.Cmd) {
				m.screen = m.screen.next()
				if m.screen == screenAgent {
					m.clearWorkspaceAttention()
				}
				return m, nil
			}},
			paletteCmd{title: "cycle view previous", hint: m.keys.CycleBack, run: func(m Model) (tea.Model, tea.Cmd) {
				m.screen = m.screen.prev()
				if m.screen == screenAgent {
					m.clearWorkspaceAttention()
				}
				return m, nil
			}},
		)
	}
	for _, it := range m.active {
		key := wsKey(it.repo.Name, it.view.WT.Name)
		identity := it.repo.Name + "/" + it.view.WT.Name
		if it.view.Live {
			cmds = append(cmds, paletteCmd{title: "stop " + identity, run: func(m Model) (tea.Model, tea.Cmd) {
				if !m.selectActiveByKey(key) {
					return m, nil
				}
				selected, _ := m.selectedActive()
				if m.busyHostedAgents(key) > 0 {
					return m.beginStopConfirm(selected.view.WT.Name), nil
				}
				m.stopWorkspace(key)
				return m, tea.Batch(m.loadCmd(), m.flashCmd("stopped "+selected.view.WT.Name))
			}})
		}
		if !it.view.WT.IsMain {
			cmds = append(cmds, paletteCmd{title: "remove " + identity, run: func(m Model) (tea.Model, tea.Cmd) {
				if !m.selectActiveByKey(key) {
					return m, nil
				}
				selected, _ := m.selectedActive()
				return m.beginRemove(selected), nil
			}})
		}
	}

	// 6. Misc / global — always available. Quit sits last and reuses the picker's
	// ctrl+c guard: with a busy hosted agent it drops to the deck and asks first.
	if m.usageEnabled() {
		cmds = append(cmds, paletteCmd{title: "show usage limits", hint: m.keys.Usage, run: func(m Model) (tea.Model, tea.Cmd) {
			m.usageOpen = true
			return m, nil
		}})
	}
	cmds = append(cmds,
		paletteCmd{title: "refresh deck", hint: "r", run: func(m Model) (tea.Model, tea.Cmd) {
			cmds := m.kickStackFetches(true)
			cmds = append(cmds, m.loadCmd())
			return m, tea.Batch(cmds...)
		}},
		paletteCmd{title: "help", hint: m.keys.Help, run: func(m Model) (tea.Model, tea.Cmd) {
			m.helpOpen = true
			return m, nil
		}},
		paletteCmd{title: "quit ct", hint: "ctrl+c", run: func(m Model) (tea.Model, tea.Cmd) {
			if n := m.busyHostedAgents(""); n > 0 {
				m.screen = screenPicker
				m = m.beginQuitConfirm(n)
				return m, nil
			}
			return m, tea.Quit
		}},
	)

	return cmds
}

// filteredPaletteCommands returns the palette rows matching the current query:
// the full list in natural order when the query is empty, otherwise the fuzzy
// matches in score order (best first), mirroring the deck's repo filter.
func (m Model) filteredPaletteCommands() []paletteCmd {
	all := m.paletteCommands()
	q := strings.TrimSpace(m.paletteInput.Value())
	if q == "" {
		return all
	}
	titles := make([]string, len(all))
	for i, c := range all {
		titles[i] = c.title
	}
	res := fuzzy.Find(q, titles)
	out := make([]paletteCmd, 0, len(res))
	for _, mt := range res {
		out = append(out, all[mt.Index])
	}
	return out
}

// openPalette shows the command palette with a fresh empty query and the cursor
// at the top. It closes the board/form/usage overlays first so overlays never
// stack, and focuses the query input.
func (m Model) openPalette() (tea.Model, tea.Cmd) {
	m.paletteOpen = true
	m.boardOpen = false
	m.formOpen = false
	m.usageOpen = false
	m.paletteCursor = 0
	m.paletteInput.SetValue("")
	return m, m.paletteInput.Focus()
}

// closePalette resets the palette to its dormant state (blurred, cleared input).
func (m Model) closePalette() Model {
	m.paletteOpen = false
	m.paletteInput.Blur()
	m.paletteInput.SetValue("")
	return m
}

// handlePalette routes key events while the command palette is open: esc or the
// palette key closes it; up/ctrl+p and down/ctrl+n move the cursor (clamped to
// the filtered list); enter closes the palette and then runs the selected row on
// the post-close model; every other key is typed into the query, resetting the
// cursor to the top whenever the query changed. Up/down never fall through to the
// input.
func (m Model) handlePalette(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc", m.keys.CommandPalette:
		return m.closePalette(), nil
	case "up", "ctrl+p":
		if m.paletteCursor > 0 {
			m.paletteCursor--
		}
		return m, nil
	case "down", "ctrl+n":
		if n := len(m.filteredPaletteCommands()); m.paletteCursor < n-1 {
			m.paletteCursor++
		}
		return m, nil
	case "enter":
		cmds := m.filteredPaletteCommands()
		if m.paletteCursor < 0 || m.paletteCursor >= len(cmds) {
			return m.closePalette(), nil
		}
		sel := cmds[m.paletteCursor]
		return sel.run(m.closePalette())
	}
	before := m.paletteInput.Value()
	var cmd tea.Cmd
	m.paletteInput, cmd = m.paletteInput.Update(msg)
	if m.paletteInput.Value() != before {
		m.paletteCursor = 0
	}
	return m, cmd
}

// activateGlobalConfig opens the home-directory workspace (synthetic key "~/config"),
// starting it fresh on first use and resuming its agent pool on subsequent presses.
func (m Model) activateGlobalConfig() (tea.Model, tea.Cmd) {
	home, err := m.ctrl.GlobalConfigDir()
	if err != nil {
		m.setError("home dir error: " + err.Error())
		return m, nil
	}
	m.homeWSPath = home
	m.homeWSKey = "~/config"
	return m.activate("~", "config", home)
}

// openNewAgentForm switches the board into the new-agent form with defaults:
// active worktree (home when no workspace is active), focus on the prompt
// field. Agents are no longer manually named — claude names the session
// after its topic — so the form opens straight on the prompt.
func (m Model) openNewAgentForm() tea.Model {
	m.boardOpen = true
	m.formOpen = true
	m.formFocus = formFieldPrompt
	m.formProvider = m.defaultAgentProvider
	if !providerIn(m.formProvider, m.agentProviders) {
		m.formProvider = m.agentProviders[0]
	}
	m.formLocation = 0
	if m.current == nil {
		m.formLocation = 1
	}
	m.promptInput.SetValue("")
	m.promptInput.Focus()
	return m
}

// formFields returns the fields in navigation order. The provider row is
// omitted entirely when there is no choice to make, preserving the legacy
// prompt → where traversal.
func (m Model) formFields() []int {
	fields := []int{formFieldPrompt}
	if len(m.agentProviders) > 1 {
		fields = append(fields, formFieldProvider)
	}
	return append(fields, formFieldWhere)
}

// moveFormFocus moves by delta through the visible fields, wrapping, and keeps
// the prompt input's focus state in sync.
func (m Model) moveFormFocus(delta int) (Model, tea.Cmd) {
	fields := m.formFields()
	idx := 0
	for i, field := range fields {
		if field == m.formFocus {
			idx = i
			break
		}
	}
	idx = ((idx+delta)%len(fields) + len(fields)) % len(fields)
	m.formFocus = fields[idx]
	m.promptInput.Blur()
	if m.formFocus == formFieldPrompt {
		return m, m.promptInput.Focus()
	}
	return m, nil
}

func (m *Model) cycleFormProvider(delta int) {
	if len(m.agentProviders) < 2 {
		return
	}
	idx := 0
	for i, provider := range m.agentProviders {
		if provider == m.formProvider {
			idx = i
			break
		}
	}
	idx = ((idx+delta)%len(m.agentProviders) + len(m.agentProviders)) % len(m.agentProviders)
	m.formProvider = m.agentProviders[idx]
}

// handleBoardForm drives the new-agent form. The prompt accepts ordinary
// multi-line editing; tab changes fields and ctrl+enter launches. On toggles,
// enter also launches and ↑/↓ (or ctrl+n/ctrl+p) move between fields.
func (m Model) handleBoardForm(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.formOpen = false
		m.promptInput.Blur()
		return m, nil
	case "ctrl+enter":
		return m.launchAgent()
	case "tab":
		return m.moveFormFocus(1)
	case "shift+tab":
		return m.moveFormFocus(-1)
	}
	if m.formFocus == formFieldPrompt {
		var cmd tea.Cmd
		m.promptInput, cmd = m.promptInput.Update(msg)
		return m, cmd
	}
	switch msg.String() {
	case "down", "ctrl+n":
		return m.moveFormFocus(1)
	case "up", "ctrl+p":
		return m.moveFormFocus(-1)
	case "enter":
		return m.launchAgent()
	}
	switch msg.String() {
	case "left", "right", "h", "l", "space":
		if m.formFocus == formFieldProvider {
			delta := 1
			if msg.String() == "left" || msg.String() == "h" {
				delta = -1
			}
			m.cycleFormProvider(delta)
		} else if m.formFocus == formFieldWhere {
			m.formLocation = 1 - m.formLocation
		}
	}
	return m, nil
}

// Agents the user doesn't name get a random adjective-noun title (e.g.
// "amber-fox") — a stable, recognisable placeholder until a future change
// summarises the conversation into something descriptive.
var promptAdjectives = []string{
	"amber", "bold", "calm", "deft", "eager", "fast", "grand", "hazy",
	"idle", "jade", "keen", "lazy", "mild", "nimble", "odd", "proud",
	"quick", "rare", "shy", "tame", "umber", "vivid", "warm", "young",
	"zany", "brave", "crisp", "dark", "fleet", "grey",
}

var promptNouns = []string{
	"badger", "cedar", "drift", "ember", "falcon", "grove", "haven",
	"inlet", "jasper", "kelp", "lantern", "moth", "nebula", "otter",
	"pine", "quartz", "raven", "stone", "tide", "vale", "willow",
	"fox", "creek", "dune", "fern", "gust", "hawk", "iris", "juniper",
	"kestrel", "larch",
}

func randomAgentTitle() string {
	adj := promptAdjectives[rand.Intn(len(promptAdjectives))]
	noun := promptNouns[rand.Intn(len(promptNouns))]
	return adj + "-" + noun
}

// activate ensures the workspace's sessions are running and switches to the
// last screen used in that worktree (editor on first open). A brand-new worktree
// starts one fresh claude session; reopening resumes the agent pool.
func (m Model) activate(repoName, wtName, dir string) (tea.Model, tea.Cmd) {
	key := repoName + "/" + wtName
	w, h := m.sessionSize()
	wasActive := m.mgr.Has(key)
	var specs []session.Spec
	if !wasActive {
		var err error
		specs, err = m.workspaceSpecs(key, true)
		if err == nil {
			specs, err = m.prepareWorkspaceSpecs(dir, specs)
		}
		if err != nil {
			m.setError("open error: " + err.Error())
			return m, m.loadCmd()
		}
	}
	ws, err := m.mgr.Activate(key, dir, specs, w, h)
	if err != nil {
		m.setError("open error: " + err.Error())
		return m, m.loadCmd()
	}
	if saved := m.savedActiveAgent(key); saved < len(ws.Agents) {
		ws.ActiveAgent = saved
	}
	if m.current != nil && m.screen != screenPicker {
		if m.lastScreens == nil {
			m.lastScreens = map[string]screen{}
		}
		m.lastScreens[m.current.key] = m.screen
	}
	m.current = &workspaceRef{repo: repoName, worktree: wtName, key: key, path: dir, ws: ws}
	if s, ok := m.lastScreens[key]; ok {
		m.screen = s
	} else {
		m.screen = screenEditor
	}
	if m.screen == screenAgent {
		m.clearWorkspaceAttention()
	}
	m.status = ""
	var save tea.Cmd
	if m.state != nil {
		m.state.Touch(key)
		m.persistAgents(key)
		save = m.writeStateCmd()
	}
	var watches tea.Cmd
	if !wasActive {
		watches = m.watchWorkspaceAgents(ws)
	}
	return m, tea.Batch(m.loadCmd(), save, watches)
}

// workspaceSpecs builds the session set for activating key: nvim and a shell
// always, plus the resumed provider-aware agent pool. When addDefault is true,
// an empty pool gets one fresh agent from the configured default provider.
// Launching into an unopened home workspace passes false so the explicitly
// selected agent is the only one created.
func (m Model) workspaceSpecs(key string, addDefault bool) ([]session.Spec, error) {
	specs := []session.Spec{m.ctrl.EditorSpec()}
	var saved []state.AgentState
	if m.state != nil {
		saved, _ = m.state.Agents(key)
	}
	if len(saved) == 0 && addDefault {
		spec, err := m.ctrl.NewProviderAgentSpec(m.defaultAgentProvider, "", "")
		if err != nil {
			return nil, err
		}
		specs = append(specs, spec)
	} else {
		for _, a := range saved {
			provider := normalizedProvider(a.Provider)
			spec, err := m.ctrl.RestoreProviderAgentSpec(provider, a.SessionID, a.Label, "")
			if err != nil {
				return nil, err
			}
			specs = append(specs, spec)
		}
	}
	return append(specs, m.ctrl.TermSpec()), nil
}

// prepareWorkspaceSpecs starts provider companions before their interactive
// sessions. On a partial failure, already-started companions are closed.
func (m Model) prepareWorkspaceSpecs(dir string, specs []session.Spec) ([]session.Spec, error) {
	prepared := append([]session.Spec(nil), specs...)
	for i := range prepared {
		if prepared[i].Kind != session.Agent {
			continue
		}
		var err error
		prepared[i], err = m.prepareAgentSpec(dir, prepared[i])
		if err != nil {
			closeSpecCompanions(prepared)
			return nil, err
		}
	}
	return prepared, nil
}

func (m Model) prepareAgentSpec(dir string, spec session.Spec) (session.Spec, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 7*time.Second)
	defer cancel()
	return m.ctrl.PrepareAgentSpec(ctx, dir, spec)
}

func closeSpecCompanions(specs []session.Spec) {
	for _, spec := range specs {
		if spec.Companion != nil {
			_ = spec.Companion.Close()
		}
	}
}

// savedActiveAgent returns the focused-agent index persisted for key, or 0.
func (m Model) savedActiveAgent(key string) int {
	if m.state == nil {
		return 0
	}
	_, active := m.state.Agents(key)
	return active
}

// persistAgents snapshots a live workspace's agent pool (resume ids, labels, and
// focused index) into ct's state so the next open can rebuild it. The caller is
// responsible for Save(); this only updates the in-memory state.
func (m *Model) persistAgents(key string) {
	if m.state == nil {
		return
	}
	ws, ok := m.mgr.Workspace(key)
	if !ok {
		return
	}
	agents := make([]state.AgentState, 0, len(ws.Agents))
	for _, s := range ws.Agents {
		if s.SessionID == "" {
			continue // not a ct-managed claude session; nothing to resume
		}
		agents = append(agents, state.AgentState{
			Provider: normalizedProvider(s.Provider), SessionID: s.SessionID, Label: s.Title,
		})
	}
	m.state.SetAgents(key, agents, ws.ActiveAgent)
}

// saveAgents snapshots the agent pool for key and returns a command that
// flushes state to disk off the UI goroutine. Call it after any change to the
// pool (spawn, close) or the focused agent, and return the command.
func (m *Model) saveAgents(key string) tea.Cmd {
	if m.state == nil {
		return nil
	}
	m.persistAgents(key)
	return m.writeStateCmd()
}

// writeStateCmd marshals the state in memory (cheap, on the UI goroutine) and
// returns a command that writes it to disk in the background. Superseded
// snapshots are dropped by Snapshot's sequence guard, so rapid-fire saves
// can't roll the file back; a final synchronous flush runs on exit.
func (m Model) writeStateCmd() tea.Cmd {
	if m.state == nil {
		return nil
	}
	sn, ok := m.state.Snapshot()
	if !ok {
		return nil
	}
	return func() tea.Msg {
		_ = sn.Write()
		return nil
	}
}

// FlushState synchronously writes ct's state to disk. main calls it once after
// the program exits so a pending background write can't be lost.
func (m Model) FlushState() {
	if m.state != nil {
		_ = m.state.Save()
	}
}

// --- key handling ---

func (m Model) handleKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if m.screen == screenSetup {
		return m.handleSetupKey(msg)
	}
	// Help is modal and reachable from anywhere (including inside a session). Any
	// key dismisses it. A key that is one of ct's own reserved action keys does
	// double duty: it closes help AND performs its action, so opening help and
	// pressing e.g. the cycle key both dismisses the overlay and cycles — the
	// user shouldn't have to press it twice. We achieve that by closing help then
	// re-dispatching the same message through the normal path. The help key is
	// excluded (its "action" is toggling help, already satisfied by the close),
	// so it never re-opens what it just closed. Every non-reserved key (letters,
	// esc, enter, space, "?") only closes: re-dispatching one would leak the
	// keystroke into the embedded session drawn beneath the overlay, which must
	// never happen.
	if m.helpOpen {
		if s := msg.String(); s == "down" || s == "j" || s == "ctrl+n" {
			m.helpOffset++
			return m, nil
		} else if s == "up" || s == "k" || s == "ctrl+p" {
			m.helpOffset = max(0, m.helpOffset-1)
			return m, nil
		}
		m.helpOpen = false
		m.helpOffset = 0
		if m.isReservedActionKey(msg.String()) {
			return m.handleKey(msg)
		}
		return m, nil
	}
	if msg.String() == m.keys.Help {
		m.helpOpen = true
		m.helpOffset = 0
		return m, nil
	}
	// The command palette is modal and reachable from anywhere (including over the
	// usage/board overlays, so alt+p still opens it while those are up). It owns
	// its own routing while open. Don't open it over a picker modal confirm
	// (create/remove/quit/stop), whose keys must reach that prompt.
	if m.paletteOpen {
		return m.handlePalette(msg)
	}
	if m.mode == modeConfirmMerge || m.mode == modeConfirmTermClose {
		return m.handleGlobalConfirmKey(msg)
	}
	if msg.String() == m.keys.CommandPalette {
		if m.confirmActive() || (m.screen == screenPicker && m.mode != modeNormal) {
			return m, nil
		}
		return m.openPalette()
	}
	// The diff viewer is modal and owns its own routing while open (scroll,
	// scope toggle, and the `x` hop into the remove flow); every other key is
	// swallowed so no deck key leaks through beneath it.
	if m.diffOpen {
		return m.handleDiff(msg)
	}
	// The stack overlay is modal and read-only like the usage overlay: it owns its
	// own routing (scroll, refresh, the restack-plan confirm) while open and
	// swallows every other key so none leaks beneath it.
	if m.stackOpen {
		return m.handleStack(msg)
	}
	// The usage overlay is modal like the board, but read-only: it only closes
	// on esc or its own key, and swallows everything else so no keystroke
	// leaks into the session underneath.
	if m.usageOpen {
		if s := msg.String(); s == "esc" || s == m.keys.Usage {
			m.usageOpen = false
		}
		return m, nil
	}
	if m.boardOpen {
		return m.handleBoard(msg)
	}
	if msg.String() == m.keys.Palette {
		return m.openBoard()
	}
	// The attention-jump chord fires from session screens and from the picker.
	// It sits below the modal overlays handled above (palette/diff/usage/board),
	// so those keep their precedence. jumpAttention is a no-op when nothing is
	// pending.
	if msg.String() == m.keys.Attention {
		return m.jumpAttention()
	}
	if msg.String() == m.keys.GlobalConfig {
		return m.activateGlobalConfig()
	}
	// Direct-jump keys work from session screens and from the picker whenever a
	// workspace is active; they no-op with none.
	switch msg.String() {
	case m.keys.GotoEditor:
		return m.gotoScreen(screenEditor)
	case m.keys.GotoAgent:
		return m.gotoScreen(screenAgent)
	case m.keys.GotoTerm:
		return m.gotoScreen(screenTerminal)
	}
	if m.screen != screenPicker {
		return m.handleSessionKey(msg)
	}
	return m.handlePicker(msg)
}

// gotoScreen jumps straight to a session view, mirroring selectTab's semantics:
// a no-op without an active workspace, and clearing the workspace's attention
// markers when landing on the agent screen.
func (m Model) gotoScreen(s screen) (tea.Model, tea.Cmd) {
	if m.current == nil {
		return m, nil
	}
	m.screen = s
	if s == screenAgent {
		m.clearWorkspaceAttention()
	}
	return m, nil
}

// isReservedActionKey reports whether s is one of ct's own reserved keys that
// should still fire when it dismisses the help overlay (see handleKey). The
// globally-reserved keys apply on every screen; keyHelp is intentionally absent
// (dismissing help already satisfies its toggle, and re-dispatching it would
// re-open the overlay). The usage key is only reserved on the agent screen and
// the terminal-pane keys only on the terminal screen — elsewhere they are
// ordinary session input, so treating them as reserved would forward the key
// into the session beneath, exactly the leak the help swallow prevents.
func (m Model) isReservedActionKey(s string) bool {
	switch s {
	case m.keys.Cycle, m.keys.CycleBack, m.keys.Picker, m.keys.Palette,
		m.keys.Attention, m.keys.GlobalConfig, m.keys.NextAgent, m.keys.PrevAgent,
		m.keys.CommandPalette, m.keys.GotoEditor, m.keys.GotoAgent, m.keys.GotoTerm:
		return true
	}
	if s == m.keys.Usage && m.screen == screenAgent && m.usageEnabled() {
		return true
	}
	if m.screen == screenTerminal {
		switch s {
		case m.keys.TermSplitV, m.keys.TermSplitH, m.keys.TermZoom, m.keys.TermClose,
			m.keys.TermFocusLeft, m.keys.TermFocusDown, m.keys.TermFocusUp, m.keys.TermFocusRight:
			return true
		}
	}
	return false
}

func (m Model) handleSetupKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "ctrl+c", "esc":
		return m, tea.Quit
	case "enter":
		root := strings.TrimSpace(m.rootInput.Value())
		if root == "" {
			return m, nil
		}
		abs, err := config.ResolveRoot(root)
		if err != nil {
			m.setError(err.Error())
			return m, nil
		}
		if err := config.Save(m.configPath, abs); err != nil {
			m.setError("save error: " + err.Error())
			return m, nil
		}
		m.ctrl.SetRoot(abs)
		m.screen = screenPicker
		flashTick := m.flashCmd("config saved — welcome to caretaker!")
		cmds := []tea.Cmd{m.loadCmd(), m.pollStatusCmd(), flashTick}
		if m.usageEnabled() {
			cmds = append(cmds, m.pollUsageCmds(), m.scheduleUsageTick())
		}
		return m, tea.Batch(cmds...)
	}
	var cmd tea.Cmd
	m.rootInput, cmd = m.rootInput.Update(msg)
	m.status = ""
	return m, cmd
}

func (m Model) handleSessionKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	// The usage key is only reserved on the agent screen (where the gauge
	// lives); everywhere else it stays an ordinary session key and is
	// forwarded below.
	if msg.String() == m.keys.Usage && m.screen == screenAgent && m.usageEnabled() {
		m.usageOpen = true
		return m, nil
	}
	switch msg.String() {
	case m.keys.Cycle:
		m.screen = m.screen.next()
		if m.screen == screenAgent && m.current != nil {
			m.clearWorkspaceAttention()
		}
		return m, nil
	case m.keys.CycleBack:
		m.screen = m.screen.prev()
		if m.screen == screenAgent && m.current != nil {
			m.clearWorkspaceAttention()
		}
		return m, nil
	case m.keys.Picker:
		return m.returnToDeck()
	case m.keys.NextAgent:
		return m.rotateAgent(+1)
	case m.keys.PrevAgent:
		return m.rotateAgent(-1)
	}
	if m.screen == screenTerminal && m.current != nil {
		key := m.current.key
		w, h := m.sessionSize()
		switch msg.String() {
		case m.keys.TermSplitV:
			return m.splitTerm(session.SplitV)
		case m.keys.TermSplitH:
			return m.splitTerm(session.SplitH)
		case m.keys.TermFocusLeft:
			m.mgr.FocusTermPaneDir(key, session.FocusLeft, w, h)
			return m, nil
		case m.keys.TermFocusDown:
			m.mgr.FocusTermPaneDir(key, session.FocusDown, w, h)
			return m, nil
		case m.keys.TermFocusUp:
			m.mgr.FocusTermPaneDir(key, session.FocusUp, w, h)
			return m, nil
		case m.keys.TermFocusRight:
			m.mgr.FocusTermPaneDir(key, session.FocusRight, w, h)
			return m, nil
		case m.keys.TermZoom:
			return m.toggleZoom()
		case m.keys.TermClose:
			return m.closeTermPane()
		}
	}

	if s := m.activeSession(); s != nil {
		m = m.dismissHint()
		if normalizedProvider(s.Provider) == agent.Codex {
			switch msg.String() {
			case "ctrl+t":
				m.codexTranscript[s] = !m.codexTranscript[s]
			case "esc":
				delete(m.codexTranscript, s)
			}
		}
		s.SendKey(toUVKey(msg))
	}
	return m, nil
}

// returnToDeck leaves the active session for the picker: it records the current
// workspace's last-viewed screen (so reopening lands there), switches to the
// deck, focuses the active list when it's non-empty, and refreshes the deck —
// dirty markers and worktree lists go stale while you edit inside a session, and
// this is the moment they're re-read. Shared by handleSessionKey's picker branch
// and the command palette's "back to deck" row.
func (m Model) returnToDeck() (tea.Model, tea.Cmd) {
	if m.current != nil {
		if m.lastScreens == nil {
			m.lastScreens = map[string]screen{}
		}
		m.lastScreens[m.current.key] = m.screen
	}
	m.screen = screenPicker
	if len(m.active) > 0 {
		m.focus = focusActive
	}
	return m, m.loadCmd()
}

// splitTerm spawns a new terminal pane splitting the current workspace's active
// pane in dir, then resizes the layout to fit. Shared by the terminal-screen
// split keys and the palette's split rows. A no-op without a current workspace.
func (m Model) splitTerm(dir session.SplitDir) (tea.Model, tea.Cmd) {
	if m.current == nil {
		return m, nil
	}
	key := m.current.key
	w, h := m.sessionSize()
	_, _ = m.mgr.SplitTermPane(key, m.current.path, m.ctrl.TermSpec(), dir, w, h)
	m.mgr.ResizeTermPanes(key, w, h)
	return m, nil
}

// closeTermPane closes the current workspace's active terminal pane and resizes
// the remaining layout. Shared by the terminal-screen close key and the
// palette's close row. A no-op without a current workspace.
func (m Model) closeTermPane() (tea.Model, tea.Cmd) {
	if m.current == nil {
		return m, nil
	}
	ws := m.current.ws
	if ws == nil {
		return m, nil
	}
	pane := ws.ActiveTermSession()
	if pane != nil && m.termPaneBusy != nil && m.termPaneBusy(pane) {
		return m.beginTermCloseConfirm(), nil
	}
	return m.closeTermPaneNow()
}

func (m Model) closeTermPaneNow() (tea.Model, tea.Cmd) {
	if m.current == nil {
		return m, nil
	}
	key := m.current.key
	w, h := m.sessionSize()
	_ = m.mgr.CloseTermPane(key)
	m.mgr.ResizeTermPanes(key, w, h)
	return m, nil
}

// rotateAgent moves the focused agent by delta (wrapping) and switches to the
// agent view. It's a no-op without at least two agents.
func (m Model) rotateAgent(delta int) (tea.Model, tea.Cmd) {
	if m.current == nil || m.current.ws == nil {
		return m, nil
	}
	n := len(m.current.ws.Agents)
	if n == 0 {
		return m, nil
	}
	m.current.ws.ActiveAgent = ((m.current.ws.ActiveAgent+delta)%n + n) % n
	m.screen = screenAgent
	m.clearWorkspaceAttention()
	return m, m.saveAgents(m.current.key)
}

// launchAgent spawns a new agent from the board's new-agent form, honouring the
// selected location (active/home worktree).
func (m Model) launchAgent() (tea.Model, tea.Cmd) {
	prompt := strings.TrimSpace(m.promptInput.Value())
	// The form no longer takes a label; every agent gets an auto-generated
	// placeholder title until a future change summarises it.
	label := randomAgentTitle()
	m.formOpen = false
	m.boardOpen = false
	m.promptInput.Blur()
	w, h := m.sessionSize()
	var restoredWatches tea.Cmd

	const homeKey = "~/config"
	isHome := m.formLocation == 1
	provider := normalizedProvider(m.formProvider)
	spec, err := m.ctrl.NewProviderAgentSpec(provider, label, prompt)
	if err != nil {
		m.setError("spawn error: " + err.Error())
		return m, nil
	}

	// Resolve the target workspace's (key, path). The home workspace is
	// synthetic: before it can host an agent it must be opened, trusted, and
	// recorded, exactly as a first activation would do.
	var key, path string
	if !isHome {
		if m.current == nil {
			return m, m.flashCmd("no active workspace")
		}
		key, path = m.current.key, m.current.path
	} else {
		home, err := m.ctrl.GlobalConfigDir()
		if err != nil {
			m.setError("home dir error: " + err.Error())
			return m, nil
		}
		m.homeWSPath = home
		m.homeWSKey = homeKey
		if provider == agent.Claude {
			if err := m.ctrl.EnsureHomeDirTrusted(); err != nil {
				m.setError("trust setup error: " + err.Error())
				return m, nil
			}
		}
		// Restore any saved pool, but don't synthesize a default agent when the
		// home workspace has never been opened: the selected provider below is
		// the requested first agent.
		specs, err := m.workspaceSpecs(homeKey, false)
		if err != nil {
			m.setError("open error: " + err.Error())
			return m, nil
		}
		homeWasActive := m.mgr.Has(homeKey)
		if !homeWasActive {
			specs, err = m.prepareWorkspaceSpecs(home, specs)
			if err != nil {
				m.setError("open error: " + err.Error())
				return m, nil
			}
		}
		homeWS, err := m.mgr.Activate(homeKey, home, specs, w, h)
		if err != nil {
			m.setError("open error: " + err.Error())
			return m, nil
		}
		if !homeWasActive {
			restoredWatches = m.watchWorkspaceAgents(homeWS)
		}
		key, path = homeKey, home
	}

	spec, err = m.prepareAgentSpec(path, spec)
	if err != nil {
		m.setError("spawn error: " + err.Error())
		return m, nil
	}
	spawned, err := m.mgr.SpawnAgent(key, path, spec, w, h)
	if err != nil {
		m.setError("spawn error: " + err.Error())
		return m, nil
	}

	// Home navigates through activate(), which rebuilds the pane pool from
	// persisted state — so persist the new agent first, then hand off.
	if isHome {
		m.persistAgents(homeKey)
		mm, cmd := m.activate("~", "config", path)
		model := mm.(Model)
		model.screen = screenAgent
		return model, tea.Batch(cmd, restoredWatches, model.watchAgentEvents(spawned), model.flashCmd("agent launched"))
	}

	save := m.saveAgents(key)
	m.screen = screenAgent
	m.clearWorkspaceAttention()
	return m, tea.Batch(save, restoredWatches, m.watchAgentEvents(spawned), m.flashCmd("agent launched"))
}

// handleMouseClick switches tabs when a left-click lands on a bar icon, focuses
// the terminal pane under a click in a split layout, and otherwise forwards the
// click to the active session.
func (m Model) handleMouseClick(msg tea.MouseClickMsg) (tea.Model, tea.Cmd) {
	if m.overlayOpen() {
		return m, nil
	}
	mo := msg.Mouse()
	if mo.Button == tea.MouseLeft {
		if s, ok := m.tabAt(mo.X, mo.Y); ok {
			return m.selectTab(s)
		}
		if m.notifZoneAt(mo.X, mo.Y) {
			// The badge is a one-click shortcut to the agent that needs attention,
			// the same destination (and cycling) as the attention-jump chord.
			return m.jumpAttention()
		}
		if m.usageZoneAt(mo.X, mo.Y) {
			m.usageOpen = true
			return m, nil
		}
		if m.paneZoomAt(mo.X, mo.Y) {
			return m.toggleZoom()
		}
		if m.screen == screenPicker && !m.helpOpen && !m.paletteOpen && !m.diffOpen && !m.stackOpen {
			if mm, cmd, ok := m.deckClick(mo.X, mo.Y); ok {
				return mm, cmd
			}
		}
		// Focus the terminal pane under the click before the click reaches the
		// program running in it. Panes aren't drawn while an overlay is up, so a
		// click then must not steal pane focus.
		if m.screen == screenTerminal && m.current != nil &&
			!m.helpOpen && !m.boardOpen && !m.usageOpen && !m.paletteOpen && !m.diffOpen && mo.Y >= barHeight {
			w, h := m.sessionSize()
			m.mgr.FocusTermPaneAt(m.current.key, mo.X, mo.Y-barHeight, w, h)
		}
	}
	m.forwardMouse(msg)
	return m, nil
}

// deckWheel scrolls the deck section under the pointer: the wheel moves that
// section's cursor (focusing the section, mirroring what a click does), so
// the lists scroll the way every other list on screen does.
func (m Model) deckWheel(msg tea.MouseWheelMsg) (tea.Model, tea.Cmd) {
	mo := msg.Mouse()
	delta := 1
	if mo.Button == tea.MouseWheelUp {
		delta = -1
	} else if mo.Button != tea.MouseWheelDown {
		return m, nil
	}
	if m.mode != modeNormal {
		return m, nil
	}

	by := mo.Y - barHeight
	L := m.deckLayout(m.height - barHeight)
	switch {
	case by >= 1 && by < 1+L.newContentH: // NEW box
		var cmd tea.Cmd
		if m.focus != focusNew {
			m.focus = focusNew
			cmd = m.filter.Focus()
		}
		m.newCursor = clamp(m.newCursor+delta, 0, max(0, len(m.repoMatches)-1))
		return m, cmd
	case by >= L.newOuterH+1 && by < L.newOuterH+1+L.activeContentH: // ACTIVE box
		m.focus = focusActive
		m.filter.Blur()
		m.activeCursor = clamp(m.activeCursor+delta, 0, max(0, len(m.active)-1))
		return m, nil
	}
	return m, nil
}

// deckClick maps a left-click in the deck to the NEW repo row or ACTIVE worktree
// row under it. A click selects (and focuses) the row; clicking the
// already-selected row activates it — opening a worktree or starting the create
// flow, mirroring enter. handled is false when the click misses every row.
func (m Model) deckClick(x, y int) (tea.Model, tea.Cmd, bool) {
	by := y - barHeight // body-relative row (bar + separator sit above)
	if by < 0 {
		return m, nil, false
	}
	// The NEW/ACTIVE boxes only span the left column; clicks on the plasma
	// panel to their right select nothing.
	if x >= m.width-m.plasmaWidth() {
		return m, nil, false
	}
	L := m.deckLayout(m.height - barHeight)

	// NEW box: content rows are body [1, 1+newContentH); the repo list begins at
	// content line 4 (header, blank, input, blank).
	if by >= 1 && by < 1+L.newContentH {
		if m.mode == modeCreateName {
			return m, nil, false
		}
		cl := by - 1
		if cl < 4 {
			return m, nil, false
		}
		start, end := windowBounds(len(m.repoMatches), m.newCursor, L.newRows)
		idx := start + (cl - 4)
		if idx < start || idx >= end || idx >= len(m.repoMatches) {
			return m, nil, false
		}
		reselect := m.focus == focusNew && m.newCursor == idx
		m.newCursor = idx
		if reselect {
			mm, cmd := m.beginCreate()
			return mm, cmd, true
		}
		var cmd tea.Cmd
		if m.focus != focusNew {
			m.focus = focusNew
			cmd = m.filter.Focus()
		}
		return m, cmd, true
	}

	// ACTIVE box: content rows are body [newOuterH+1, …); worktree rows begin at
	// content line 2 (header, blank).
	top := L.newOuterH
	if by >= top+1 && by < top+1+L.activeContentH {
		cl := by - (top + 1)
		if cl < 2 {
			return m, nil, false
		}
		display, rowItem := m.activeDisplay(m.width - m.plasmaWidth() - 4)
		start, end := activeWindowStart(rowItem, m.activeCursor, L.activeRows)
		di := start + (cl - 2)
		if di < start || di >= end || di >= len(display) {
			return m, nil, false
		}
		item := rowItem[di]
		if item < 0 {
			return m, nil, false // a repo header row
		}
		reselect := m.focus == focusActive && m.activeCursor == item
		m.focus = focusActive
		m.activeCursor = item
		m.filter.Blur()
		if reselect {
			if it, ok := m.selectedActive(); ok {
				mm, cmd := m.activate(it.repo.Name, it.view.WT.Name, it.view.WT.Path)
				return mm, cmd, true
			}
		}
		return m, nil, true
	}

	return m, nil, false
}

// selectTab activates a clicked bar tab: the picker is always reachable; the
// session tabs only switch when a workspace is active. Switching to the
// picker refreshes the deck, matching the picker key.
func (m Model) selectTab(s screen) (tea.Model, tea.Cmd) {
	if s == screenPicker || m.current != nil {
		wasSession := m.screen != screenPicker
		m.screen = s
		if s == screenAgent && m.current != nil {
			m.clearWorkspaceAttention()
		}
		if s == screenPicker && wasSession {
			return m, m.loadCmd()
		}
	}
	return m, nil
}

// forwardMouse relays a mouse event to the session under the pointer. In an
// unzoomed split terminal layout it hit-tests the pane rectangles and delivers
// pane-local coordinates to that pane's program, so a click or wheel scroll acts
// on the pane it lands on rather than only the focused one; events over a
// divider or the footer are dropped. Everywhere else it relays to the active
// session translated below the bar. The emulator only encodes the event if the
// program has enabled mouse reporting.
func (m Model) forwardMouse(msg tea.MouseMsg) {
	if m.screen == screenPicker {
		return
	}
	// A full-body overlay (help/board/usage/palette) covers the session, so no
	// mouse event may reach it. Only the palette needs the explicit guard here —
	// help/board/usage are handled by the callers — but returning early keeps
	// every event out of the session drawn beneath the palette.
	if m.paletteOpen {
		return
	}
	mo := msg.Mouse()
	if mo.Y < barHeight {
		return
	}
	// Target session and the pane origin to subtract from the pointer. Default:
	// the active session, offset only by the bar (bx, by stay 0).
	s := m.activeSession()
	bx, by := 0, 0
	if m.screen == screenTerminal && m.current != nil && m.current.ws != nil {
		ws := m.current.ws
		if !ws.TermZoomed && len(ws.Terms) > 1 && ws.TermLayout != nil {
			w, h := m.sessionSize()
			bounds := session.ComputePaneBounds(ws.TermLayout, 0, 0, w, h)
			idx := session.PaneAt(bounds, mo.X, mo.Y-barHeight)
			if idx < 0 || idx >= len(ws.Terms) || ws.Terms[idx] == nil {
				return // divider, footer, or outside every pane
			}
			for _, b := range bounds {
				if b.Idx == idx {
					bx, by = b.X, b.Y
					break
				}
			}
			s = ws.Terms[idx]
		}
	}
	if s == nil {
		return
	}
	shift := func(p tea.Mouse) uv.Mouse {
		return uv.Mouse{X: p.X - bx, Y: p.Y - barHeight - by, Button: p.Button, Mod: uv.KeyMod(p.Mod)}
	}
	switch e := msg.(type) {
	case tea.MouseClickMsg:
		s.SendMouse(uv.MouseClickEvent(shift(e.Mouse())))
	case tea.MouseReleaseMsg:
		s.SendMouse(uv.MouseReleaseEvent(shift(e.Mouse())))
	case tea.MouseWheelMsg:
		if normalizedProvider(s.Provider) == agent.Codex && m.codexTranscript[s] {
			if seq, ok := codexTranscriptWheel(e.Mouse().Button); ok {
				_, _ = s.WriteInput([]byte(seq))
				return
			}
		}
		s.SendMouse(uv.MouseWheelEvent(shift(e.Mouse())))
	case tea.MouseMotionMsg:
		s.SendMouse(uv.MouseMotionEvent(shift(e.Mouse())))
	}
}

// codexTranscriptWheel translates a wheel direction into Codex's default
// transcript-pager bindings. Ultraviolet's terminal emulator cannot encode
// modified cursor keys, so callers write the standard xterm sequences directly
// to the hosted PTY.
func codexTranscriptWheel(button tea.MouseButton) (string, bool) {
	switch button {
	case tea.MouseWheelUp:
		return "\x1b[1;2A", true // Shift+Up
	case tea.MouseWheelDown:
		return "\x1b[1;2B", true // Shift+Down
	default:
		return "", false
	}
}

func (m Model) handlePicker(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch m.mode {
	case modeCreateName:
		return m.handleCreateKey(msg)
	case modeConfirmRemove:
		return m.handleConfirmKey(msg)
	case modeConfirmArchive:
		return m.handleConfirmArchiveKey(msg)
	case modeConfirmQuit:
		return m.handleConfirmQuitKey(msg)
	case modeConfirmStop:
		return m.handleConfirmStopKey(msg)
	}
	if msg.String() == "ctrl+c" {
		// Quitting runs mgr.CloseAll(), which SIGKILLs every hosted pty — every
		// agent and nvim. Guard it only when a hosted agent is mid-task; the
		// common (nothing busy) case quits with no friction.
		if n := m.busyHostedAgents(""); n > 0 {
			m = m.beginQuitConfirm(n)
			return m, nil
		}
		return m, tea.Quit
	}
	// "?" is a convenient deck-only alias for the help key; the picker owns its
	// input, so intercepting it before the filter is safe.
	if msg.String() == "?" {
		m.helpOpen = true
		return m, nil
	}
	// The picker key jumps straight to the most recently activated worktree, so
	// it toggles: session -> picker -> back to the most recent work.
	if msg.String() == m.keys.Picker {
		if r, w, p, ok := m.mostRecentWorktree(); ok {
			return m.activate(r, w, p)
		}
		return m, nil
	}
	if msg.String() == "tab" {
		return m.toggleFocus()
	}
	if m.focus == focusNew {
		return m.handleNewKey(msg)
	}
	return m.handleActiveKey(msg)
}

func (m Model) toggleFocus() (tea.Model, tea.Cmd) {
	if m.focus == focusNew {
		m.focus = focusActive
		m.filter.Blur()
		return m, nil
	}
	m.focus = focusNew
	return m, m.filter.Focus()
}

func (m Model) handleNewKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "up", "ctrl+p":
		if m.newCursor > 0 {
			m.newCursor--
		}
		return m, nil
	case "down", "ctrl+n":
		// j/k are left to fall through to the fuzzy filter, which owns this
		// section's input.
		if m.newCursor < len(m.repoMatches)-1 {
			m.newCursor++
		}
		return m, nil
	case "enter":
		return m.beginCreate()
	case "esc":
		m.filter.SetValue("")
		m.recomputeMatches()
		return m, nil
	}

	var cmd tea.Cmd
	m.filter, cmd = m.filter.Update(msg)
	m.recomputeMatches()
	return m, cmd
}

// beginCreate enters the new-worktree naming form for the repo under newCursor.
func (m Model) beginCreate() (tea.Model, tea.Cmd) {
	if m.newCursor < 0 || m.newCursor >= len(m.repoMatches) {
		return m, nil
	}
	return m.beginCreateFor(m.repoMatches[m.newCursor])
}

// beginCreateFor enters the new-worktree naming form for repo r. Shared by
// beginCreate (which resolves r from newCursor) and the command palette's
// "new worktree in <repo>…" rows, so both set up identical form state.
func (m Model) beginCreateFor(r repo.Repo) (tea.Model, tea.Cmd) {
	m.pendingRepo = r
	m.mode = modeCreateName
	m.filter.Blur()
	m.nameInput.SetValue("")
	m.status = "new worktree in " + r.Name
	return m, m.nameInput.Focus()
}

func (m Model) handleActiveKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	// Shared list-nav dialect: arrows and ctrl+p/ctrl+n move, and — because the
	// ACTIVE list owns no text input — j/k do too.
	case "up", "k", "ctrl+p":
		if m.activeCursor > 0 {
			m.activeCursor--
		}
	case "down", "j", "ctrl+n":
		if m.activeCursor < len(m.active)-1 {
			m.activeCursor++
		}
	case "r":
		m.status = ""
		// Re-kick the stack cache too, ignoring the freshness window, so an
		// explicit refresh reflects fresh gh/git state alongside the deck reload.
		cmds := m.kickStackFetches(true)
		cmds = append(cmds, m.loadCmd())
		return m, tea.Batch(cmds...)
	case "enter":
		if it, ok := m.selectedActive(); ok {
			return m.activate(it.repo.Name, it.view.WT.Name, it.view.WT.Path)
		}
	case "d":
		if it, ok := m.selectedActive(); ok {
			key := wsKey(it.repo.Name, it.view.WT.Name)
			// Stopping hard-kills this workspace's sessions. Guard it when the
			// worktree has a busy agent; otherwise stop instantly as before.
			if m.busyHostedAgents(key) > 0 {
				m = m.beginStopConfirm(it.view.WT.Name)
				return m, nil
			}
			m.stopWorkspace(key)
			return m, tea.Batch(m.loadCmd(), m.flashCmd("stopped "+it.view.WT.Name))
		}
	case "v":
		// Open the read-only diff of everything this branch carries beyond main.
		if it, ok := m.selectedActive(); ok {
			return m.openDiff(it)
		}
	case "s":
		// Open the stack screen for a stackable worktree, kicking a fresh status
		// fetch that lands in both the cache and the open screen.
		if it, ok := m.selectedActive(); ok {
			if p, pok := stackParams(it); pok {
				key := wsKey(it.repo.Name, it.view.WT.Name)
				m = m.enterStackOverlay(key, it.repo.Name, it.view.WT.Name, p)
				m.markStackLoading(key)
				return m, m.fetchStackCmd(key, p)
			}
		}
	case "x":
		if it, ok := m.selectedActive(); ok && !it.view.WT.IsMain {
			m = m.beginRemove(it)
		}
	case "1", "2", "3":
		// The deck badges these ranks next to the most recently opened
		// worktrees; pressing the digit jumps straight there.
		if it, ok := m.rankedActive(int(msg.String()[0] - '0')); ok {
			return m.activate(it.repo.Name, it.view.WT.Name, it.view.WT.Path)
		}
	}
	return m, nil
}

// beginRemove enters the remove-worktree confirm panel for it. The panel is
// dirty-aware (git worktree remove --force discards uncommitted work silently,
// so a dirty tree gets an explicit red warning above the destructive option),
// and its cursor starts on the safe "keep branch" row so the reflexive enter
// keeps the branch. Both the deck's `x` key and the diff viewer's `x` (the
// review loop) route through here so the two prompts can never drift. The
// caller guards against the main worktree, which can't be removed.
func (m Model) beginRemove(it activeItem) Model {
	m.mode = modeConfirmRemove
	m.status = ""
	m.confirm = confirmState{
		title:   "REMOVE WORKTREE",
		context: removeConfirmContext(it),
		options: []confirmOption{
			{label: "remove worktree, keep branch", key: "b"},
			{label: "remove worktree + delete branch", key: "y", danger: true},
			{label: "view diff first", key: "v"},
			{label: "cancel", key: "esc"},
		},
	}
	return m
}

func (m Model) beginArchive(msg archivePreflightMsg) Model {
	it := msg.it
	it.view.Dirty = msg.dirty
	context := removeConfirmContext(it)
	context = append([]string{
		repoHdrStyle.Render(it.repo.Name + " / " + it.view.WT.Name),
		dimStyle.Render("all stack PRs landed · archive removes this worktree and local branch"),
	}, context[1:]...)
	if it.view.Dirty {
		context = append(context, errStyle.Render("✷ untracked files will also be lost"))
	}
	m.screen = screenPicker
	m.stackOpen = false
	m.stackView = stackView{}
	m.mode = modeConfirmArchive
	m.status = ""
	m.confirm = confirmState{
		title:       "ARCHIVE WORKTREE",
		context:     context,
		fingerprint: msg.fingerprint,
		target:      &it,
		options: []confirmOption{
			{label: "cancel", key: "esc"},
			{label: "view diff first", key: "v"},
			{label: "archive worktree + delete local branch", key: "y", danger: true},
		},
	}
	return m
}

// beginQuitConfirm enters the quit guard's confirm panel, warning that n busy
// hosted agents are still running. The cursor starts on the safe "cancel" row.
func (m Model) beginQuitConfirm(n int) Model {
	m.mode = modeConfirmQuit
	m.status = ""
	m.confirm = confirmState{
		title:   "QUIT CT",
		context: []string{dimStyle.Render(fmt.Sprintf("%d busy agent(s) still running — quitting kills them", n))},
		options: []confirmOption{
			{label: "cancel", key: "esc"},
			{label: "quit anyway", key: "y", danger: true},
		},
	}
	return m
}

// beginStopConfirm enters the stop guard's confirm panel for the named
// worktree, whose busy agent stopping would kill. The cursor starts on the safe
// "cancel" row.
func (m Model) beginStopConfirm(name string) Model {
	m.mode = modeConfirmStop
	m.status = ""
	m.confirm = confirmState{
		title:   "STOP WORKSPACE",
		context: []string{dimStyle.Render(fmt.Sprintf("%q has a busy agent — stopping kills it", name))},
		options: []confirmOption{
			{label: "cancel", key: "esc"},
			{label: "stop anyway", key: "y", danger: true},
		},
	}
	return m
}

func (m Model) beginAgentCloseConfirm(r boardRow) Model {
	m.mode = modeConfirmAgentClose
	m.status = ""
	target := r
	context := agentConfirmContext(r)
	if m.boardAgentActive(r) {
		context = append(context, errStyle.Render("! current work will be interrupted"))
	}
	m.confirm = confirmState{
		title:   "CLOSE AGENT",
		context: context,
		options: []confirmOption{
			{label: "keep agent", key: "esc"},
			{label: "close agent", key: "d", danger: true},
		},
		agent: &target,
	}
	return m
}

func (m Model) beginAgentRestartConfirm(r boardRow) Model {
	m.mode = modeConfirmAgentRestart
	m.status = ""
	target := r
	context := append(agentConfirmContext(r),
		errStyle.Render("! restarting will interrupt the current turn"),
		dimStyle.Render("provider, conversation, and pool position will be preserved"),
	)
	m.confirm = confirmState{
		title:   "RESTART AGENT",
		context: context,
		options: []confirmOption{
			{label: "keep running", key: "esc"},
			{label: "restart agent", key: "r", danger: true},
		},
		agent: &target,
	}
	return m
}

// requestStackMerge either executes immediately for the explicit auto_merge
// opt-in or opens the shared destructive-action confirmation panel.
func (m Model) requestStackMerge(key string, p stack.Params, st stack.StackStatus) (tea.Model, tea.Cmd) {
	if m.stackAutoMerge {
		m.stackView.working = true
		return m, m.mergeStackCmd(key, p)
	}
	return m.beginMergeConfirm(key, p, st), nil
}

func (m Model) beginMergeConfirm(key string, p stack.Params, st stack.StackStatus) Model {
	hint := st.MergeHint
	if hint == nil {
		return m
	}
	base := st.MainBranch
	if base == "" {
		base = "base branch"
	}
	context := []string{
		repoHdrStyle.Render(st.Repo + " / " + st.Worktree),
		nameStyle.Render(fmt.Sprintf("PR #%d · %s", hint.Number, hint.Subject)),
		dimStyle.Render("squash merge into " + base),
	}
	for _, c := range st.Commits {
		if c.PR != nil && c.PR.Number == hint.Number {
			checks := c.PR.Checks.Summary
			if checks == "" {
				checks = "unknown"
			}
			context = append(context, dimStyle.Render("checks: "+checks+" · mergeable: "+strings.ToLower(c.PR.Mergeable)))
			break
		}
	}
	target := stackMergeTarget{key: key, params: p}
	m.mode = modeConfirmMerge
	m.confirm = confirmState{
		title:   "MERGE PR",
		context: context,
		options: []confirmOption{
			{label: "cancel", key: "esc"},
			{label: fmt.Sprintf("merge PR #%d into %s", hint.Number, base), key: "m", danger: true},
		},
		merge: &target,
	}
	return m
}

func (m Model) handleGlobalConfirmKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if m.mode == modeConfirmTermClose {
		return m.handleConfirmTermCloseKey(msg)
	}
	return m.handleConfirmMergeKey(msg)
}

func (m Model) handleConfirmMergeKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	key := msg.String()
	if confirmIsNav(key) {
		m.confirm.cursor = m.confirmMove(key)
		return m, nil
	}
	if key == "enter" {
		key = m.confirmSelectedKey()
	}
	if key == "esc" {
		return m.clearConfirm(), nil
	}
	if key != "m" || m.confirm.merge == nil {
		return m, nil
	}
	target := *m.confirm.merge
	m = m.clearConfirm()
	m.stackView.working = true
	return m, m.mergeStackCmd(target.key, target.params)
}

func (m Model) beginTermCloseConfirm() Model {
	if m.current == nil || m.current.ws == nil {
		return m
	}
	ws := m.current.ws
	target := termCloseTarget{
		key: m.current.key, index: ws.ActiveTerm, worktree: m.current.worktree,
	}
	m.mode = modeConfirmTermClose
	m.confirm = confirmState{
		title: "CLOSE TERMINAL PANE",
		context: []string{
			repoHdrStyle.Render(m.current.repo + " / " + m.current.worktree),
			dimStyle.Render(fmt.Sprintf("pane %d/%d has an active foreground process", ws.ActiveTerm+1, len(ws.Terms))),
			errStyle.Render("! closing it will stop that process"),
		},
		options: []confirmOption{
			{label: "keep pane", key: "esc"},
			{label: "close pane and stop process", key: "x", danger: true},
		},
		term: &target,
	}
	return m
}

func (m Model) handleConfirmTermCloseKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	key := msg.String()
	if confirmIsNav(key) {
		m.confirm.cursor = m.confirmMove(key)
		return m, nil
	}
	if key == "enter" {
		key = m.confirmSelectedKey()
	}
	if key == "esc" {
		return m.clearConfirm(), nil
	}
	if key != "x" || m.confirm.term == nil {
		return m, nil
	}
	target := *m.confirm.term
	if m.current == nil || m.current.key != target.key || m.current.ws == nil ||
		m.current.ws.ActiveTerm != target.index {
		return m.clearConfirm(), nil
	}
	m = m.clearConfirm()
	return m.closeTermPaneNow()
}

func agentConfirmContext(r boardRow) []string {
	context := []string{
		nameStyle.Render(normalizedProvider(r.provider).String() + " · " + r.label),
		dimStyle.Render(r.repo + " / " + r.worktree),
	}
	if r.status != "" {
		context = append(context, dimStyle.Render(r.status))
	}
	return context
}

// rankedActive returns the active item badged with the given recency rank
// (1..3), if any.
func (m Model) rankedActive(rank int) (activeItem, bool) {
	for _, it := range m.active {
		if m.recentRank[wsKey(it.repo.Name, it.view.WT.Name)] == rank {
			return it, true
		}
	}
	return activeItem{}, false
}

func (m Model) handleCreateKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.mode = modeNormal
		m.nameInput.Blur()
		m.status = ""
		return m, m.filter.Focus()
	case "enter":
		name := strings.TrimSpace(m.nameInput.Value())
		if name == "" {
			// Empty stays a silent no-op: dismiss the form, no error.
			m.mode = modeNormal
			m.nameInput.Blur()
			m.status = ""
			return m, m.filter.Focus()
		}
		if err := repo.ValidateWorktreeName(name); err != nil {
			// Reject client-side: keep the form open so the user can fix the
			// name, and surface the reason inline instead of letting it fail
			// later with raw git stderr (or, for "..", escape the repo).
			m.setError("invalid name: " + err.Error())
			return m, nil
		}
		m.mode = modeNormal
		m.nameInput.Blur()
		r := m.pendingRepo
		focusCmd := m.filter.Focus()
		flashTick := m.flashCmd("creating " + name + "…")
		return m, tea.Batch(focusCmd, flashTick, func() tea.Msg {
			wt, err := m.ctrl.Create(r, name, "")
			return createdMsg{wt: wt, err: err}
		})
	}
	var cmd tea.Cmd
	m.nameInput, cmd = m.nameInput.Update(msg)
	return m, cmd
}

// handleConfirmKey resolves the remove-worktree confirm panel. Navigation keys
// (arrows / j / k / ctrl+p / ctrl+n) move the cursor and enter fires the option
// under it; every other key is dispatched by mnemonic, so the direct-key paths
// are byte-identical to the old status-line prompt: "y" removes the worktree
// and deletes its branch, "b" removes it but keeps the branch, "v" opens the
// diff for the review loop. Escape cancels; unrelated keys are ignored.
func (m Model) handleConfirmKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	key := msg.String()
	if confirmIsNav(key) {
		m.confirm.cursor = m.confirmMove(key)
		return m, nil
	}
	if key == "enter" {
		key = m.confirmSelectedKey()
	}
	// "v" exits the prompt and opens the diff for the same worktree; from the
	// diff, `x` loops back to this prompt (the review loop).
	if key == "v" {
		it, ok := m.selectedActive()
		m = m.clearConfirm()
		if !ok {
			return m, nil
		}
		return m.openDiff(it)
	}
	if key == "y" || key == "b" {
		deleteBranch := key == "y"
		it, ok := m.selectedActive()
		m = m.clearConfirm()
		if !ok {
			return m, nil
		}
		m.stopWorkspace(wsKey(it.repo.Name, it.view.WT.Name))
		r, wt := it.repo, it.view.WT
		var flashTick tea.Cmd
		if deleteBranch {
			flashTick = m.flashCmd("removing " + wt.Name + "…")
		} else {
			flashTick = m.flashCmd("removing " + wt.Name + " (keeping branch)…")
		}
		remove := func() tea.Msg { return actionDoneMsg{err: m.ctrl.Remove(r, wt, deleteBranch)} }
		return m, tea.Batch(remove, flashTick)
	}
	if key == "esc" {
		m = m.clearConfirm()
	}
	return m, nil
}

func (m Model) handleConfirmArchiveKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	key := msg.String()
	if confirmIsNav(key) {
		m.confirm.cursor = m.confirmMove(key)
		return m, nil
	}
	if key == "enter" {
		key = m.confirmSelectedKey()
	}
	if m.confirm.target == nil {
		return m.clearConfirm(), nil
	}
	it := *m.confirm.target
	if key == "v" {
		m = m.clearConfirm()
		return m.openDiff(it)
	}
	if key != "y" {
		return m.clearConfirm(), nil
	}
	fingerprint := m.confirm.fingerprint
	p, ok := stackParams(it)
	m = m.clearConfirm()
	if !ok {
		return m, m.flashCmd("archive unavailable for this worktree")
	}
	m.stopWorkspace(wsKey(it.repo.Name, it.view.WT.Name))
	r, wt, ctrl, cleanup := it.repo, it.view.WT, m.ctrl, m.stackArchive
	archive := func() tea.Msg {
		fresh, _, err := archiveWorktreeFingerprint(wt.Path)
		if err != nil {
			return actionDoneMsg{err: fmt.Errorf("archive preflight: %w", err)}
		}
		if fresh != fingerprint {
			return actionDoneMsg{err: fmt.Errorf("worktree changed while archive confirmation was open; review and archive again")}
		}
		if _, err := cleanup(p); err != nil {
			return actionDoneMsg{err: fmt.Errorf("archive cleanup: %w", err)}
		}
		if err := ctrl.Remove(r, wt, true); err != nil {
			return actionDoneMsg{err: fmt.Errorf("archive worktree: %w", err)}
		}
		return actionDoneMsg{}
	}
	return m, tea.Batch(archive, m.flashCmd("archiving "+wt.Name+"…"))
}

// handleConfirmQuitKey resolves the quit guard: navigation moves the cursor,
// enter fires the option under it, "y" quits directly (running CloseAll, which
// kills every hosted pty). Escape cancels; unrelated keys are ignored.
func (m Model) handleConfirmQuitKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	key := msg.String()
	if confirmIsNav(key) {
		m.confirm.cursor = m.confirmMove(key)
		return m, nil
	}
	if key == "enter" {
		key = m.confirmSelectedKey()
	}
	if key == "y" {
		return m, tea.Quit
	}
	if key == "esc" {
		m = m.clearConfirm()
	}
	return m, nil
}

// handleConfirmStopKey resolves the stop guard: navigation moves the cursor,
// enter fires the option under it, "y" stops the selected worktree directly,
// while escape cancels and unrelated keys are ignored. The target is re-read from the cursor here (it
// can't move while the prompt is modal), mirroring handleConfirmKey.
func (m Model) handleConfirmStopKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	key := msg.String()
	if confirmIsNav(key) {
		m.confirm.cursor = m.confirmMove(key)
		return m, nil
	}
	if key == "enter" {
		key = m.confirmSelectedKey()
	}
	if key == "y" {
		it, ok := m.selectedActive()
		m = m.clearConfirm()
		if !ok {
			return m, nil
		}
		m.stopWorkspace(wsKey(it.repo.Name, it.view.WT.Name))
		return m, tea.Batch(m.loadCmd(), m.flashCmd("stopped "+it.view.WT.Name))
	}
	if key == "esc" {
		m = m.clearConfirm()
	}
	return m, nil
}

func (m Model) handleConfirmAgentKey(msg tea.KeyPressMsg, restart bool) (tea.Model, tea.Cmd) {
	key := msg.String()
	if confirmIsNav(key) {
		m.confirm.cursor = m.confirmMove(key)
		return m, nil
	}
	if key == "enter" {
		key = m.confirmSelectedKey()
	}
	target := m.confirm.agent
	actionKey := "d"
	if restart {
		actionKey = "r"
	}
	if key == "esc" {
		return m.clearConfirm(), nil
	}
	if key != actionKey || target == nil {
		return m, nil
	}
	r := *target
	m = m.clearConfirm()
	if restart {
		return m.restartBoardAgent(r)
	}
	return m.closeBoardAgent(r)
}

// clearConfirm dismisses whichever confirm panel is up: back to normal mode,
// cleared panel state, and no lingering status text.
func (m Model) clearConfirm() Model {
	m.mode = modeNormal
	m.status = ""
	m.confirm = confirmState{}
	return m
}

// --- diff viewer ---

// openDiff opens the read-only diff overlay for it: it stamps the target,
// switches into the loading state, and returns the command that runs the git
// calls off the UI goroutine. The result arrives as a diffMsg keyed to the
// target so a stale fetch (the diff was closed or reopened elsewhere meanwhile)
// is dropped on arrival.
func (m Model) openDiff(it activeItem) (tea.Model, tea.Cmd) {
	m.diffOpen = true
	m.diffView = diffState{
		repoName: it.repo.Name,
		wtName:   it.view.WT.Name,
		key:      wsKey(it.repo.Name, it.view.WT.Name),
		base:     it.view.BaseBranch,
		ahead:    it.view.Ahead,
		loading:  true,
	}
	return m, m.fetchDiffCmd(it)
}

// fetchDiffCmd runs one diff fetch off the UI goroutine: the committed diff and
// numstat versus the base (only when the branch has a base to compare against),
// the uncommitted diff and numstat, and the untracked-file list. Any git error
// short-circuits into the message's err, which closes the overlay with a flash.
func (m Model) fetchDiffCmd(it activeItem) tea.Cmd {
	wt := it.view.WT
	base := it.view.BaseBranch
	key := wsKey(it.repo.Name, it.view.WT.Name)
	return func() tea.Msg {
		msg := diffMsg{key: key}
		if base != "" {
			body, err := repo.DiffAgainstBase(wt, base)
			if err != nil {
				msg.err = err
				return msg
			}
			stat, err := repo.NumstatAgainstBase(wt, base)
			if err != nil {
				msg.err = err
				return msg
			}
			msg.committedBody, msg.committedStat = body, stat
		}
		body, err := repo.DiffUncommitted(wt)
		if err != nil {
			msg.err = err
			return msg
		}
		stat, err := repo.NumstatUncommitted(wt)
		if err != nil {
			msg.err = err
			return msg
		}
		msg.uncommittedBody, msg.uncommittedStat = body, stat
		untracked, err := repo.UntrackedFiles(wt)
		if err != nil {
			msg.err = err
			return msg
		}
		msg.untracked = untracked
		return msg
	}
}

// diffChromeRows is the number of fixed rows renderDiff draws around the
// scrollable body: the header, the faint rule beneath it, and the footer legend.
const diffChromeRows = 3

// diffBodyHeight is the number of scrollable content rows the diff viewer shows,
// kept in step with renderDiff's own budget so scroll clamping and rendering
// agree on the window size.
func (m Model) diffBodyHeight() int {
	return max(1, m.height-barHeight-diffChromeRows)
}

// handleDiff routes key events while the diff viewer is open: esc/q close it,
// the movement keys scroll (clamped to the content), J/K jump between file
// headers, u toggles the scope, and x closes the diff and drops into the remove
// flow for the same worktree. Everything else is swallowed.
func (m Model) handleDiff(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	sc := m.diffView.active()
	avail := m.diffBodyHeight()
	maxOff := max(0, len(sc.lines)-avail)
	switch msg.String() {
	case "esc", "q":
		m.diffOpen = false
		m.diffView = diffState{}
	case "j", "down":
		m.diffView.offset = clamp(m.diffView.offset+1, 0, maxOff)
	case "k", "up":
		m.diffView.offset = clamp(m.diffView.offset-1, 0, maxOff)
	case "ctrl+d":
		m.diffView.offset = clamp(m.diffView.offset+avail/2, 0, maxOff)
	case "ctrl+u":
		m.diffView.offset = clamp(m.diffView.offset-avail/2, 0, maxOff)
	case "g":
		m.diffView.offset = 0
	case "G":
		m.diffView.offset = maxOff
	case "J", "]":
		m.diffView.offset = nextFileLine(sc.fileLines, m.diffView.offset, maxOff)
	case "K", "[":
		m.diffView.offset = prevFileLine(sc.fileLines, m.diffView.offset)
	case "u":
		m.diffView.scopeUncommitted = !m.diffView.scopeUncommitted
		m.diffView.offset = 0
	case "x":
		// Close the diff and re-enter the remove flow for the same worktree. The
		// cursor can't have moved while the diff was modal, so selectedActive is
		// still the diff's target.
		it, ok := m.selectedActive()
		m.diffOpen = false
		m.diffView = diffState{}
		if ok && !it.view.WT.IsMain {
			m = m.beginRemove(it)
		}
	}
	return m, nil
}

// diffWheel scrolls the open diff by three lines per wheel notch, clamped to the
// content.
func (m Model) diffWheel(msg tea.MouseWheelMsg) (tea.Model, tea.Cmd) {
	mo := msg.Mouse()
	delta := 3
	if mo.Button == tea.MouseWheelUp {
		delta = -3
	} else if mo.Button != tea.MouseWheelDown {
		return m, nil
	}
	sc := m.diffView.active()
	maxOff := max(0, len(sc.lines)-m.diffBodyHeight())
	m.diffView.offset = clamp(m.diffView.offset+delta, 0, maxOff)
	return m, nil
}

// nextFileLine returns the offset of the first file-header line strictly past
// the current offset (clamped so it never scrolls past the bottom), or the
// bottom itself when there is no later file header.
func nextFileLine(fileLines []int, offset, maxOff int) int {
	for _, i := range fileLines {
		if i > offset {
			return clamp(i, 0, maxOff)
		}
	}
	return maxOff
}

// prevFileLine returns the offset of the last file-header line strictly before
// the current offset, or the top (0) when there is none.
func prevFileLine(fileLines []int, offset int) int {
	prev := 0
	for _, i := range fileLines {
		if i >= offset {
			break
		}
		prev = i
	}
	return prev
}

// stopWorkspace closes a workspace's sessions (dropping its stale attention
// markers) and, if it was current, returns to the picker.
func (m *Model) stopWorkspace(key string) {
	if ws, ok := m.mgr.Workspace(key); ok {
		for _, s := range ws.Agents {
			m.clearAgentTracking(s.Pid())
		}
	}
	m.mgr.Close(key)
	for pid, e := range m.attention {
		if e.key == key {
			delete(m.attention, pid)
		}
	}
	if m.current != nil && m.current.key == key {
		m.current = nil
		m.screen = screenPicker
	}
}

func (m *Model) clearAgentTracking(pid int) {
	if pid == 0 {
		return
	}
	if m.removed == nil {
		m.removed = map[int]int64{}
	}
	// Remember the StartedAt so the poll can tell this dead agent from a future
	// one that reuses the pid (see the removed field and the statusMsg handler).
	m.removed[pid] = m.agentStatus[pid].StartedAt
	delete(m.attention, pid)
	delete(m.agentStatus, pid)
	delete(m.agentPrevStatus, pid)
	delete(m.busySince, pid)
}

func wsKey(repoName, wtName string) string { return repoName + "/" + wtName }

// humanDur formats an elapsed duration compactly for status columns.
func humanDur(d time.Duration) string {
	switch {
	case d < 90*time.Second:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < 90*time.Minute:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	default:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	}
}

// flash sets a transient info status that the status poll auto-clears after
// transientStatusTTL.
func (m *Model) flash(s string) {
	m.status = s
	m.statusLevel = statusInfo
	m.statusAt = time.Now()
}

// flashCmd sets a transient status like flash and returns a dedicated one-shot
// tick that expires it after transientStatusTTL by calling maybeExpireStatus.
// This tick fires on its own timer regardless of deck activity, so a flash
// never lingers past its TTL waiting on some other event loop. It's stamped
// with the current statusAt so a flash set before it fires won't be cleared
// early (see statusExpireMsg). Callers batch the returned cmd with any
// command they already return.
func (m *Model) flashCmd(s string) tea.Cmd {
	m.flash(s)
	at := m.statusAt
	return tea.Tick(transientStatusTTL, func(time.Time) tea.Msg {
		return statusExpireMsg{at: at}
	})
}

// setError sets a sticky error status: styled red and never auto-expired, it
// stays until the next action replaces or clears it.
func (m *Model) setError(s string) {
	m.status = s
	m.statusLevel = statusError
	m.statusAt = time.Time{}
}

// maybeExpireStatus clears a transient info status once it has been shown for
// transientStatusTTL. It's driven solely by the dedicated flash-expiry tick
// scheduled by flashCmd (see statusExpireMsg), not by the status poll.
func (m *Model) maybeExpireStatus() {
	if m.status == "" || m.mode != modeNormal || m.statusAt.IsZero() {
		return
	}
	if m.statusLevel == statusError {
		return
	}
	if time.Since(m.statusAt) > transientStatusTTL {
		m.status = ""
		m.statusAt = time.Time{}
	}
}

// toUVKey converts a Bubble Tea key event into the ultraviolet key event the
// emulator expects (the field layouts are identical).
func toUVKey(k tea.KeyPressMsg) uv.KeyPressEvent {
	code := k.Code
	mod := uv.KeyMod(k.Mod)
	// The vt SendKey default branch only encodes key.Code when Mod==0, so it
	// silently drops shift+letter pairs from kitty-protocol terminals. Collapse
	// them: if shift is the only modifier and there is a known shifted rune,
	// use it directly and clear the shift bit.
	if mod == uv.ModShift && k.ShiftedCode != 0 {
		code = k.ShiftedCode
		mod = 0
	}
	return uv.KeyPressEvent{
		Text:        k.Text,
		Mod:         mod,
		Code:        code,
		ShiftedCode: k.ShiftedCode,
		BaseCode:    k.BaseCode,
	}
}

// --- derived state ---

func (m *Model) recomputeMatches() {
	repos := make([]repo.Repo, 0, len(m.groups))
	names := make([]string, 0, len(m.groups))
	for _, g := range m.groups {
		repos = append(repos, g.Repo)
		names = append(names, g.Repo.Name)
	}

	q := strings.TrimSpace(m.filter.Value())
	if q == "" {
		m.repoMatches = repos
	} else {
		res := fuzzy.Find(q, names)
		m.repoMatches = make([]repo.Repo, 0, len(res))
		for _, mt := range res {
			m.repoMatches = append(m.repoMatches, repos[mt.Index])
		}
	}
	m.newCursor = clamp(m.newCursor, 0, max(0, len(m.repoMatches)-1))
}

func (m *Model) recomputeActive() {
	var items []activeItem
	for _, g := range m.groups {
		var views []WorktreeView
		for _, wv := range g.Worktrees {
			if wv.WT.IsMain {
				continue
			}
			if m.mgr != nil {
				wv.Live = m.mgr.Has(wsKey(g.Repo.Name, wv.WT.Name))
			}
			views = append(views, wv)
		}
		m.sortByRecency(g.Repo.Name, views)
		for _, wv := range views {
			items = append(items, activeItem{repo: g.Repo, view: wv})
		}
	}
	m.active = items
	m.activeCursor = clamp(m.activeCursor, 0, max(0, len(items)-1))
}

// mostRecentWorktree returns the worktree opened most recently in ct (across
// all repos), or ok=false if none has ever been opened.
func (m Model) mostRecentWorktree() (repoName, wtName, path string, ok bool) {
	if m.state == nil {
		return
	}
	var best int64
	for _, g := range m.groups {
		for _, wv := range g.Worktrees {
			if wv.WT.IsMain {
				continue
			}
			if t := m.state.Opened(wsKey(g.Repo.Name, wv.WT.Name)); t > best {
				best = t
				repoName, wtName, path, ok = g.Repo.Name, wv.WT.Name, wv.WT.Path, true
			}
		}
	}
	return
}

// computeRecentRanks finds the three worktrees opened most recently in ct
// (across all repos) and maps their keys to ranks 1..3 for badge display.
func (m *Model) computeRecentRanks() {
	m.recentRank = nil
	if m.state == nil {
		return
	}
	type wr struct {
		key string
		t   int64
	}
	var ws []wr
	for _, g := range m.groups {
		for _, wv := range g.Worktrees {
			if wv.WT.IsMain {
				continue
			}
			key := wsKey(g.Repo.Name, wv.WT.Name)
			if t := m.state.Opened(key); t > 0 {
				ws = append(ws, wr{key, t})
			}
		}
	}
	sort.SliceStable(ws, func(i, j int) bool { return ws[i].t > ws[j].t })

	ranks := make(map[string]int, 3)
	for i, w := range ws {
		if i >= 3 {
			break
		}
		ranks[w.key] = i + 1
	}
	m.recentRank = ranks
}

// sortByRecency orders a repo's worktrees most-recent-first: by when ct last
// opened them, then (for never-opened ones) by HEAD commit time, then by name.
func (m *Model) sortByRecency(repoName string, views []WorktreeView) {
	opened := func(name string) int64 {
		if m.state == nil {
			return 0
		}
		return m.state.Opened(wsKey(repoName, name))
	}
	sort.SliceStable(views, func(i, j int) bool {
		if oi, oj := opened(views[i].WT.Name), opened(views[j].WT.Name); oi != oj {
			return oi > oj
		}
		if views[i].CommitTime != views[j].CommitTime {
			return views[i].CommitTime > views[j].CommitTime
		}
		return views[i].WT.Name < views[j].WT.Name
	})
}

// pathToKey maps a session's working directory to its workspace key. Checks the
// currently active workspace first (covering synthetic workspaces like ~/config
// that don't appear in m.active), then walks the active list. Paths are cleaned
// before comparison to absorb trailing slashes and minor OS-level variation.
func (m Model) pathToKey(path string) (string, bool) {
	path = filepath.Clean(path)
	if m.current != nil && filepath.Clean(m.current.path) == path {
		return m.current.key, true
	}
	for _, it := range m.active {
		if filepath.Clean(it.view.WT.Path) == path {
			return wsKey(it.repo.Name, it.view.WT.Name), true
		}
	}
	if m.homeWSPath != "" && filepath.Clean(m.homeWSPath) == path {
		return m.homeWSKey, true
	}
	return "", false
}

// agentLocation resolves a live session by identity rather than pid, avoiding
// stale observer events mutating a replacement process that reused the pid.
func (m Model) agentLocation(target *session.Session) (key, path string, ok bool) {
	match := func(key, path string) (string, string, bool) {
		ws, exists := m.mgr.Workspace(key)
		if !exists {
			return "", "", false
		}
		for _, candidate := range ws.Agents {
			if candidate == target {
				return key, path, true
			}
		}
		return "", "", false
	}
	if m.current != nil {
		if key, path, ok := match(m.current.key, m.current.path); ok {
			return key, path, true
		}
	}
	for _, it := range m.active {
		if key, path, ok := match(wsKey(it.repo.Name, it.view.WT.Name), it.view.WT.Path); ok {
			return key, path, true
		}
	}
	if m.homeWSKey != "" {
		return match(m.homeWSKey, m.homeWSPath)
	}
	return "", "", false
}

// busyHostedAgents counts agents polled as busy whose working directory maps to
// a workspace ct still hosts, optionally filtered to a single workspace key
// (onlyKey == "" counts across every hosted workspace). It backs the quit and
// stop guards: only agents ct hosts are killed by CloseAll/Close, so agents
// running outside ct don't count.
//
// Note: agentStatus is refreshed on a poll timer and can lag reality by up to
// one poll interval, so this may briefly miss an agent that just went busy or
// still flag one that just finished. That staleness is acceptable for a
// confirmation prompt — the guard is a safety net, not a hard lock.
func (m Model) busyHostedAgents(onlyKey string) int {
	if m.mgr == nil {
		return 0
	}
	n := 0
	for _, st := range m.agentStatus {
		if st.Status != "busy" {
			continue
		}
		key, ok := m.pathToKey(st.Cwd)
		if !ok || !m.mgr.Has(key) {
			continue
		}
		if onlyKey != "" && key != onlyKey {
			continue
		}
		n++
	}
	return n
}

func (m Model) selectedActive() (activeItem, bool) {
	if m.activeCursor < 0 || m.activeCursor >= len(m.active) {
		return activeItem{}, false
	}
	return m.active[m.activeCursor], true
}

func (m *Model) selectActiveByKey(key string) bool {
	for i, it := range m.active {
		if wsKey(it.repo.Name, it.view.WT.Name) == key {
			m.screen = screenPicker
			m.focus = focusActive
			m.activeCursor = i
			return true
		}
	}
	return false
}

func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
