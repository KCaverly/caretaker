package session

import (
	"strings"
	"testing"
	"time"

	uv "github.com/charmbracelet/ultraviolet"
)

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
		func() {
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
	s, err := Start(Terminal, "term", t.TempDir(), []string{"cat"}, 80, 24, func() {})
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
	if ws1.Term == nil || ws1.Term != ws2.Term {
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
	if ws.Editor == nil || ws.Term == nil || len(ws.Agents) != 1 {
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
