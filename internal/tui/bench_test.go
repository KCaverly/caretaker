package tui

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

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
