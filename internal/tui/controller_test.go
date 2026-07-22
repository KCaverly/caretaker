package tui

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/KCaverly/caretaker/internal/agent"
	"github.com/KCaverly/caretaker/internal/config"
)

func TestParseAgentStatuses(t *testing.T) {
	data := []byte(`[
		{"pid":1480,"cwd":"/r/a","kind":"interactive","status":"busy","startedAt":1781790525707},
		{"pid":43053,"cwd":"/r/b","kind":"interactive","status":"idle"},
		{"pid":900,"cwd":"/r/c","kind":"interactive","status":"waiting","waitingFor":"permission prompt"},
		{"cwd":"/r/d","kind":"interactive","state":"done"}
	]`)

	got, err := parseAgentStatuses(data)
	if err != nil {
		t.Fatal(err)
	}
	// The pidless entry (background-only) is skipped.
	if len(got) != 3 {
		t.Fatalf("expected 3 live entries, got %d: %+v", len(got), got)
	}
	if got[1480].Status != "busy" || got[1480].Cwd != "/r/a" || got[1480].StartedAt != 1781790525707 {
		t.Errorf("busy entry parsed wrong: %+v", got[1480])
	}
	if got[43053].Status != "idle" {
		t.Errorf("idle entry parsed wrong: %+v", got[43053])
	}
	if got[900].Status != "waiting" || got[900].WaitingFor != "permission prompt" {
		t.Errorf("waiting entry parsed wrong: %+v", got[900])
	}
}

func TestParseAgentStatusesBadJSON(t *testing.T) {
	if _, err := parseAgentStatuses([]byte("not json")); err == nil {
		t.Error("expected an error for malformed JSON")
	}
}

func TestNewAgentSpecLabels(t *testing.T) {
	c := &Controller{}
	c.cfg.Agent = "claude"

	unnamed := c.NewAgentSpec("")
	if unnamed.SessionID == "" {
		t.Fatal("new agent should carry a generated session id")
	}
	wantUnnamed := []string{"claude", "--session-id", unnamed.SessionID, "--teammate-mode", "in-process"}
	if got := unnamed.Argv; !equalStrings(got, wantUnnamed) {
		t.Errorf("unnamed argv = %v, want %v", got, wantUnnamed)
	}
	if unnamed.Title != "claude" {
		t.Errorf("unnamed title = %q", unnamed.Title)
	}
	if unnamed.Provider != agent.Claude {
		t.Errorf("unnamed provider = %q, want claude", unnamed.Provider)
	}
	if !equalStrings(unnamed.UnsetEnv, []string{"TMUX", "TERM_PROGRAM"}) {
		t.Errorf("unnamed unset env = %v", unnamed.UnsetEnv)
	}

	named := c.NewAgentSpec("refactor auth")
	want := []string{"claude", "--session-id", named.SessionID, "--teammate-mode", "in-process", "-n", "refactor auth"}
	if got := named.Argv; !equalStrings(got, want) {
		t.Errorf("named argv = %v, want %v", got, want)
	}
	if named.Title != "refactor auth" {
		t.Errorf("named title = %q", named.Title)
	}

	// Each new agent gets a distinct session id.
	if a, b := c.NewAgentSpec(""), c.NewAgentSpec(""); a.SessionID == b.SessionID {
		t.Error("two new agents shared a session id")
	}
}

func TestControllerLegacyAgentProviderDefaults(t *testing.T) {
	c := NewController(config.Config{Agent: "custom-claude"})
	if got := c.DefaultAgentProvider(); got != agent.Claude {
		t.Fatalf("default provider = %q, want claude", got)
	}
	got := c.EnabledAgentProviders()
	if !equalProviders(got, []agent.Provider{agent.Claude}) {
		t.Fatalf("enabled providers = %v, want [claude]", got)
	}
	got[0] = agent.Codex
	if got := c.EnabledAgentProviders(); !equalProviders(got, []agent.Provider{agent.Claude}) {
		t.Fatal("EnabledAgentProviders returned controller-owned storage")
	}
	if got := c.NewAgentSpec("").Argv[0]; got != "custom-claude" {
		t.Fatalf("legacy Claude command = %q, want custom-claude", got)
	}

	// Direct literal controllers used by focused tests receive the same
	// provider/default fallbacks through the accessors and builders.
	direct := &Controller{}
	direct.cfg.Agent = "literal-claude"
	if got := direct.DefaultAgentProvider(); got != agent.Claude {
		t.Fatalf("direct default provider = %q, want claude", got)
	}
	if got := direct.NewAgentSpec("").Argv[0]; got != "literal-claude" {
		t.Fatalf("direct legacy command = %q, want literal-claude", got)
	}
}

func TestProviderAgentSpecs(t *testing.T) {
	projects := t.TempDir()
	c := NewController(config.Config{
		Agent: "legacy-claude",
		Agents: config.Agents{
			Default: agent.Codex,
			Enabled: []agent.Provider{agent.Codex, agent.Claude},
			Claude:  config.AgentProvider{Command: "configured-claude", Args: []string{"--claude-base"}},
			Codex:   config.AgentProvider{Command: "configured-codex", Args: []string{"--codex-base"}},
		},
	})
	c.projectsDir = projects

	if got := c.DefaultAgentProvider(); got != agent.Codex {
		t.Fatalf("default provider = %q, want codex", got)
	}
	if got := c.EnabledAgentProviders(); !equalProviders(got, []agent.Provider{agent.Codex, agent.Claude}) {
		t.Fatalf("enabled providers = %v", got)
	}

	t.Run("claude", func(t *testing.T) {
		spec, err := c.NewProviderAgentSpec(agent.Claude, "amber-fox", "fix the tests")
		if err != nil {
			t.Fatal(err)
		}
		want := []string{"configured-claude", "--claude-base", "--session-id", spec.SessionID, "--teammate-mode", "in-process", "-n", "amber-fox", "fix the tests"}
		if !equalStrings(spec.Argv, want) {
			t.Errorf("argv = %v, want %v", spec.Argv, want)
		}
		if spec.Provider != agent.Claude || spec.SessionID == "" {
			t.Errorf("metadata = provider %q id %q", spec.Provider, spec.SessionID)
		}
		if !equalStrings(spec.UnsetEnv, []string{"TMUX", "TERM_PROGRAM"}) {
			t.Errorf("unset env = %v", spec.UnsetEnv)
		}
	})

	t.Run("codex", func(t *testing.T) {
		spec, err := c.NewProviderAgentSpec(agent.Codex, "amber-fox", "fix the tests")
		if err != nil {
			t.Fatal(err)
		}
		want := []string{"configured-codex", "--codex-base", "fix the tests"}
		if !equalStrings(spec.Argv, want) {
			t.Errorf("argv = %v, want %v", spec.Argv, want)
		}
		if spec.Provider != agent.Codex || spec.SessionID != "" {
			t.Errorf("metadata = provider %q id %q", spec.Provider, spec.SessionID)
		}
		if len(spec.UnsetEnv) != 0 {
			t.Errorf("Codex unset env = %v, want none", spec.UnsetEnv)
		}
		if spec.Title != "amber-fox" {
			t.Errorf("title = %q", spec.Title)
		}
	})
}

func TestRestoreProviderAgentSpecs(t *testing.T) {
	projects := t.TempDir()
	c := NewController(config.Config{Agents: config.Agents{
		Default: agent.Claude,
		Enabled: []agent.Provider{agent.Claude, agent.Codex},
		Claude:  config.AgentProvider{Command: "claude", Args: []string{"--claude-base"}},
		Codex:   config.AgentProvider{Command: "codex", Args: []string{"--codex-base"}},
	}})
	c.projectsDir = projects
	id := "11111111-2222-4333-8444-555555555555"

	// Codex resumes through its resume subcommand.
	codexSpec, err := c.RestoreProviderAgentSpec(agent.Codex, id, "amber-fox", "continue")
	if err != nil {
		t.Fatal(err)
	}
	wantCodex := []string{"codex", "--codex-base", "resume", id, "continue"}
	if !equalStrings(codexSpec.Argv, wantCodex) {
		t.Errorf("Codex resume argv = %v, want %v", codexSpec.Argv, wantCodex)
	}
	if codexSpec.Provider != agent.Codex || codexSpec.SessionID != id {
		t.Errorf("Codex metadata = provider %q id %q", codexSpec.Provider, codexSpec.SessionID)
	}

	// If caretaker stopped before the provider supplied an ID, restoration is a
	// fresh launch rather than a malformed resume with an empty argument.
	emptyCodex, err := c.RestoreProviderAgentSpec(agent.Codex, "", "amber-fox", "continue")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"codex", "--codex-base", "continue"}; !equalStrings(emptyCodex.Argv, want) {
		t.Errorf("empty-ID Codex argv = %v, want %v", emptyCodex.Argv, want)
	}
	if emptyCodex.SessionID != "" {
		t.Errorf("fresh Codex ID = %q, want provider-assigned empty ID", emptyCodex.SessionID)
	}
	emptyClaude, err := c.RestoreProviderAgentSpec(agent.Claude, "", "amber-fox", "continue")
	if err != nil {
		t.Fatal(err)
	}
	if emptyClaude.SessionID == "" {
		t.Error("empty-ID Claude restore did not generate a fresh UUID")
	}
	wantEmptyClaude := []string{"claude", "--claude-base", "--session-id", emptyClaude.SessionID, "--teammate-mode", "in-process", "-n", "amber-fox", "continue"}
	if !equalStrings(emptyClaude.Argv, wantEmptyClaude) {
		t.Errorf("empty-ID Claude argv = %v, want %v", emptyClaude.Argv, wantEmptyClaude)
	}

	// Claude keeps the missing-transcript fresh fallback, including the initial
	// prompt.
	claudeFresh, err := c.RestoreProviderAgentSpec(agent.Claude, id, "amber-fox", "continue")
	if err != nil {
		t.Fatal(err)
	}
	wantFresh := []string{"claude", "--claude-base", "--session-id", id, "--teammate-mode", "in-process", "-n", "amber-fox", "continue"}
	if !equalStrings(claudeFresh.Argv, wantFresh) {
		t.Errorf("Claude fresh argv = %v, want %v", claudeFresh.Argv, wantFresh)
	}

	proj := filepath.Join(projects, "-some-project")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(proj, id+".jsonl"), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	claudeResume, err := c.RestoreProviderAgentSpec(agent.Claude, id, "amber-fox", "continue")
	if err != nil {
		t.Fatal(err)
	}
	wantResume := []string{"claude", "--claude-base", "--resume", id, "--teammate-mode", "in-process", "continue"}
	if !equalStrings(claudeResume.Argv, wantResume) {
		t.Errorf("Claude resume argv = %v, want %v", claudeResume.Argv, wantResume)
	}
}

func TestProviderAgentSpecErrors(t *testing.T) {
	c := NewController(config.Config{Agents: config.Agents{
		Default: agent.Claude,
		Enabled: []agent.Provider{agent.Claude},
		Claude:  config.AgentProvider{Command: "claude"},
	}})
	if _, err := c.NewProviderAgentSpec(agent.Provider("other"), "", ""); err == nil {
		t.Error("expected unknown-provider error")
	}
	if _, err := c.RestoreProviderAgentSpec("", "id", "", ""); err == nil {
		t.Error("expected empty-provider error")
	}
	if _, err := c.NewProviderAgentSpec(agent.Codex, "", ""); err == nil {
		t.Error("expected missing-command error")
	}
}

func TestResumeAgentSpec(t *testing.T) {
	c := &Controller{}
	c.cfg.Agent = "claude"

	id := "11111111-2222-4333-8444-555555555555"
	spec := c.ResumeAgentSpec(id, "refactor auth")
	want := []string{"claude", "--resume", id, "--teammate-mode", "in-process"}
	if got := spec.Argv; !equalStrings(got, want) {
		t.Errorf("resume argv = %v, want %v", got, want)
	}
	if spec.SessionID != id {
		t.Errorf("resume session id = %q, want %q", spec.SessionID, id)
	}
	if spec.Title != "refactor auth" {
		t.Errorf("resume title = %q", spec.Title)
	}
	// An empty label falls back to the default "claude" display title.
	if got := c.ResumeAgentSpec(id, "").Title; got != "claude" {
		t.Errorf("empty-label title = %q, want claude", got)
	}
}

func TestAgentSpecResumeOrFallback(t *testing.T) {
	projects := t.TempDir()
	c := &Controller{projectsDir: projects}
	c.cfg.Agent = "claude"

	id := "11111111-2222-4333-8444-555555555555"

	// No transcript on disk: AgentSpec falls back to a fresh session under the
	// same id, so the pane comes up working instead of erroring on --resume.
	fresh := c.AgentSpec(id, "refactor auth")
	wantFresh := []string{"claude", "--session-id", id, "--teammate-mode", "in-process", "-n", "refactor auth"}
	if got := fresh.Argv; !equalStrings(got, wantFresh) {
		t.Errorf("missing-transcript argv = %v, want %v", got, wantFresh)
	}
	if fresh.SessionID != id {
		t.Errorf("fallback kept session id = %q, want %q", fresh.SessionID, id)
	}

	// Transcript present (under any project dir, matched by unique id): resume.
	proj := filepath.Join(projects, "-some-project")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(proj, id+".jsonl"), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	resumed := c.AgentSpec(id, "refactor auth")
	wantResume := []string{"claude", "--resume", id, "--teammate-mode", "in-process"}
	if got := resumed.Argv; !equalStrings(got, wantResume) {
		t.Errorf("present-transcript argv = %v, want %v", got, wantResume)
	}
}

func TestEnsureProjectTrusted(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, ".claude.json")
	home := "/Users/test"
	other := "/Users/test/other-project"
	initial := `{"userID":"abc123","projects":{"` + other + `":{"hasTrustDialogAccepted":true,"lastCost":42}}}`
	if err := os.WriteFile(configPath, []byte(initial), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := ensureProjectTrusted(configPath, home); err != nil {
		t.Fatal(err)
	}

	var got map[string]any
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	projects := got["projects"].(map[string]any)
	homeProj := projects[home].(map[string]any)
	if homeProj["hasTrustDialogAccepted"] != true {
		t.Errorf("home project hasTrustDialogAccepted = %v, want true", homeProj["hasTrustDialogAccepted"])
	}
	if got["userID"] != "abc123" {
		t.Errorf("unrelated top-level field userID = %v, want abc123", got["userID"])
	}
	otherProj := projects[other].(map[string]any)
	if otherProj["lastCost"] != float64(42) {
		t.Errorf("unrelated project field lastCost = %v, want 42", otherProj["lastCost"])
	}

	// Calling again is a no-op: it should not error and the flag stays true.
	if err := ensureProjectTrusted(configPath, home); err != nil {
		t.Fatal(err)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func equalProviders(a, b []agent.Provider) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
