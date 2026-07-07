// Package session hosts real interactive programs (nvim, claude, a shell) inside
// ct: each runs on its own pty, with a virtual-terminal emulator maintaining the
// screen so ct can render it beneath the status bar. Sessions persist (and keep
// running) for ct's lifetime; switching views never relaunches them.
package session

import (
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"

	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/vt"
	"github.com/creack/pty"
)

// Kind identifies the type of program a session runs.
type Kind int

const (
	Editor Kind = iota
	Agent
	Terminal
)

// Session is one program running on a pty with a terminal emulator mirroring its
// screen.
type Session struct {
	Kind  Kind
	Title string
	// SessionID is the claude session UUID an Agent session runs under (set by
	// the Manager from the Spec). It lets ct resume the same conversation across
	// runs; empty for non-agent sessions.
	SessionID string

	cmd *exec.Cmd
	pty *os.File
	emu *vt.SafeEmulator

	cursorVisible atomic.Bool
	closed        atomic.Bool
	closeOnce     sync.Once
	dirty         func() // signalled when the screen changes
}

// Start launches argv in dir on a pty sized w×h and returns a running Session.
// dirty is called whenever the program produces output (so ct can repaint).
func Start(kind Kind, title, dir string, argv []string, w, h int, dirty func()) (*Session, error) {
	if len(argv) == 0 {
		shell := os.Getenv("SHELL")
		if shell == "" {
			shell = "/bin/sh"
		}
		argv = []string{shell}
	}

	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "TERM=xterm-256color")
	if kind == Agent {
		// Claude's agent-teams feature auto-detects tmux/iTerm2 from these and
		// would then spawn split-pane teammates outside ct's emulator. Drop them
		// so teams render in-process inside the pane ct controls.
		cmd.Env = dropEnv(cmd.Env, "TMUX", "TERM_PROGRAM")
	}

	w, h = max(w, 1), max(h, 1)
	f, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: uint16(h), Cols: uint16(w)})
	if err != nil {
		return nil, err
	}

	s := &Session{
		Kind:  kind,
		Title: title,
		cmd:   cmd,
		pty:   f,
		emu:   vt.NewSafeEmulator(w, h),
		dirty: dirty,
	}
	s.cursorVisible.Store(true)
	s.emu.SetCallbacks(vt.Callbacks{
		CursorVisibility: func(v bool) { s.cursorVisible.Store(v) },
	})

	go s.pumpOutput()    // pty → emulator (screen)
	go io.Copy(f, s.emu) //nolint:errcheck // emulator(SendKey) → pty (input)

	return s, nil
}

// pumpOutput copies child output into the emulator and signals repaints. When
// the pty closes (child exited or session closed), it reaps the process.
func (s *Session) pumpOutput() {
	buf := make([]byte, 32*1024)
	for {
		n, err := s.pty.Read(buf)
		if n > 0 {
			_, _ = s.emu.Write(buf[:n])
			s.signal()
		}
		if err != nil {
			s.closed.Store(true)
			s.signal()
			_ = s.cmd.Wait()
			return
		}
	}
}

func (s *Session) signal() {
	if s.dirty != nil {
		s.dirty()
	}
}

// WriteInput writes p directly to the pty's stdin, bypassing key encoding.
// Use this to send raw text (e.g. an initial prompt) immediately after spawning.
func (s *Session) WriteInput(p []byte) (int, error) { return s.pty.Write(p) }

// SendKey forwards a key event to the program.
func (s *Session) SendKey(k uv.KeyEvent) { s.emu.SendKey(k) }

// SendMouse forwards a mouse event to the program (the emulator only encodes it
// if the program has requested a mouse mode).
func (s *Session) SendMouse(m uv.MouseEvent) { s.emu.SendMouse(m) }

// Render returns the program's current screen as a styled string.
func (s *Session) Render() string { return s.emu.Render() }

// Cursor returns the program's cursor position and visibility.
func (s *Session) Cursor() (x, y int, visible bool) {
	p := s.emu.CursorPosition()
	return p.X, p.Y, s.cursorVisible.Load()
}

// Resize resizes both the emulator and the pty.
func (s *Session) Resize(w, h int) {
	if w < 1 || h < 1 {
		return
	}
	s.emu.Resize(w, h)
	_ = pty.Setsize(s.pty, &pty.Winsize{Rows: uint16(h), Cols: uint16(w)})
}

// Alive reports whether the program is still running.
func (s *Session) Alive() bool { return !s.closed.Load() }

// Pid returns the program's process id, or 0 if it isn't running. ct uses it to
// match the session against `claude agents --json` entries.
func (s *Session) Pid() int {
	if s.cmd == nil || s.cmd.Process == nil {
		return 0
	}
	return s.cmd.Process.Pid
}

// dropEnv returns env with any "KEY=..." entries for the given keys removed.
func dropEnv(env []string, keys ...string) []string {
	out := env[:0:0]
	for _, e := range env {
		drop := false
		for _, k := range keys {
			if strings.HasPrefix(e, k+"=") {
				drop = true
				break
			}
		}
		if !drop {
			out = append(out, e)
		}
	}
	return out
}

// Close terminates the program and releases its resources.
func (s *Session) Close() {
	s.closeOnce.Do(func() {
		s.closed.Store(true)
		if s.cmd.Process != nil {
			_ = s.cmd.Process.Kill()
		}
		_ = s.pty.Close()
		_ = s.emu.Close()
	})
}
