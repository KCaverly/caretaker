package session

import (
	"fmt"
	"strings"
	"testing"

	"github.com/charmbracelet/x/vt"
)

// resizeForWindowMsg mirrors what the TUI does on tea.WindowSizeMsg. It lives
// here (not inline in the benchmark) so the pre/post comparison changes only
// this one function when the resize strategy changes.
func resizeForWindowMsg(m *Manager, currentKey string, w, h int) {
	m.ResizeWorkspace(currentKey, w, h)
}

// BenchmarkWindowResize measures the synchronous work one tea.WindowSizeMsg
// triggers on the UI goroutine with 6 open workspaces (editor + 2 agents +
// 1 terminal each): the manager resize plus the current workspace's pane
// recompute. Sizes alternate so every iteration performs real emulator
// resizes and pty ioctls rather than no-ops.
func BenchmarkWindowResize(b *testing.B) {
	m := NewManager()
	defer m.CloseAll()

	sleep := []string{"sleep", "60"}
	specs := []Spec{
		{Kind: Editor, Argv: sleep},
		{Kind: Agent, Argv: sleep},
		{Kind: Agent, Argv: sleep},
		{Kind: Terminal, Argv: sleep},
	}
	keys := make([]string, 6)
	for i := range keys {
		keys[i] = fmt.Sprintf("repo/wt-%d", i)
		if _, err := m.Activate(keys[i], b.TempDir(), specs, 120, 40); err != nil {
			b.Fatal(err)
		}
	}
	current := keys[0]

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		w := 120 + i%2
		resizeForWindowMsg(m, current, w, 40)
	}
}

// BenchmarkSetVisible measures the per-message cost of SetVisible in the two
// cases the UI hits. "unchanged" is the overwhelmingly common one: Model.Update
// calls SetVisible after every message, and the visible set is the same on
// every dirtyMsg repaint under sustained output — that path must not lock or
// allocate. "changed" alternates the set each call so every iteration pays the
// full map rebuild under visMu (the screen/pane switch cost).
func BenchmarkSetVisible(b *testing.B) {
	m := NewManager()
	defer m.CloseAll()
	sleep := []string{"sleep", "60"}
	ws, err := m.Activate("r/w", b.TempDir(), []Spec{
		{Kind: Editor, Argv: sleep},
		{Kind: Agent, Argv: sleep},
	}, 120, 40)
	if err != nil {
		b.Fatal(err)
	}
	editor, agent := ws.Editor, ws.Agents[0]

	b.Run("unchanged", func(b *testing.B) {
		m.SetVisible(editor)
		b.ResetTimer()
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			m.SetVisible(editor)
		}
	})
	b.Run("changed", func(b *testing.B) {
		b.ResetTimer()
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if i%2 == 0 {
				m.SetVisible(editor)
			} else {
				m.SetVisible(agent)
			}
		}
	})
}

// BenchmarkWorkspaceActivate measures one full Manager.Activate for a fresh
// workspace — the synchronous UI-goroutine work behind opening a worktree,
// creating one, or switching to a not-yet-active one for the first time. The
// spec set mirrors a real activation: editor + 2 agents + terminal, each a
// process spawned on its own pty. This is the latency the user waits through
// before the editor appears.
func BenchmarkWorkspaceActivate(b *testing.B) {
	sleep := []string{"sleep", "60"}
	specs := []Spec{
		{Kind: Editor, Argv: sleep},
		{Kind: Agent, Argv: sleep},
		{Kind: Agent, Argv: sleep},
		{Kind: Terminal, Argv: sleep},
	}
	dir := b.TempDir()

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		m := NewManager()
		key := fmt.Sprintf("repo/wt-%d", i)
		if _, err := m.Activate(key, dir, specs, 120, 40); err != nil {
			b.Fatal(err)
		}
		b.StopTimer()
		m.CloseAll()
		b.StartTimer()
	}
}

// BenchmarkBuildEnv measures buildEnv in isolation — the per-session
// environment construction StartSpec runs for every pane it spawns (editor,
// each agent, terminal). It snapshots os.Environ() and applies the TERM default
// plus provider env changes; a workspace activation pays it once per session.
func BenchmarkBuildEnv(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = buildEnv([]string{"FOO=bar"}, []string{"TMUX", "TERM_PROGRAM"})
	}
}

// BenchmarkEmulatorRender measures the cost of serialising a full emulator
// screen to a styled string — the work every repaint pays per visible session.
// This is what makes dropping repaints for invisible sessions worthwhile: the
// vt layer re-renders the whole buffer (w×h cells) on every call, regardless
// of how much changed.
func BenchmarkEmulatorRender(b *testing.B) {
	for _, size := range []struct{ w, h int }{{80, 24}, {120, 40}, {200, 50}} {
		b.Run(fmt.Sprintf("%dx%d", size.w, size.h), func(b *testing.B) {
			emu := vt.NewSafeEmulator(size.w, size.h)
			// Fill the screen with styled text so cells carry SGR attributes,
			// approximating a busy TUI program (editor/agent) rather than
			// blank cells.
			line := strings.Repeat("\x1b[33mlorem\x1b[0m \x1b[1;34mipsum\x1b[0m dolor ", 12)
			for i := 0; i < size.h; i++ {
				_, _ = emu.Write([]byte(line + "\r\n"))
			}
			b.ResetTimer()
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				_ = emu.Render()
			}
		})
	}
}

// BenchmarkSessionRender measures Session.Render() with the render cache in
// place, over the two paths that matter:
//
//   - hit:  repeated Render() with no intervening write — what every frame not
//     driven by this session's own output now costs (a bar poll tick, a badge
//     update, another pane's write). This should be near-free.
//   - miss: a write invalidates the cache before each Render(), so every call
//     re-serialises — the unavoidable cost when this session is the one
//     producing output. This should match BenchmarkEmulatorRender.
//
// The gap between the two is the win: a cache hit replaces a full w×h buffer
// serialisation with a flag check and a string return.
func BenchmarkSessionRender(b *testing.B) {
	for _, size := range []struct{ w, h int }{{80, 24}, {120, 40}, {200, 50}} {
		emu := vt.NewSafeEmulator(size.w, size.h)
		line := strings.Repeat("\x1b[33mlorem\x1b[0m \x1b[1;34mipsum\x1b[0m dolor ", 12)
		for i := 0; i < size.h; i++ {
			_, _ = emu.Write([]byte(line + "\r\n"))
		}
		s := &Session{emu: emu}
		s.renderCacheDirty.Store(true)
		_ = s.Render() // prime the cache

		b.Run(fmt.Sprintf("hit/%dx%d", size.w, size.h), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				_ = s.Render()
			}
		})
		b.Run(fmt.Sprintf("miss/%dx%d", size.w, size.h), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				s.renderCacheDirty.Store(true)
				_ = s.Render()
			}
		})
	}
}
