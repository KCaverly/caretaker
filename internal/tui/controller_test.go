package tui

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestParseAgentStatuses(t *testing.T) {
	data := []byte(`[
		{"pid":1480,"cwd":"/r/a","kind":"interactive","name":"refactor-auth","status":"busy","startedAt":1781790525707},
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
	if got[1480].Status != "busy" || got[1480].Cwd != "/r/a" || got[1480].Name != "refactor-auth" || got[1480].StartedAt != 1781790525707 {
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
	// The title stays empty so the UI can substitute claude's own session
	// name (from the status poll) once it appears.
	if unnamed.Title != "" {
		t.Errorf("unnamed title = %q, want empty", unnamed.Title)
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
	// An empty label stays empty: display code falls back to the live
	// session name, then "claude".
	if got := c.ResumeAgentSpec(id, "").Title; got != "" {
		t.Errorf("empty-label title = %q, want empty", got)
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

func TestAgentDisplayTitle(t *testing.T) {
	var m Model
	m.agentStatus = map[int]AgentStatus{7: {Name: "fix-login-flow"}}
	if got := m.agentDisplayTitle(7, "my label"); got != "my label" {
		t.Errorf("user label should win: got %q", got)
	}
	if got := m.agentDisplayTitle(7, ""); got != "fix-login-flow" {
		t.Errorf("live session name should fill an empty label: got %q", got)
	}
	if got := m.agentDisplayTitle(8, ""); got != "claude" {
		t.Errorf("placeholder before the first poll: got %q", got)
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
