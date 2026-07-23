package session

import (
	"errors"
	"io"
	"sync"
	"time"

	"github.com/KCaverly/caretaker/internal/agent"
)

// repaintWindow bounds how often WaitDirty releases the UI to re-render under
// sustained pty output. The first dirty signal after a quiet gap is released
// immediately (leading edge) so a lone keystroke echo never waits; further
// signals that land within the window collapse into a single trailing repaint.
// The width trades leading-edge input latency against the sustained render
// ceiling: every repaint re-serialises the whole vt buffer per visible session
// (~60µs/616 allocs at 80×24 up to ~253µs/1,901 allocs at 200×50), so an
// uncapped loop under a build or a streaming agent renders back-to-back as fast
// as frames complete. At 12ms a burst is capped near ~80fps — well below the
// flicker threshold — while a solitary echo still repaints with no added delay.
const repaintWindow = 12 * time.Millisecond

// errNoWorkspace is returned when an operation targets a workspace that isn't
// active.
var errNoWorkspace = errors.New("session: workspace not active")

// errNoAgent is returned when an operation targets an agent that isn't active.
var errNoAgent = errors.New("session: agent not active")

// errEmptyAgentArgv prevents a malformed provider spec from silently launching
// the user's shell in an agent pane.
var errEmptyAgentArgv = errors.New("session: agent command is empty")

// Spec describes one session to start in a workspace.
type Spec struct {
	Kind  Kind
	Title string
	Argv  []string
	// Provider identifies the CLI that owns this agent conversation.
	Provider agent.Provider
	// SessionID is the provider-owned, opaque conversation ID recorded on the
	// started Session so ct can persist and later resume it.
	SessionID string
	// Env adds or replaces process environment entries in KEY=value form.
	Env []string
	// UnsetEnv removes inherited process environment entries by key. Removal
	// wins if the same key is also present in Env.
	UnsetEnv []string
	// Events carries provider lifecycle notifications for this session.
	Events <-chan agent.Event
	// Companion owns any provider-side process needed by the interactive
	// session. It is closed if startup fails and whenever the Session closes.
	Companion io.Closer
}

// Workspace holds the live sessions for one activated worktree: a single editor,
// a pool of terminal panes arranged in a split tree, and a pool of agents.
type Workspace struct {
	Editor     *Session
	Terms      []*Session // terminal panes; the tree below describes their layout
	TermLayout *PaneNode  // nil when Terms is empty
	ActiveTerm int        // index of the focused pane in Terms
	TermZoomed bool       // when true, only the focused pane is shown full-size
	Agents     []*Session
	// ActiveAgent indexes the focused agent in Agents (clamped to a valid index
	// while any agent exists).
	ActiveAgent int
	// w, h is the body size last applied to this workspace's sessions. Only
	// the current workspace is resized on terminal resize; others catch up in
	// Activate when this differs from the requested size.
	w, h int
}

// ActiveAgentSession returns the focused agent, or nil if the pool is empty.
func (w *Workspace) ActiveAgentSession() *Session {
	if w.ActiveAgent < 0 || w.ActiveAgent >= len(w.Agents) {
		return nil
	}
	return w.Agents[w.ActiveAgent]
}

// ActiveTermSession returns the focused terminal pane, or nil if none exist.
func (w *Workspace) ActiveTermSession() *Session {
	if w.ActiveTerm < 0 || w.ActiveTerm >= len(w.Terms) {
		return nil
	}
	return w.Terms[w.ActiveTerm]
}

// all returns every live session in the workspace.
func (w *Workspace) all() []*Session {
	ss := make([]*Session, 0, len(w.Agents)+1+len(w.Terms))
	if w.Editor != nil {
		ss = append(ss, w.Editor)
	}
	ss = append(ss, w.Terms...)
	return append(ss, w.Agents...)
}

// Manager owns the live Workspace for every activated worktree, keyed by a
// caller-provided workspace key (e.g. "repo/worktree").
type Manager struct {
	mu     sync.Mutex
	spaces map[string]*Workspace
	dirty  chan struct{}

	// lastRepaint stamps when WaitDirty last released a repaint, gating the
	// coalescing window. Only WaitDirty (a single in-flight repaint command)
	// touches it, but its own mutex keeps that free of any race assumption.
	repaintMu   sync.Mutex
	lastRepaint time.Time

	// visible is the set of sessions currently drawn on screen; output from any
	// other session is dropped instead of waking the UI. Guarded by its own
	// RWMutex so pty pump goroutines never contend with mu (which Resize et al.
	// hold across ioctls).
	visMu   sync.RWMutex
	visible map[*Session]struct{}
	// visList mirrors the installed visible set (in the order last passed to
	// SetVisible, nils removed). It exists only so SetVisible can detect an
	// unchanged set and skip the lock + map rebuild. Touched ONLY by SetVisible,
	// which the contract restricts to the UI goroutine, so it needs no lock;
	// signalDirty (pump goroutines) reads visible, never visList.
	visList []*Session
}

// NewManager returns an empty Manager.
func NewManager() *Manager {
	return &Manager{
		spaces: make(map[string]*Workspace),
		dirty:  make(chan struct{}, 1),
	}
}

// Dirty returns a channel that receives a value whenever a visible session's
// screen changes; callers use it to trigger a repaint.
func (m *Manager) Dirty() <-chan struct{} { return m.dirty }

// WaitDirty blocks until a visible session's screen changes, then returns —
// coalescing bursts into at most one repaint per repaintWindow. The first
// signal after a quiet gap returns immediately (leading edge: no latency on a
// lone keystroke echo); a signal that arrives inside the window instead sleeps
// out the remainder and drains one further pending signal, so a run of fast
// output collapses into a single trailing repaint rather than re-rendering the
// whole vt buffer once per frame. Only one repaint command calls this at a
// time; repaintMu keeps lastRepaint race-free regardless.
func (m *Manager) WaitDirty() {
	<-m.dirty

	m.repaintMu.Lock()
	remaining := repaintWindow - time.Since(m.lastRepaint)
	m.repaintMu.Unlock()

	if remaining > 0 {
		// Inside the window from the previous repaint: this is sustained
		// output. Sleep the remainder, then drain any signal that piled up
		// during it — the imminent render reads the current buffer and so
		// already reflects those changes, and dropping it here avoids an
		// extra redundant repaint once output stops.
		time.Sleep(remaining)
		select {
		case <-m.dirty:
		default:
		}
	}

	m.repaintMu.Lock()
	m.lastRepaint = time.Now()
	m.repaintMu.Unlock()
}

// SetVisible replaces the set of sessions considered on-screen. Output from
// sessions outside the set no longer triggers repaints; switching a session
// into view repaints it anyway (the switch itself renders), so no frame is
// ever missed. Call with no arguments when no session is visible (picker,
// overlays).
//
// The UI calls this after EVERY message, but the visible set is unchanged on
// the overwhelming majority of frames (every dirtyMsg repaint under output).
// So the common path must be cheap: sameVisible compares the incoming set
// against the last one installed and, when they match, returns without taking
// visMu or allocating a map. Only a real change rebuilds the map under the
// lock. Must be called only from the UI goroutine (see visList).
func (m *Manager) SetVisible(ss ...*Session) {
	if m.sameVisible(ss) {
		return
	}
	vis := make(map[*Session]struct{}, len(ss))
	list := make([]*Session, 0, len(ss))
	for _, s := range ss {
		if s != nil {
			vis[s] = struct{}{}
			list = append(list, s)
		}
	}
	m.visMu.Lock()
	m.visible = vis
	m.visMu.Unlock()
	m.visList = list
}

// sameVisible reports whether ss (nils skipped) is element-for-element equal to
// the last installed visible set. An ordered compare is deliberately
// conservative: a reordering of the same members reports "changed" and pays one
// extra rebuild, but equal here always means the set is genuinely unchanged. No
// lock is taken — visList is UI-goroutine-only, the same goroutine that calls
// SetVisible.
func (m *Manager) sameVisible(ss []*Session) bool {
	i := 0
	for _, s := range ss {
		if s == nil {
			continue
		}
		if i >= len(m.visList) || m.visList[i] != s {
			return false
		}
		i++
	}
	return i == len(m.visList)
}

func (m *Manager) signalDirty(s *Session) {
	m.visMu.RLock()
	_, ok := m.visible[s]
	m.visMu.RUnlock()
	if !ok {
		return // off-screen output; the poll/badges cover attention
	}
	select {
	case m.dirty <- struct{}{}:
	default: // coalesce: a repaint is already pending
	}
}

// Count returns the number of active workspaces.
func (m *Manager) Count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.spaces)
}

// Has reports whether a workspace has live sessions.
func (m *Manager) Has(key string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.spaces[key]
	return ok
}

// Workspace returns the live Workspace for key.
func (m *Manager) Workspace(key string) (*Workspace, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	w, ok := m.spaces[key]
	return w, ok
}

// Activate returns the Workspace for key, starting its sessions from specs (in
// dir, sized w×h) if it isn't already running. Sessions are assigned by Kind:
// the first Editor and Terminal specs become Editor/Term, and every Agent spec
// joins the agent pool. An existing workspace is reused as-is.
func (m *Manager) Activate(key, dir string, specs []Spec, w, h int) (*Workspace, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if ws, ok := m.spaces[key]; ok {
		// A workspace that sat in the background through terminal resizes has
		// stale pty sizes; bring it up to date now that it's becoming current.
		if ws.w != w || ws.h != h {
			m.resizeLocked(ws, w, h)
		}
		return ws, nil
	}

	sessions, err := startSpecs(specs, dir, w, h, m.signalDirty)
	if err != nil {
		return nil, err
	}

	ws := &Workspace{w: w, h: h}
	for i, sp := range specs {
		s := sessions[i]
		switch {
		case sp.Kind == Editor && ws.Editor == nil:
			ws.Editor = s
		case sp.Kind == Terminal:
			idx := len(ws.Terms)
			ws.Terms = append(ws.Terms, s)
			if ws.TermLayout == nil {
				ws.TermLayout = &PaneNode{Dir: SplitNone, Idx: idx}
			}
		default:
			ws.Agents = append(ws.Agents, s)
		}
	}
	m.spaces[key] = ws
	return ws, nil
}

// startSpecs launches every spec concurrently and returns the started sessions
// in spec order. Each StartSpec is independent — it forks its own process and
// allocates its own terminal emulator (a multi-megabyte parser buffer that must
// be zeroed) — so overlapping them collapses the wall-clock latency a workspace
// activation adds before the editor appears, instead of paying editor + agents
// + terminal one after another. The fork/exec syscalls still serialise on the
// runtime's fork lock, but the emulator allocation and pty setup around them run
// in parallel across cores. On any failure every session that did start is
// closed; StartSpec already releases the companion of a spec it failed to start,
// so activation stays all-or-nothing.
func startSpecs(specs []Spec, dir string, w, h int, dirty func(*Session)) ([]*Session, error) {
	sessions := make([]*Session, len(specs))
	errs := make([]error, len(specs))
	var wg sync.WaitGroup
	wg.Add(len(specs))
	for i := range specs {
		go func(i int) {
			defer wg.Done()
			sessions[i], errs[i] = StartSpec(specs[i], dir, w, h, dirty)
		}(i)
	}
	wg.Wait()

	for _, err := range errs {
		if err != nil {
			for _, s := range sessions {
				if s != nil {
					s.Close()
				}
			}
			return nil, err
		}
	}
	return sessions, nil
}

// SpawnAgent starts a new agent session in an active workspace, appends it to
// the pool, and focuses it. The workspace must already be active.
func (m *Manager) SpawnAgent(key, dir string, spec Spec, w, h int) (*Session, error) {
	m.mu.Lock()
	ws, ok := m.spaces[key]
	if !ok {
		m.mu.Unlock()
		closeSpecCompanions([]Spec{spec})
		return nil, errNoWorkspace
	}
	s, err := StartSpec(spec, dir, w, h, m.signalDirty)
	if err != nil {
		m.mu.Unlock()
		return nil, err
	}
	ws.Agents = append(ws.Agents, s)
	ws.ActiveAgent = len(ws.Agents) - 1
	m.mu.Unlock()
	return s, nil
}

// ReplaceAgent starts spec and, only after startup succeeds, swaps it into idx
// in the active workspace's agent pool. The pool order and focused index are
// preserved. A failed start leaves the old agent running and untouched.
func (m *Manager) ReplaceAgent(key, dir string, idx int, spec Spec, w, h int) (*Session, error) {
	m.mu.Lock()
	ws, ok := m.spaces[key]
	if !ok {
		m.mu.Unlock()
		closeSpecCompanions([]Spec{spec})
		return nil, errNoWorkspace
	}
	if idx < 0 || idx >= len(ws.Agents) {
		m.mu.Unlock()
		closeSpecCompanions([]Spec{spec})
		return nil, errNoAgent
	}

	replacement, err := StartSpec(spec, dir, w, h, m.signalDirty)
	if err != nil {
		m.mu.Unlock()
		return nil, err
	}
	old := ws.Agents[idx]
	ws.Agents[idx] = replacement
	m.mu.Unlock()

	// Close outside the manager lock: replacement is already installed, and a
	// slow process teardown must not block unrelated workspace operations.
	old.Close()
	return replacement, nil
}

func closeSpecCompanions(specs []Spec) {
	for _, spec := range specs {
		if spec.Companion != nil {
			_ = spec.Companion.Close()
		}
	}
}

// CloseAgent terminates and removes the agent at idx, clamping the focused
// index. It's a no-op if the workspace or index is invalid.
func (m *Manager) CloseAgent(key string, idx int) {
	m.mu.Lock()
	ws, ok := m.spaces[key]
	if !ok || idx < 0 || idx >= len(ws.Agents) {
		m.mu.Unlock()
		return
	}
	s := ws.Agents[idx]
	ws.Agents = append(ws.Agents[:idx], ws.Agents[idx+1:]...)
	if ws.ActiveAgent >= len(ws.Agents) {
		ws.ActiveAgent = max(0, len(ws.Agents)-1)
	}
	m.mu.Unlock()
	s.Close()
}

// ResizeWorkspace resizes key's editor and agents to w×h and recomputes its
// terminal pane rectangles. Other workspaces are deliberately left alone —
// resizing them means one pty ioctl (and a SIGWINCH redraw in the program)
// per session for output nobody is watching; Activate brings a stale
// workspace up to date when it next becomes current.
func (m *Manager) ResizeWorkspace(key string, w, h int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if ws, ok := m.spaces[key]; ok {
		m.resizeLocked(ws, w, h)
	}
}

// resizeLocked applies w×h to every session of ws and records the size.
// Callers must hold m.mu.
func (m *Manager) resizeLocked(ws *Workspace, w, h int) {
	ws.w, ws.h = w, h
	if ws.Editor != nil {
		ws.Editor.Resize(w, h)
	}
	for _, a := range ws.Agents {
		a.Resize(w, h)
	}
	if ws.TermLayout != nil {
		for _, b := range ComputePaneBounds(ws.TermLayout, 0, 0, w, h) {
			if b.Idx < len(ws.Terms) && ws.Terms[b.Idx] != nil {
				ws.Terms[b.Idx].Resize(b.W, b.H)
			}
		}
	}
}

// ResizeTermPanes recomputes pane bounds from the split tree and resizes each
// terminal session to its assigned rectangle. w and h are the body dimensions
// (terminal height minus the status-bar chrome).
func (m *Manager) ResizeTermPanes(key string, w, h int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	ws, ok := m.spaces[key]
	if !ok || ws.TermLayout == nil {
		return
	}
	for _, b := range ComputePaneBounds(ws.TermLayout, 0, 0, w, h) {
		if b.Idx < len(ws.Terms) && ws.Terms[b.Idx] != nil {
			ws.Terms[b.Idx].Resize(b.W, b.H)
		}
	}
}

// SplitTermPane spawns a new terminal session and inserts it into the pane
// tree as a sibling of the currently focused pane (split in direction sd).
// The new pane becomes the focused pane. The caller should call
// ResizeTermPanes afterwards to correct the pane dimensions.
func (m *Manager) SplitTermPane(key, dir string, spec Spec, sd SplitDir, w, h int) (*Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	ws, ok := m.spaces[key]
	if !ok {
		return nil, errNoWorkspace
	}
	if w < 4 || h < 3 {
		return nil, nil // too small to split; silently ignore
	}
	newIdx := len(ws.Terms)
	s, err := StartSpec(spec, dir, w/2, h, m.signalDirty)
	if err != nil {
		return nil, err
	}
	ws.Terms = append(ws.Terms, s)
	ws.TermLayout = SplitPaneNode(ws.TermLayout, ws.ActiveTerm, newIdx, sd)
	ws.ActiveTerm = newIdx
	return s, nil
}

// CloseTermPane closes the currently focused terminal pane, heals the split
// tree, and compacts the Terms slice. The caller should call ResizeTermPanes
// afterwards to fill the vacated space.
func (m *Manager) CloseTermPane(key string) error {
	m.mu.Lock()
	ws, ok := m.spaces[key]
	if !ok || len(ws.Terms) == 0 {
		m.mu.Unlock()
		return errNoWorkspace
	}
	closeIdx := ws.ActiveTerm
	s := ws.Terms[closeIdx]
	n := len(ws.Terms)

	ws.Terms = append(ws.Terms[:closeIdx], ws.Terms[closeIdx+1:]...)
	ws.TermLayout = ClosePaneNode(ws.TermLayout, closeIdx)

	if ws.TermLayout != nil && closeIdx < n-1 {
		mapping := make(map[int]int, n-1-closeIdx)
		for i := closeIdx + 1; i < n; i++ {
			mapping[i] = i - 1
		}
		RemapPaneIndices(ws.TermLayout, mapping)
	}

	if len(ws.Terms) == 0 {
		ws.ActiveTerm = 0
	} else {
		ws.ActiveTerm = min(closeIdx, len(ws.Terms)-1)
	}
	m.mu.Unlock()
	s.Close()
	return nil
}

// CycleTermPane advances the focused terminal pane to the next leaf in the
// in-order traversal of the split tree.
func (m *Manager) CycleTermPane(key string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	ws, ok := m.spaces[key]
	if !ok || ws.TermLayout == nil {
		return
	}
	ws.ActiveTerm = NextPaneIdx(ws.TermLayout, ws.ActiveTerm)
}

// FocusTermPaneDir moves the focused terminal pane in direction dir, using the
// pane rectangles resolved for a w×h body. It's a no-op when the workspace is
// unknown, has fewer than two panes, has no layout, is zoomed, or when there is
// no pane beyond the active one in that direction (no wrapping).
func (m *Manager) FocusTermPaneDir(key string, dir FocusDir, w, h int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	ws, ok := m.spaces[key]
	if !ok || len(ws.Terms) < 2 || ws.TermLayout == nil || ws.TermZoomed {
		return
	}
	bounds := ComputePaneBounds(ws.TermLayout, 0, 0, w, h)
	if idx := FocusPaneDir(bounds, ws.ActiveTerm, dir); idx >= 0 {
		ws.ActiveTerm = idx
	}
}

// FocusTermPaneAt focuses the terminal pane containing body-local point (x, y),
// using the pane rectangles resolved for a w×h body. It's a no-op when the
// workspace is unknown, has fewer than two panes, has no layout, is zoomed, or
// when the point lands on a divider or outside every pane.
func (m *Manager) FocusTermPaneAt(key string, x, y, w, h int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	ws, ok := m.spaces[key]
	if !ok || len(ws.Terms) < 2 || ws.TermLayout == nil || ws.TermZoomed {
		return
	}
	bounds := ComputePaneBounds(ws.TermLayout, 0, 0, w, h)
	if idx := PaneAt(bounds, x, y); idx >= 0 {
		ws.ActiveTerm = idx
	}
}

// ZoomTermPane toggles the focused terminal pane between its normal split
// position and a full-size view.
func (m *Manager) ZoomTermPane(key string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	ws, ok := m.spaces[key]
	if !ok {
		return
	}
	ws.TermZoomed = !ws.TermZoomed
}

// Close terminates and forgets a workspace's sessions.
func (m *Manager) Close(key string) {
	m.mu.Lock()
	ws := m.spaces[key]
	delete(m.spaces, key)
	m.mu.Unlock()
	if ws != nil {
		for _, s := range ws.all() {
			s.Close()
		}
	}
}

// CloseAll terminates every session (call on exit).
func (m *Manager) CloseAll() {
	m.mu.Lock()
	all := m.spaces
	m.spaces = make(map[string]*Workspace)
	m.mu.Unlock()
	for _, ws := range all {
		for _, s := range ws.all() {
			s.Close()
		}
	}
}
