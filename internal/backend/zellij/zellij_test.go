package zellij

import (
	"strings"
	"testing"

	"github.com/KCaverly/caretaker/internal/workspace"
)

func TestSanitize(t *testing.T) {
	cases := map[string]string{
		"caretaker-feat-login": "caretaker-feat-login",
		"my repo/feat":         "my-repo-feat",
		"--weird--":            "weird",
		"":                     "ct",
	}
	for in, want := range cases {
		if got := sanitize(in); got != want {
			t.Errorf("sanitize(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSessionName(t *testing.T) {
	ws := workspace.Workspace{Repo: "caretaker", Worktree: "feat/login"}
	if got := SessionName(ws); got != "caretaker-feat-login" {
		t.Fatalf("SessionName = %q", got)
	}
}

func TestRenderLayout(t *testing.T) {
	ws := workspace.Default("caretaker", "feat-login", "/home/u/repos/caretaker/.worktrees/feat-login",
		workspace.Commands{Editor: "nvim", Agent: "claude", Shell: "/bin/zsh"})
	out := renderLayout(ws)

	for _, want := range []string{
		`cwd "/home/u/repos/caretaker/.worktrees/feat-login"`,
		`tab name="nvim" focus=true {`,
		`pane command="nvim"`,
		`tab name="claude" {`,
		`pane command="claude"`,
		`tab name="term"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("layout missing %q\n---\n%s", want, out)
		}
	}

	// Only the first tab is focused.
	if strings.Count(out, "focus=true") != 1 {
		t.Errorf("expected exactly one focused tab:\n%s", out)
	}
	// The terminal tab is a default pane (no command).
	if strings.Contains(out, `tab name="term" {`) {
		t.Errorf("term tab should be a bare default pane:\n%s", out)
	}
}
