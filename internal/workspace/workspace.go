// Package workspace models the layout of sessions bound to a single worktree.
// It is backend-agnostic: a backend (zellij, native, ...) turns a Workspace into
// real running sessions.
package workspace

// SessionKind identifies the type of a session in a workspace.
type SessionKind int

const (
	Editor   SessionKind = iota // the nvim window
	Agent                       // a claude session
	Terminal                    // a shell
)

func (k SessionKind) String() string {
	switch k {
	case Editor:
		return "editor"
	case Agent:
		return "agent"
	case Terminal:
		return "terminal"
	default:
		return "unknown"
	}
}

// Session is a single pane/tab in a workspace.
type Session struct {
	Kind  SessionKind
	Title string   // tab title shown by the backend
	Argv  []string // command to run; empty means an interactive shell
}

// Workspace is the set of sessions for one worktree.
type Workspace struct {
	Repo     string // owning repo name
	Worktree string // worktree name
	Dir      string // worktree working directory (cwd for every session)
	Sessions []Session
}

// Commands holds the programs used to populate a default workspace.
type Commands struct {
	Editor string
	Agent  string
	Shell  string
}

// Default builds the default workspace template: nvim + 1 claude + 1 terminal.
func Default(repo, worktree, dir string, cmds Commands) Workspace {
	return Workspace{
		Repo:     repo,
		Worktree: worktree,
		Dir:      dir,
		Sessions: []Session{
			{Kind: Editor, Title: "nvim", Argv: []string{cmds.Editor}},
			{Kind: Agent, Title: "claude", Argv: []string{cmds.Agent}},
			// Empty Argv → the backend opens an interactive shell.
			{Kind: Terminal, Title: "term"},
		},
	}
}

// AgentSession builds an additional claude session.
func AgentSession(cmds Commands) Session {
	return Session{Kind: Agent, Title: "claude", Argv: []string{cmds.Agent}}
}

// TerminalSession builds an additional terminal session (interactive shell).
func TerminalSession() Session {
	return Session{Kind: Terminal, Title: "term"}
}
