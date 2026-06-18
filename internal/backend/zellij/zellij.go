// Package zellij implements the Phase-1 ct backend on top of the zellij
// terminal multiplexer. Each workspace maps to a zellij session whose tabs are
// the workspace's sessions (nvim / claude / term).
package zellij

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/KCaverly/caretaker/internal/workspace"
)

// Backend hosts workspaces as zellij sessions.
type Backend struct {
	layoutDir string // where generated layout files are written
}

// New returns a zellij Backend, creating its layout cache directory.
func New() (*Backend, error) {
	cache, err := os.UserCacheDir()
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(cache, "ct", "layouts")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return &Backend{layoutDir: dir}, nil
}

// SessionName returns the zellij session name for a workspace.
func SessionName(ws workspace.Workspace) string {
	return sanitize(ws.Repo + "-" + ws.Worktree)
}

// sanitize maps a string to a zellij-safe session name: [A-Za-z0-9_-].
func sanitize(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		out = "ct"
	}
	return out
}

// Exists reports whether a live (non-exited) session for ws is running.
func (b *Backend) Exists(ws workspace.Workspace) (bool, error) {
	return b.live(SessionName(ws))
}

func (b *Backend) live(name string) (bool, error) {
	out, err := run("list-sessions", "--no-formatting")
	if err != nil {
		// `list-sessions` exits non-zero when there are no sessions at all.
		if strings.Contains(strings.ToLower(out), "no active") || strings.Contains(strings.ToLower(err.Error()), "no active") {
			return false, nil
		}
		return false, err
	}
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || fields[0] != name {
			continue
		}
		// A session is live unless marked EXITED ("attach to resurrect").
		return !strings.Contains(line, "EXITED"), nil
	}
	return false, nil
}

// Ensure creates a fresh background session with the workspace layout if a live
// one does not already exist.
func (b *Backend) Ensure(ws workspace.Workspace) error {
	name := SessionName(ws)
	live, err := b.live(name)
	if err != nil {
		return err
	}
	if live {
		return nil
	}

	// Clear any exited/resurrectable remnant so we start from the generated
	// layout. The session is known to be non-live here, so a plain
	// delete-session is enough (and avoids --force's noisy non-zero exit).
	_, _ = run("delete-session", name)

	layoutPath := filepath.Join(b.layoutDir, name+".kdl")
	if err := os.WriteFile(layoutPath, []byte(renderLayout(ws)), 0o644); err != nil {
		return fmt.Errorf("writing layout: %w", err)
	}
	if _, err := run("--layout", layoutPath, "attach", "--create-background", name); err != nil {
		return err
	}
	return nil
}

// AttachCmd returns the command that attaches to ws full-screen. Ensure must
// have been called first so the session exists.
func (b *Backend) AttachCmd(ws workspace.Workspace) (*exec.Cmd, error) {
	return exec.Command("zellij", "attach", SessionName(ws)), nil
}

// AddSession adds a tab to the running session for ws.
func (b *Backend) AddSession(ws workspace.Workspace, s workspace.Session) error {
	name := SessionName(ws)
	live, err := b.live(name)
	if err != nil {
		return err
	}
	if !live {
		return fmt.Errorf("workspace %q is not running", name)
	}

	args := []string{"action", "new-tab", "--name", s.Title, "--cwd", ws.Dir}
	if len(s.Argv) > 0 {
		args = append(args, "--")
		args = append(args, s.Argv...)
	}
	cmd := exec.Command("zellij", args...)
	cmd.Env = append(os.Environ(), "ZELLIJ_SESSION_NAME="+name)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("zellij action new-tab: %s", strings.TrimSpace(string(out)))
	}
	return nil
}

// Archive kills the session, leaving the worktree on disk.
func (b *Backend) Archive(ws workspace.Workspace) error {
	name := SessionName(ws)
	live, err := b.live(name)
	if err != nil {
		return err
	}
	if !live {
		return nil
	}
	// kill-session cleanly terminates an active session (exits 0 and removes it,
	// unlike `delete-session --force` which is meant for exited sessions).
	_, err = run("kill-session", name)
	return err
}

// run executes a zellij subcommand and returns combined output.
func run(args ...string) (string, error) {
	cmd := exec.Command("zellij", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("zellij %s: %s", strings.Join(args, " "), strings.TrimSpace(string(out)))
	}
	return string(out), nil
}
