// Package backend defines the seam between ct's workspace model and the program
// that actually hosts sessions. Phase 1 ships a zellij backend; a native PTY
// backend can be added later without changing the deck or the workspace model.
package backend

import (
	"os/exec"

	"github.com/KCaverly/caretaker/internal/workspace"
)

// Backend hosts workspaces. Implementations map a Workspace onto real sessions.
type Backend interface {
	// Exists reports whether a live session for ws is currently running.
	Exists(ws workspace.Workspace) (bool, error)

	// Ensure creates the session and its layout if it does not already exist.
	Ensure(ws workspace.Workspace) error

	// AttachCmd returns the command that attaches to ws full-screen, to be run
	// via tea.ExecProcess. A backend that attaches in-process (e.g. native PTY)
	// returns (nil, nil); callers must handle that case.
	AttachCmd(ws workspace.Workspace) (*exec.Cmd, error)

	// AddSession adds a session to a running workspace.
	AddSession(ws workspace.Workspace, s workspace.Session) error

	// Archive tears down the running session, leaving the worktree on disk.
	Archive(ws workspace.Workspace) error
}
