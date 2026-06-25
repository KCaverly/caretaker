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

// Workspace holds the live sessions for one activated worktree: a single editor
// and terminal, plus a pool of agents (Claude sessions) with one focused.
type Workspace struct {
	Editor *Session
	Term   *Session
	Agents []*Session
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

// all returns every live session in the workspace.
func (w *Workspace) all() []*Session {
	ss := make([]*Session, 0, len(w.Agents)+2)
	if w.Editor != nil {
		ss = append(ss, w.Editor)
	}
	if w.Term != nil {
		ss = append(ss, w.Term)
	}
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
		case sp.Kind == Terminal && ws.Term == nil:
			ws.Term = s
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

// Resize resizes every live session.
func (m *Manager) Resize(w, h int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, ws := range m.spaces {
		for _, s := range ws.all() {
			s.Resize(w, h)
		}
	}
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
