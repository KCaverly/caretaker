package session

import (
	"errors"
	"sync"
)

// errNoWorkspace is returned when an operation targets a workspace that isn't
// active.
var errNoWorkspace = errors.New("session: workspace not active")

// Spec describes one session to start in a workspace.
type Spec struct {
	Kind  Kind
	Title string
	Argv  []string
	// SessionID is the claude session UUID an Agent spec runs under, recorded on
	// the started Session so ct can persist and later resume it.
	SessionID string
}

// Workspace holds the live sessions for one activated worktree: a single editor,
// a pool of terminal panes arranged in a split tree, and a pool of agents.
type Workspace struct {
	Editor     *Session
	Terms      []*Session // terminal panes; the tree below describes their layout
	TermLayout *PaneNode  // nil when Terms is empty
	ActiveTerm int        // index of the focused pane in Terms
	TermZoomed bool       // when true, only the focused pane is shown full-size
	Agents      []*Session
	// ActiveAgent indexes the focused agent in Agents (clamped to a valid index
	// while any agent exists).
	ActiveAgent int
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
}

// NewManager returns an empty Manager.
func NewManager() *Manager {
	return &Manager{
		spaces: make(map[string]*Workspace),
		dirty:  make(chan struct{}, 1),
	}
}

// Dirty returns a channel that receives a value whenever any session's screen
// changes; callers use it to trigger a repaint.
func (m *Manager) Dirty() <-chan struct{} { return m.dirty }

func (m *Manager) signalDirty() {
	select {
	case m.dirty <- struct{}{}:
	default: // coalesce: a repaint is already pending
	}
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
		return ws, nil
	}

	ws := &Workspace{}
	for _, sp := range specs {
		s, err := Start(sp.Kind, sp.Title, dir, sp.Argv, w, h, m.signalDirty)
		if err != nil {
			for _, started := range ws.all() {
				started.Close()
			}
			return nil, err
		}
		s.SessionID = sp.SessionID
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

// SpawnAgent starts a new agent session in an active workspace, appends it to
// the pool, and focuses it. The workspace must already be active.
func (m *Manager) SpawnAgent(key, dir string, spec Spec, w, h int) (*Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	ws, ok := m.spaces[key]
	if !ok {
		return nil, errNoWorkspace
	}
	s, err := Start(spec.Kind, spec.Title, dir, spec.Argv, w, h, m.signalDirty)
	if err != nil {
		return nil, err
	}
	s.SessionID = spec.SessionID
	ws.Agents = append(ws.Agents, s)
	ws.ActiveAgent = len(ws.Agents) - 1
	return s, nil
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

// Resize resizes every non-terminal session (editor and agents). Terminal
// panes are resized separately by ResizeTermPanes because each pane has its
// own dimensions determined by the split tree.
func (m *Manager) Resize(w, h int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, ws := range m.spaces {
		if ws.Editor != nil {
			ws.Editor.Resize(w, h)
		}
		for _, a := range ws.Agents {
			a.Resize(w, h)
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
	s, err := Start(spec.Kind, spec.Title, dir, spec.Argv, w/2, h, m.signalDirty)
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
