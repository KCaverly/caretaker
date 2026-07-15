// Package session hosts real interactive programs (nvim, claude, a shell) inside
// ct: each runs on its own pty, with a virtual-terminal emulator maintaining the
// screen so ct can render it beneath the status bar. Sessions persist (and keep
// running) for ct's lifetime; switching views never relaunches them.
package session

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/KCaverly/caretaker/internal/agent"
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
	// Provider identifies the CLI that owns an agent session. It is empty for
	// non-agent sessions and legacy agent specs that predate provider metadata.
	Provider agent.Provider
	// SessionID is the provider-owned, opaque conversation ID an Agent session
	// runs under. It lets ct resume the same conversation across runs; empty for
	// non-agent sessions and conversations whose provider has not supplied an ID.
	SessionID string
	// Events carries lifecycle notifications from the provider integration.
	Events <-chan agent.Event

	cmd *exec.Cmd
	pty *os.File
	emu *vt.SafeEmulator

	cursorVisible atomic.Bool
	closed        atomic.Bool
	closeOnce     sync.Once
	companionOnce sync.Once
	emuMu         sync.Mutex     // keeps a scrollback snapshot coherent with pty output
	dirty         func(*Session) // signalled when the screen changes
	companion     io.Closer

	// Render cache. emu.Render() re-serialises the entire w×h buffer to an ANSI
	// string on every call (~60µs/616 allocs at 80×24 up to ~253µs/1,901 allocs
	// at 200×50), so a frame triggered by anything other than this session's own
	// output — a bar poll tick, a badge update, another pane's write — must not
	// pay that cost. renderCache holds the last serialisation and is returned
	// while the screen is unchanged.
	//
	// The screen only changes via emu.Write (the pty pump) and emu.Resize;
	// SendKey/SendMouse/Paste write to the child's input pipe, not the screen
	// (their echo returns through the pty as ordinary output). So renderCacheDirty
	// is set in exactly those two places. The cursor is queried separately, per
	// frame, via Cursor()/emu.CursorPosition() and is never part of the cached
	// string, so caching cannot stale the cursor.
	//
	// Concurrency: renderCache is read and written only on the UI goroutine
	// (Render and Resize), so it needs no lock of its own. renderCacheDirty is
	// the sole cross-goroutine handshake — the pty pump goroutine sets it after
	// each emu.Write — so it is atomic. See Render for the set/clear ordering.
	renderCache      string
	renderCacheDirty atomic.Bool
	// scrollOffset is the number of rows the rendered viewport sits above the
	// live bottom. It is owned by the UI goroutine.
	scrollOffset int
}

// Start launches argv in dir on a pty sized w×h and returns a running Session.
// dirty is called with the session whenever the program produces output, so the
// caller can decide whether a repaint is needed (e.g. only for visible sessions).
func Start(kind Kind, title, dir string, argv []string, w, h int, dirty func(*Session)) (*Session, error) {
	return StartSpec(Spec{Kind: kind, Title: title, Argv: argv}, dir, w, h, dirty)
}

// StartSpec launches spec in dir on a pty sized w×h. In addition to the
// command, it applies provider-specific environment changes and propagates
// agent metadata to the returned Session.
func StartSpec(spec Spec, dir string, w, h int, dirty func(*Session)) (*Session, error) {
	if len(spec.Argv) == 0 && spec.Kind == Agent {
		if spec.Companion != nil {
			_ = spec.Companion.Close()
		}
		return nil, errEmptyAgentArgv
	}
	if len(spec.Argv) == 0 {
		shell := os.Getenv("SHELL")
		if shell == "" {
			shell = "/bin/sh"
		}
		spec.Argv = []string{shell}
	}

	cmd := exec.Command(spec.Argv[0], spec.Argv[1:]...)
	cmd.Dir = dir
	cmd.Env = buildEnv(spec.Env, spec.UnsetEnv)

	w, h = max(w, 1), max(h, 1)
	f, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: uint16(h), Cols: uint16(w)})
	if err != nil {
		if spec.Companion != nil {
			_ = spec.Companion.Close()
		}
		return nil, err
	}

	s := &Session{
		Kind:      spec.Kind,
		Title:     spec.Title,
		Provider:  spec.Provider,
		SessionID: spec.SessionID,
		Events:    spec.Events,
		cmd:       cmd,
		pty:       f,
		emu:       vt.NewSafeEmulator(w, h),
		dirty:     dirty,
		companion: spec.Companion,
	}
	s.cursorVisible.Store(true)
	s.renderCacheDirty.Store(true) // no cache yet: first Render must serialise
	s.emu.SetCallbacks(vt.Callbacks{
		CursorVisibility: func(v bool) { s.cursorVisible.Store(v) },
	})

	go s.pumpOutput()    // pty → emulator (screen)
	go io.Copy(f, s.emu) //nolint:errcheck // emulator(SendKey) → pty (input)

	return s, nil
}

// buildEnv returns the current process environment with caretaker's terminal
// default and the spec's provider-specific changes applied. Explicit Env
// entries replace inherited entries with the same key; UnsetEnv wins over both
// inherited and explicit entries.
func buildEnv(set, unset []string) []string {
	env := append([]string(nil), os.Environ()...)
	env = upsertEnv(env, "TERM=xterm-256color")
	for _, entry := range set {
		env = upsertEnv(env, entry)
	}
	return dropEnv(env, unset...)
}

// upsertEnv appends entry after removing existing entries for its key. Invalid
// entries are left for os/exec to reject rather than silently changing the
// caller's requested environment.
func upsertEnv(env []string, entry string) []string {
	key, _, ok := strings.Cut(entry, "=")
	if !ok || key == "" {
		return append(env, entry)
	}
	return append(dropEnv(env, key), entry)
}

// pumpOutput copies child output into the emulator and signals repaints. When
// the pty closes (child exited or session closed), it reaps the process.
func (s *Session) pumpOutput() {
	buf := make([]byte, 32*1024)
	for {
		n, err := s.pty.Read(buf)
		if n > 0 {
			s.emuMu.Lock()
			_, _ = s.emu.Write(buf[:n])
			s.emuMu.Unlock()
			// Mark the cache stale BEFORE signalling the repaint, so the frame
			// this write triggers never serves the pre-write screen.
			s.renderCacheDirty.Store(true)
			s.signal()
		}
		if err != nil {
			s.closed.Store(true)
			s.signal()
			_ = s.cmd.Wait()
			s.closeCompanion()
			return
		}
	}
}

func (s *Session) signal() {
	if s.dirty != nil {
		s.dirty(s)
	}
}

// WriteInput writes p directly to the pty's stdin, bypassing key encoding.
// Use this to send raw text (e.g. an initial prompt) immediately after spawning.
func (s *Session) WriteInput(p []byte) (int, error) { return s.pty.Write(p) }

// SendKey forwards a key event to the program.
func (s *Session) SendKey(k uv.KeyEvent) { s.emu.SendKey(k) }

// Paste delivers pasted text to the program. The emulator wraps it in the
// bracketed-paste guards (ESC[200~…ESC[201~) when the child has enabled DEC
// private mode 2004, so a multi-line paste is received as one literal block
// rather than as line-by-line keystrokes — nvim and claude both enable the
// mode, and raw bytes would trigger editor auto-indent mangling or a premature
// submit. When the child has not enabled the mode the text is sent as-is.
func (s *Session) Paste(text string) { s.emu.Paste(text) }

// SendMouse forwards a mouse event to the program (the emulator only encodes it
// if the program has requested a mouse mode).
func (s *Session) SendMouse(m uv.MouseEvent) { s.emu.SendMouse(m) }

// Render returns the program's current screen as a styled string, reusing the
// cached serialisation while the screen is unchanged (see the renderCache
// field). Called only on the UI goroutine.
//
// The dirty flag is cleared BEFORE serialising, not after: a pty write that
// lands mid-render re-sets the flag, so the next frame re-serialises and never
// serves a screen that predates that write. Clearing after Render could instead
// swallow such a write (we would read the pre-write buffer, then clear the flag
// the write had set) and leave the stale frame on screen until the next write.
// The CompareAndSwap collapses a burst of writes since the last frame into a
// single re-serialisation.
func (s *Session) Render() string {
	s.emuMu.Lock()
	defer s.emuMu.Unlock()
	if s.scrollOffset > 0 {
		return s.renderScrollbackLocked()
	}
	if s.renderCacheDirty.CompareAndSwap(true, false) {
		s.renderCache = s.emu.Render()
	}
	return s.renderCache
}

// Cursor returns the program's cursor position and visibility.
func (s *Session) Cursor() (x, y int, visible bool) {
	s.emuMu.Lock()
	defer s.emuMu.Unlock()
	if s.scrollOffset > 0 {
		return 0, 0, false
	}
	p := s.emu.CursorPosition()
	return p.X, p.Y, s.cursorVisible.Load()
}

// Resize resizes both the emulator and the pty.
func (s *Session) Resize(w, h int) {
	if w < 1 || h < 1 {
		return
	}
	s.emuMu.Lock()
	s.scrollForShrink(h)
	s.emu.Resize(w, h)
	s.scrollOffset = min(s.scrollOffset, s.emu.ScrollbackLen())
	s.emuMu.Unlock()
	s.renderCacheDirty.Store(true) // resize reshapes the buffer; drop the cache
	_ = pty.Setsize(s.pty, &pty.Winsize{Rows: uint16(h), Cols: uint16(w)})
}

// Scroll moves the rendered viewport through primary-screen scrollback. A
// positive number moves toward older output; a negative number moves back to
// the live bottom. It returns false when this session has no scrollback to
// show (for example an alternate-screen TUI), allowing callers to pass the
// wheel event on to a program that handles mouse input itself.
func (s *Session) Scroll(rows int) bool {
	if rows == 0 {
		return false
	}
	s.emuMu.Lock()
	defer s.emuMu.Unlock()
	if s.emu.IsAltScreen() {
		return false
	}
	maxOffset := s.emu.ScrollbackLen()
	if maxOffset == 0 {
		return false
	}
	s.scrollOffset = min(max(s.scrollOffset+rows, 0), maxOffset)
	return true
}

// renderScrollbackLocked renders one terminal-height window from the combined
// primary-screen scrollback and live screen. emuMu must be held by the caller.
func (s *Session) renderScrollbackLocked() string {
	w, h := s.emu.Width(), s.emu.Height()
	sb := s.emu.Scrollback()
	sbLen := 0
	if sb != nil {
		sbLen = sb.Len()
	}
	total := sbLen + h
	start := max(total-h-s.scrollOffset, 0)
	var out strings.Builder
	for y := range h {
		if y > 0 {
			out.WriteByte('\n')
		}
		lineIndex := start + y
		if lineIndex < sbLen {
			out.WriteString(sb.Line(lineIndex).Render())
			continue
		}
		line := make(uv.Line, w)
		for x := range w {
			if cell := s.emu.CellAt(x, lineIndex-sbLen); cell != nil {
				line[x] = *cell
			}
		}
		out.WriteString(line.Render())
	}
	return out.String()
}

// scrollForShrink keeps the newest output visible when the pane loses rows
// (splitting, unzooming). The emulator's Resize truncates the buffer from the
// BOTTOM, which would destroy the most recent screenful and leave the oldest;
// a real terminal instead scrolls content up into scrollback so the cursor's
// line survives. Emulate that here: scroll up (CSI S — which also pushes the
// evicted top lines into the emulator's scrollback) just enough that the
// cursor row lands inside the new height, where Resize's cursor clamp will
// then agree with the content. Alt-screen programs (nvim, claude) repaint on
// SIGWINCH and have no scrollback semantics, so they are left alone. Growing
// back does not restore the scrolled-off lines — like a terminal that doesn't
// re-fill from scrollback, the content simply stays where it is.
func (s *Session) scrollForShrink(newH int) {
	if s.emu.IsAltScreen() {
		return
	}
	shift := s.emu.CursorPosition().Y - (newH - 1)
	if shift <= 0 {
		return
	}
	_, _ = fmt.Fprintf(s.emu, "\x1b[%dS", shift)
}

// Size returns the emulator's current dimensions.
func (s *Session) Size() (w, h int) { return s.emu.Width(), s.emu.Height() }

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
		s.closeCompanion()
	})
}

func (s *Session) closeCompanion() {
	s.companionOnce.Do(func() {
		if s.companion != nil {
			_ = s.companion.Close()
		}
	})
}
