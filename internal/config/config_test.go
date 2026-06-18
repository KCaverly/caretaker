package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDefault(t *testing.T) {
	t.Setenv("SHELL", "/bin/zsh")
	d := Default()
	if d.Editor != "nvim" || d.Agent != "claude" || d.Backend != "zellij" {
		t.Fatalf("unexpected defaults: %+v", d)
	}
	if d.Shell != "/bin/zsh" {
		t.Fatalf("shell = %q, want /bin/zsh", d.Shell)
	}
	if d.WorktreePath != ".worktrees/{name}" || d.BranchName != "{name}" {
		t.Fatalf("unexpected templates: %+v", d)
	}
}

func TestExpandTilde(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home dir")
	}
	got, err := expandTilde("~/repos")
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(home, "repos"); got != want {
		t.Fatalf("expandTilde = %q, want %q", got, want)
	}
	if got, _ := expandTilde("/abs/path"); got != "/abs/path" {
		t.Fatalf("expandTilde left absolute path alone: %q", got)
	}
}

func TestValidateRequiresRoot(t *testing.T) {
	c := Default()
	if err := c.validate(); err == nil {
		t.Fatal("expected error when root is empty")
	}

	dir := t.TempDir()
	c.Root = dir
	if err := c.validate(); err != nil {
		t.Fatalf("unexpected error for valid root: %v", err)
	}
	if !filepath.IsAbs(c.Root) {
		t.Fatalf("root not made absolute: %q", c.Root)
	}
}
