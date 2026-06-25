package tui

import "testing"

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
	// An empty label falls back to the default "claude" display title.
	if got := c.ResumeAgentSpec(id, "").Title; got != "claude" {
		t.Errorf("empty-label title = %q, want claude", got)
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
