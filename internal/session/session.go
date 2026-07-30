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

	// Scrollback view state. scrollOff is how many lines the pane is scrolled
	// back from the live screen; 0 is the live screen and is the only state in
	// which the emulator's own render is shown verbatim. renderCacheOff records
	// which offset renderCache was built for, so a scroll invalidates the cache
	// the same way a write does.
	//
	// anchorLen is the scrollback length observed on the last render while
	// scrolled. New output pushes lines into scrollback, which would slide the
	// viewport across the content it is parked on; carrying the growth into
	// scrollOff holds the view still instead. All four fields are touched only on
	// the UI goroutine (Render and the scroll methods), like renderCache.
	scrollOff      int
	renderCacheOff int
	anchorLen      int
}

// ScrollBy moves the pane's view up (negative delta) or down (positive) through
// its scrollback, clamped to the buffer, and reports whether the offset changed.
//
// Alt-screen programs are left alone: nvim, less and the agent TUIs own the
// viewport, implement their own scrolling, and push nothing into scrollback, so
// scrolling "behind" them would show unrelated history from before they started.
func (s *Session) ScrollBy(delta int) bool {
	if s.emu.IsAltScreen() {
		return false
	}
	return s.setScrollOff(s.scrollOff - delta)
}

// ScrollToBottom returns the pane to the live screen, reporting whether it had
// been scrolled. It is what makes typing an escape hatch: input goes to a program
// whose output the user can then see.
func (s *Session) ScrollToBottom() bool { return s.setScrollOff(0) }

// ScrollOffset is how many lines back from the live screen the pane is showing.
func (s *Session) ScrollOffset() int { return s.scrollOff }

// Scrolled reports whether the pane is showing history rather than live output.
// The UI surfaces this: a parked view of a busy terminal is indistinguishable
// from a hung one otherwise.
func (s *Session) Scrolled() bool { return s.scrollOff > 0 }

func (s *Session) setScrollOff(off int) bool {
	off = min(max(off, 0), s.emu.ScrollbackLen())
	if off == s.scrollOff {
		return false
	}
	s.scrollOff = off
	s.anchorLen = s.emu.ScrollbackLen()
	return true
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
			_, _ = s.emu.Write(buf[:n])
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

// SendKey forwards a key event to the program. Typing also returns a scrolled-back
// pane to the live screen: input the user cannot see the result of is worse than
// losing their scroll position.
func (s *Session) SendKey(k uv.KeyEvent) {
	s.ScrollToBottom()
	s.emu.SendKey(k)
}

// Paste delivers pasted text to the program. The emulator wraps it in the
// bracketed-paste guards (ESC[200~…ESC[201~) when the child has enabled DEC
// private mode 2004, so a multi-line paste is received as one literal block
// rather than as line-by-line keystrokes — nvim and claude both enable the
// mode, and raw bytes would trigger editor auto-indent mangling or a premature
// submit. When the child has not enabled the mode the text is sent as-is.
func (s *Session) Paste(text string) {
	s.ScrollToBottom()
	s.emu.Paste(text)
}

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
	dirty := s.renderCacheDirty.CompareAndSwap(true, false)
	s.holdScrollAnchor()
	// The offset is part of the cache key: scrolling changes the frame without
	// the screen having changed, and a write changes the frame at either offset.
	if dirty || s.renderCacheOff != s.scrollOff {
		s.renderCacheOff = s.scrollOff
		if s.scrollOff > 0 {
			s.renderCache = s.renderScrolledBack(s.scrollOff)
		} else {
			s.renderCache = s.emu.Render()
		}
	}
	return s.renderCache
}

// holdScrollAnchor keeps a scrolled-back view parked on the content it was
// scrolled to while new output arrives. Each line the program emits is pushed
// into scrollback, which moves the newest-line boundary the offset is measured
// from; without compensating, a parked view drifts upward through its own history
// at the speed of the output. Growth is carried into the offset instead.
//
// Once the buffer saturates at its maximum, pushes evict the oldest line and the
// length stops growing, so the drift becomes undetectable and resumes — an
// acceptable limit ten thousand lines back.
func (s *Session) holdScrollAnchor() {
	if s.scrollOff == 0 {
		return
	}
	n := s.emu.ScrollbackLen()
	if grew := n - s.anchorLen; grew > 0 {
		s.scrollOff = min(s.scrollOff+grew, n)
	}
	s.anchorLen = n
}

// renderScrolledBack composites the frame for a scrolled-back view: the last off
// lines of scrollback, then as much of the live screen's top as still fits. That
// ordering is what makes scrolling continuous — at off == 1 the frame is the
// newest scrollback line above all but the last screen row, and it walks upward
// from there.
func (s *Session) renderScrolledBack(off int) string {
	h := s.emu.Height()
	sb := s.emu.Scrollback()
	n := sb.Len()
	if off > n {
		off = n
	}

	lines := make([]string, 0, h)
	// Scrollback is oldest-first, so the visible slice is its tail.
	for i := n - off; i < n && len(lines) < h; i++ {
		lines = append(lines, sb.Line(i).Render())
	}
	// emu.Render() emits one self-contained line per screen row, so the top of the
	// screen is a plain prefix of its split.
	for _, line := range strings.Split(s.emu.Render(), "\n") {
		if len(lines) >= h {
			break
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}

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
	s.scrollForShrink(h)
	s.emu.Resize(w, h)
	// A resize reflows the buffer, so a parked offset no longer points at the same
	// content and may exceed the new scrollback length. Snap to the live screen
	// rather than show an arbitrary window of history.
	s.setScrollOff(0)
	s.renderCacheDirty.Store(true) // resize reshapes the buffer; drop the cache
	_ = pty.Setsize(s.pty, &pty.Winsize{Rows: uint16(h), Cols: uint16(w)})
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

// HasForegroundProcess reports whether the terminal's foreground process group
// differs from the session's initial process group. For an interactive shell,
// that means a foreground job currently owns the terminal. Platform-specific
// implementations query the PTY directly; uncertainty is treated as busy.
func (s *Session) HasForegroundProcess() bool {
	if s == nil || !s.Alive() || s.cmd == nil || s.cmd.Process == nil || s.pty == nil {
		return false
	}
	return s.hasForegroundProcess()
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
		s.stopInputPump()
		s.closeCompanion()
	})
}

// stopInputPump unblocks and terminates the `io.Copy(pty, emu)` input goroutine
// started in StartSpec. It closes the emulator's input pipe writer directly
// rather than calling emu.Close(). emu.Close() sets an unsynchronised `closed`
// bool that the pump's blocked emu.Read() reads the instant it wakes; the two
// race, and they can't be serialised with a mutex because Read must block
// without holding one (exactly why vt's SafeEmulator leaves Read lock-free).
// Closing the input pipe writer instead wakes Read through an ordinary pipe EOF
// and never touches that field, so the pump drains any buffered input and exits
// race-free. The emulator holds no other resource that Close would release — it
// owns no goroutines or file descriptors, only the pipe and GC'd buffers — so
// closing the pipe is a complete teardown of the input path. The type assertion
// is a defensive fallback: InputPipe returns an *io.PipeWriter today, but if a
// future vt stops exposing a closer we degrade to emu.Close() rather than leak
// the goroutine.
func (s *Session) stopInputPump() {
	if c, ok := s.emu.InputPipe().(io.Closer); ok {
		_ = c.Close()
		return
	}
	_ = s.emu.Close()
}

func (s *Session) closeCompanion() {
	s.companionOnce.Do(func() {
		if s.companion != nil {
			_ = s.companion.Close()
		}
	})
}
