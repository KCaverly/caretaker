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
	ss1, err := m.Activate("repo/wt", t.TempDir(), specs, 80, 24)
	if err != nil {
		t.Fatal(err)
	}
	ss2, err := m.Activate("repo/wt", t.TempDir(), specs, 80, 24)
	if err != nil {
		t.Fatal(err)
	}
	if &ss1[0] == nil || ss1[0] != ss2[0] {
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
