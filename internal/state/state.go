// Package state persists ct's small bits of cross-session state — currently the
// last time each worktree was opened, used to order the active list.
package state

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// State is ct's persisted state, loaded from and saved to a JSON file in the
// user's XDG state directory.
type State struct {
	path       string
	LastOpened map[string]int64           `json:"last_opened"` // "repo/worktree" -> unix seconds
	Workspaces map[string]*WorkspaceState `json:"workspaces"`  // "repo/worktree" -> agent pool
}

// WorkspaceState records a worktree's agent pool so ct can rebuild it (resuming
// each claude conversation) the next time the worktree is opened.
type WorkspaceState struct {
	Agents      []AgentState `json:"agents"`
	ActiveAgent int          `json:"active_agent"`
}

// AgentState is one persisted agent: the claude session UUID to resume and the
// display label shown in the agent palette.
type AgentState struct {
	SessionID string `json:"session_id"`
	Label     string `json:"label"`
}

// dir returns ct's state directory (honoring XDG_STATE_HOME).
func dir() (string, error) {
	if x := os.Getenv("XDG_STATE_HOME"); x != "" {
		return filepath.Join(x, "ct"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "state", "ct"), nil
}

// Load reads the state file, returning an empty (usable) state if it's missing
// or unreadable. It never errors so a corrupt/absent file can't block startup.
func Load() *State {
	s := &State{LastOpened: map[string]int64{}, Workspaces: map[string]*WorkspaceState{}}
	d, err := dir()
	if err != nil {
		return s
	}
	s.path = filepath.Join(d, "state.json")
	data, err := os.ReadFile(s.path)
	if err != nil {
		return s
	}
	_ = json.Unmarshal(data, s)
	if s.LastOpened == nil {
		s.LastOpened = map[string]int64{}
	}
	if s.Workspaces == nil {
		s.Workspaces = map[string]*WorkspaceState{}
	}
	return s
}

// Touch records that key was opened just now.
func (s *State) Touch(key string) {
	s.LastOpened[key] = time.Now().Unix()
}

// Opened returns the last-opened unix time for key, or 0 if never opened.
func (s *State) Opened(key string) int64 {
	return s.LastOpened[key]
}

// Agents returns the persisted agent pool for key (resume order) and the index
// of the agent that was focused. It returns nil if the worktree has no saved
// pool, which the caller treats as "start one fresh agent".
func (s *State) Agents(key string) (agents []AgentState, active int) {
	ws := s.Workspaces[key]
	if ws == nil {
		return nil, 0
	}
	return ws.Agents, ws.ActiveAgent
}

// SetAgents records the agent pool for key. An empty pool clears the entry so a
// later open starts fresh rather than resuming nothing.
func (s *State) SetAgents(key string, agents []AgentState, active int) {
	if len(agents) == 0 {
		delete(s.Workspaces, key)
		return
	}
	s.Workspaces[key] = &WorkspaceState{Agents: agents, ActiveAgent: active}
}

// Save atomically writes the state to disk. It's a no-op if no path resolved.
func (s *State) Save() error {
	if s.path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}
