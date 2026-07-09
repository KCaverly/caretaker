package session

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	uv "github.com/charmbracelet/ultraviolet"
)

// waitForFileEquals polls path until its contents equal want, or fails. Used
// by the paste tests, which capture exactly the bytes a child receives on its
// stdin so the bracketed-paste wrapping can be asserted precisely.
func waitForFileEquals(t *testing.T, path, want string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	var got string
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(path); err == nil {
			got = string(b)
			if got == want {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out; file %s = %q, want %q", path, got, want)
}

// TestSessionPasteRaw verifies that when the child has NOT enabled bracketed
// paste, the pasted text is delivered to its stdin verbatim (unwrapped). The
// child runs in raw mode with echo off so it receives the bytes immediately
// and unmodified, and captures them to a file we inspect.
func TestSessionPasteRaw(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "captured")
	s, err := Start(Terminal, "term", dir,
		[]string{"sh", "-c", "stty raw -echo; printf READY; exec cat > '" + out + "'"},
		80, 24, func(*Session) {})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	waitFor(t, s, "READY") // child up and raw mode in effect

	s.Paste("plain paste")
	waitForFileEquals(t, out, "plain paste")
}

// TestSessionPasteBracketed verifies that when the child has enabled DEC
// private mode 2004 (bracketed paste) — as nvim and claude do — the pasted
// text arrives wrapped in the ESC[200~…ESC[201~ guards, so a multi-line paste
// is treated as one literal block instead of per-line keystrokes.
func TestSessionPasteBracketed(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "captured")
	// printf '\033[?2004h' enables the mode (child output the emulator sees);
	// printf READY afterwards guarantees that, once it renders, the mode-set
	// sequence has already been processed.
	s, err := Start(Terminal, "term", dir,
		[]string{"sh", "-c", `stty raw -echo; printf '\033[?2004h'; printf READY; exec cat > '` + out + `'`},
		80, 24, func(*Session) {})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	waitFor(t, s, "READY")

	s.Paste("alpha\nbeta")
	const start, end = "\x1b[200~", "\x1b[201~"
	waitForFileEquals(t, out, start+"alpha\nbeta"+end)
}

func keyRune(r rune) uv.KeyPressEvent { return uv.KeyPressEvent{Code: r, Text: string(r)} }
func keyEnter() uv.KeyPressEvent      { return uv.KeyPressEvent{Code: uv.KeyEnter} }

// waitFor polls until the session screen contains want, or fails.
func waitFor(t *testing.T, s *Session, want string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(s.Render(), want) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %q; screen was:\n%s", want, s.Render())
}

func TestSessionRendersOutput(t *testing.T) {
	dirty := make(chan struct{}, 16)
	s, err := Start(Terminal, "term", t.TempDir(),
		[]string{"sh", "-c", "echo ct-smoke-output; sleep 2"}, 80, 24,
		func(*Session) {
			select {
			case dirty <- struct{}{}:
			default:
			}
		})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	waitFor(t, s, "ct-smoke-output")

	// A repaint signal should have fired.
	select {
	case <-dirty:
	case <-time.After(time.Second):
		t.Error("expected a dirty signal after output")
	}
}

func TestSessionSendKey(t *testing.T) {
	s, err := Start(Terminal, "term", t.TempDir(), []string{"cat"}, 80, 24, func(*Session) {})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	// cat echoes stdin; send "ping" + enter.
	for _, r := range "ping" {
		s.SendKey(keyRune(r))
	}
	s.SendKey(keyEnter())
	waitFor(t, s, "ping")
}

func TestDirtyOnlyForVisibleSessions(t *testing.T) {
	m := NewManager()
	defer m.CloseAll()

	specs := []Spec{
		{Kind: Terminal, Argv: []string{"cat"}},
		{Kind: Agent, Argv: []string{"cat"}},
	}
	ws, err := m.Activate("r/w", t.TempDir(), specs, 80, 24)
	if err != nil {
		t.Fatal(err)
	}
	term, agent := ws.Terms[0], ws.Agents[0]

	drain := func() {
		for {
			select {
			case <-m.Dirty():
			default:
				return
			}
		}
	}

	// Only the terminal is on screen.
	m.SetVisible(term)
	drain()

	// Output from the invisible agent must not wake the UI.
	_, _ = agent.WriteInput([]byte("off-screen\n"))
	waitFor(t, agent, "off-screen")
	select {
	case <-m.Dirty():
		t.Fatal("dirty signal fired for an invisible session")
	default:
	}

	// Output from the visible terminal must wake it.
	_, _ = term.WriteInput([]byte("on-screen\n"))
	waitFor(t, term, "on-screen")
	select {
	case <-m.Dirty():
	case <-time.After(time.Second):
		t.Fatal("no dirty signal for the visible session")
	}

	// After clearing visibility (picker/overlay shown), nothing wakes the UI.
	m.SetVisible()
	drain()
	_, _ = term.WriteInput([]byte("hidden-now\n"))
	waitFor(t, term, "hidden-now")
	select {
	case <-m.Dirty():
		t.Fatal("dirty signal fired with no visible sessions")
	default:
	}
}

// TestRenderCacheMatchesEmulator proves the cache is transparent: once the
// screen has settled, Session.Render() returns exactly what the uncached
// emu.Render() would, and repeated calls return the identical string.
func TestRenderCacheMatchesEmulator(t *testing.T) {
	s, err := Start(Terminal, "term", t.TempDir(), []string{"cat"}, 80, 24, func(*Session) {})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	_, _ = s.WriteInput([]byte("hello-cache\n"))
	waitFor(t, s, "hello-cache")

	got := s.Render()
	if want := s.emu.Render(); got != want {
		t.Fatalf("cached render differs from emulator:\n got %q\nwant %q", got, want)
	}
	if again := s.Render(); again != got {
		t.Fatalf("repeated cached render differs:\n first %q\nsecond %q", got, again)
	}
}

// TestRenderCacheInvalidatesOnWrite proves new pty output is reflected on the
// next Render() rather than being masked by a stale cache.
func TestRenderCacheInvalidatesOnWrite(t *testing.T) {
	s, err := Start(Terminal, "term", t.TempDir(), []string{"cat"}, 80, 24, func(*Session) {})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	_, _ = s.WriteInput([]byte("first-line\n"))
	waitFor(t, s, "first-line")
	before := s.Render()

	_, _ = s.WriteInput([]byte("second-line\n"))
	waitFor(t, s, "second-line") // waitFor renders, so it must observe the write
	after := s.Render()
	if after == before {
		t.Fatal("render did not change after a new write (stale cache)")
	}
	if want := s.emu.Render(); after != want {
		t.Fatalf("post-write render differs from emulator:\n got %q\nwant %q", after, want)
	}
}

// TestRenderCacheInvalidatesOnResize proves a resize (which reshapes the buffer
// with no pty output) drops the cache: the next Render() reflects the new size
// rather than replaying the pre-resize serialisation.
func TestRenderCacheInvalidatesOnResize(t *testing.T) {
	s, err := Start(Terminal, "term", t.TempDir(), []string{"cat"}, 80, 24, func(*Session) {})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	_, _ = s.WriteInput([]byte("resize-me\n"))
	waitFor(t, s, "resize-me")
	before := s.Render() // primes the cache at 80x24

	s.Resize(40, 10)
	after := s.Render()
	if after == before {
		t.Fatal("render did not change after resize (stale cache)")
	}
	if want := s.emu.Render(); after != want {
		t.Fatalf("post-resize render differs from emulator:\n got %q\nwant %q", after, want)
	}
}

// TestRenderCacheNoStaleFrameUnderRacingWrites hammers pty output while a
// separate goroutine renders in a loop (the UI goroutine's role), then — once
// writes and the render loop have quiesced — asserts the final Render()
// reflects the final screen. Meant to run under -race: it exercises the pump
// goroutine's renderCacheDirty store against the render goroutine's load, and
// catches an ordering bug that would leave a stale frame latched. Rendering is
// confined to the single loop goroutine (matching the real single-UI-goroutine
// invariant) so renderCache itself is never touched concurrently.
func TestRenderCacheNoStaleFrameUnderRacingWrites(t *testing.T) {
	s, err := Start(Terminal, "term", t.TempDir(), []string{"cat"}, 80, 24, func(*Session) {})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
				_ = s.Render()
			}
		}
	}()

	for i := 0; i < 500; i++ {
		_, _ = s.WriteInput([]byte(fmt.Sprintf("line-%04d\n", i)))
	}
	_, _ = s.WriteInput([]byte("FINAL-MARKER\n"))

	close(stop)
	<-done // render loop stopped: the main goroutine is now the sole renderer

	// Poll to let the final write drain through the pump, then assert the cache
	// reflects the last content and matches the emulator exactly.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && !strings.Contains(s.Render(), "FINAL-MARKER") {
		time.Sleep(20 * time.Millisecond)
	}
	got := s.Render()
	if !strings.Contains(got, "FINAL-MARKER") {
		t.Fatalf("final render missing the last write:\n%s", got)
	}
	if want := s.emu.Render(); got != want {
		t.Fatalf("final render is stale relative to emulator:\n got %q\nwant %q", got, want)
	}
}

func TestManagerActivateReuses(t *testing.T) {
	m := NewManager()
	defer m.CloseAll()

	specs := []Spec{{Kind: Terminal, Title: "term", Argv: []string{"sh", "-c", "sleep 5"}}}
	ws1, err := m.Activate("repo/wt", t.TempDir(), specs, 80, 24)
	if err != nil {
		t.Fatal(err)
	}
	ws2, err := m.Activate("repo/wt", t.TempDir(), specs, 80, 24)
	if err != nil {
		t.Fatal(err)
	}
	if len(ws1.Terms) == 0 || len(ws2.Terms) == 0 || ws1.Terms[0] != ws2.Terms[0] {
		t.Fatal("Activate should reuse existing sessions, not relaunch")
	}
	if !m.Has("repo/wt") {
		t.Fatal("manager should report the workspace as active")
	}

	m.Close("repo/wt")
	if m.Has("repo/wt") {
		t.Fatal("workspace should be gone after Close")
	}
}

func TestActivateResizesStaleWorkspace(t *testing.T) {
	m := NewManager()
	defer m.CloseAll()

	specs := []Spec{
		{Kind: Editor, Argv: []string{"sleep", "5"}},
		{Kind: Terminal, Argv: []string{"sleep", "5"}},
	}
	if _, err := m.Activate("r/w", t.TempDir(), specs, 80, 24); err != nil {
		t.Fatal(err)
	}

	// The terminal was resized while this workspace sat in the background;
	// re-activating it at the new size must bring its sessions up to date.
	ws, err := m.Activate("r/w", t.TempDir(), specs, 100, 30)
	if err != nil {
		t.Fatal(err)
	}
	if w, h := ws.Editor.Size(); w != 100 || h != 30 {
		t.Fatalf("stale editor not resized on activate: %dx%d, want 100x30", w, h)
	}
	if w, h := ws.Terms[0].Size(); w != 100 || h != 30 {
		t.Fatalf("stale term pane not resized on activate: %dx%d, want 100x30", w, h)
	}
}

func TestManagerSpawnAndCloseAgent(t *testing.T) {
	m := NewManager()
	defer m.CloseAll()

	sleep := []string{"sh", "-c", "sleep 5"}
	specs := []Spec{
		{Kind: Editor, Argv: sleep},
		{Kind: Agent, Argv: sleep},
		{Kind: Terminal, Argv: sleep},
	}
	ws, err := m.Activate("r/w", t.TempDir(), specs, 80, 24)
	if err != nil {
		t.Fatal(err)
	}
	if ws.Editor == nil || len(ws.Terms) == 0 || len(ws.Agents) != 1 {
		t.Fatalf("activate should assign editor/term and one agent, got %+v", ws)
	}

	// Spawning a second and third agent focuses the newest.
	if _, err := m.SpawnAgent("r/w", t.TempDir(), Spec{Kind: Agent, Argv: sleep}, 80, 24); err != nil {
		t.Fatal(err)
	}
	if _, err := m.SpawnAgent("r/w", t.TempDir(), Spec{Kind: Agent, Argv: sleep}, 80, 24); err != nil {
		t.Fatal(err)
	}
	if len(ws.Agents) != 3 || ws.ActiveAgent != 2 {
		t.Fatalf("expected 3 agents with active=2, got %d active=%d", len(ws.Agents), ws.ActiveAgent)
	}

	// Closing the focused (last) agent clamps the active index.
	m.CloseAgent("r/w", 2)
	if len(ws.Agents) != 2 || ws.ActiveAgent != 1 {
		t.Fatalf("after close: %d agents active=%d, want 2 active=1", len(ws.Agents), ws.ActiveAgent)
	}

	// Closing the first agent shifts the slice; active clamps within range.
	m.CloseAgent("r/w", 0)
	if len(ws.Agents) != 1 || ws.ActiveAgent != 0 {
		t.Fatalf("after close: %d agents active=%d, want 1 active=0", len(ws.Agents), ws.ActiveAgent)
	}

	// Spawning into a non-existent workspace errors.
	if _, err := m.SpawnAgent("nope", t.TempDir(), Spec{Kind: Agent, Argv: sleep}, 80, 24); err == nil {
		t.Error("expected error spawning into an inactive workspace")
	}
}
