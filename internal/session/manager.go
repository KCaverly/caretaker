package session

import "sync"

// Spec describes one session to start in a workspace.
type Spec struct {
	Kind  Kind
	Title string
	Argv  []string
}

// Manager owns the live sessions for every activated workspace, keyed by a
// caller-provided workspace key (e.g. "repo/worktree").
type Manager struct {
	mu       sync.Mutex
	sessions map[string][]*Session
	dirty    chan struct{}
}

// NewManager returns an empty Manager.
func NewManager() *Manager {
	return &Manager{
		sessions: make(map[string][]*Session),
		dirty:    make(chan struct{}, 1),
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
	_, ok := m.sessions[key]
	return ok
}

// Get returns the sessions for a workspace.
func (m *Manager) Get(key string) ([]*Session, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	ss, ok := m.sessions[key]
	return ss, ok
}

// Activate returns the sessions for key, starting them from specs (in dir, sized
// w×h) if they aren't already running. Existing sessions are reused as-is.
func (m *Manager) Activate(key, dir string, specs []Spec, w, h int) ([]*Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if ss, ok := m.sessions[key]; ok {
		return ss, nil
	}

	var ss []*Session
	for _, sp := range specs {
		s, err := Start(sp.Kind, sp.Title, dir, sp.Argv, w, h, m.signalDirty)
		if err != nil {
			for _, started := range ss {
				started.Close()
			}
			return nil, err
		}
		ss = append(ss, s)
	}
	m.sessions[key] = ss
	return ss, nil
}

// Resize resizes every live session.
func (m *Manager) Resize(w, h int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, ss := range m.sessions {
		for _, s := range ss {
			s.Resize(w, h)
		}
	}
}

// Close terminates and forgets a workspace's sessions.
func (m *Manager) Close(key string) {
	m.mu.Lock()
	ss := m.sessions[key]
	delete(m.sessions, key)
	m.mu.Unlock()
	for _, s := range ss {
		s.Close()
	}
}

// CloseAll terminates every session (call on exit).
func (m *Manager) CloseAll() {
	m.mu.Lock()
	all := m.sessions
	m.sessions = make(map[string][]*Session)
	m.mu.Unlock()
	for _, ss := range all {
		for _, s := range ss {
			s.Close()
		}
	}
}
