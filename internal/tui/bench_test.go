package tui

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/KCaverly/caretaker/internal/config"
	"github.com/KCaverly/caretaker/internal/session"
)

// benchGit runs a git command in dir, failing the benchmark on error.
func benchGit(b *testing.B, dir string, args ...string) {
	b.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=bench", "GIT_AUTHOR_EMAIL=bench@bench",
		"GIT_COMMITTER_NAME=bench", "GIT_COMMITTER_EMAIL=bench@bench",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		b.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// benchRoot builds a repos root of nRepos git repositories with nWorktrees
// linked worktrees each (plus main), approximating a real ct root.
func benchRoot(b *testing.B, nRepos, nWorktrees int) string {
	b.Helper()
	root := b.TempDir()
	for i := 0; i < nRepos; i++ {
		rp := filepath.Join(root, fmt.Sprintf("repo-%02d", i))
		if err := os.MkdirAll(rp, 0o755); err != nil {
			b.Fatal(err)
		}
		benchGit(b, rp, "init", "-q", "-b", "main")
		if err := os.WriteFile(filepath.Join(rp, "README.md"), []byte("bench\n"), 0o644); err != nil {
			b.Fatal(err)
		}
		benchGit(b, rp, "add", ".")
		benchGit(b, rp, "commit", "-q", "-m", "init")
		for j := 0; j < nWorktrees; j++ {
			name := fmt.Sprintf("feat-%02d", j)
			benchGit(b, rp, "worktree", "add", "-q", "-b", name, filepath.Join(rp, ".worktrees", name))
		}
	}
	return root
}

// BenchmarkControllerLoad measures one full deck refresh — repo discovery plus
// per-worktree git status — over a root of 8 repos × (main + 3 worktrees).
// This is the work behind startup, every workspace activation, create/remove,
// and the picker's `r` refresh.
func BenchmarkControllerLoad(b *testing.B) {
	root := benchRoot(b, 8, 3)
	c := NewController(config.Config{Root: root})
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		groups, err := c.Load()
		if err != nil {
			b.Fatal(err)
		}
		if len(groups) != 8 {
			b.Fatalf("expected 8 groups, got %d", len(groups))
		}
	}
}

// BenchmarkEnsureProjectTrusted measures the read + full JSON round-trip of a
// ~40KB claude config (the size observed in practice) in the already-trusted
// case — the cost every background-agent launch paid inline on the UI
// goroutine before EnsureHomeDirTrusted cached the verified flag.
func BenchmarkEnsureProjectTrusted(b *testing.B) {
	dir := b.TempDir()
	configPath := filepath.Join(dir, ".claude.json")
	home := "/Users/bench"

	// Build a config of realistic shape/size: many project entries with a few
	// fields each, plus the home project already trusted.
	projects := map[string]any{
		home: map[string]any{"hasTrustDialogAccepted": true},
	}
	for i := 0; i < 60; i++ {
		projects[fmt.Sprintf("/Users/bench/code/project-%02d", i)] = map[string]any{
			"hasTrustDialogAccepted":     i%2 == 0,
			"lastCost":                   float64(i) * 1.7,
			"lastTotalInputTokens":       i * 91234,
			"lastTotalOutputTokens":      i * 4321,
			"lastSessionId":              "aaaaaaaa-bbbb-cccc-dddd-eeeeffff0000",
			"exampleFiles":               []string{"main.go", "internal/app/app.go", "internal/app/app_test.go"},
			"history":                    []string{"fix the flaky test", "add pagination to the list endpoint", "refactor the session manager"},
			"mcpContextUris":             []string{},
			"hasCompletedProjectOnboard": true,
		}
	}
	data, err := json.MarshalIndent(map[string]any{"userID": "bench", "projects": projects}, "", "  ")
	if err != nil {
		b.Fatal(err)
	}
	if err := os.WriteFile(configPath, data, 0o644); err != nil {
		b.Fatal(err)
	}
	b.Logf("config size: %d bytes", len(data))

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if err := ensureProjectTrusted(configPath, home); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkViewBarOnlyFrame measures a full Model.View() for a frame where the
// visible session's screen has NOT changed — the common case behind a status
// poll tick, a usage poll, or a badge update, all of which redraw the whole
// frame while the hosted program sits idle. Before the per-session render cache
// every such frame re-serialised the visible editor's entire vt buffer
// (~60µs/600+ allocs at this size, per BenchmarkEmulatorRender); with the cache
// the session contributes only a cached-string return, so what remains here is
// essentially the bar/chrome cost alone. The editor is filled with styled text
// and then idles (sleep), so every View() in the loop is a session cache hit.
func BenchmarkViewBarOnlyFrame(b *testing.B) {
	m := sampleModel()
	defer m.mgr.CloseAll()

	dir := b.TempDir()
	fill := []string{"sh", "-c",
		`i=0; while [ $i -lt 40 ]; do ` +
			`printf '\033[33mlorem\033[0m \033[1;34mipsum\033[0m dolor sit amet %d\r\n' $i; ` +
			`i=$((i+1)); done; sleep 60`}
	ws, err := m.mgr.Activate("r/w", dir,
		[]session.Spec{{Kind: session.Editor, Argv: fill}}, m.width, m.height-barHeight)
	if err != nil {
		b.Fatal(err)
	}
	m.current = &workspaceRef{repo: "r", worktree: "w", key: "r/w", path: dir, ws: ws}
	m.screen = screenEditor

	// Wait for the fill to land so the benchmarked frames render a busy screen
	// (a hot cache), not a blank one.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && !strings.Contains(ws.Editor.Render(), "lorem") {
		time.Sleep(20 * time.Millisecond)
	}

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = m.View()
	}
}
