package config

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

func TestPath(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home dir")
	}

	t.Run("default", func(t *testing.T) {
		t.Setenv("CT_CONFIG", "")
		got, err := Path()
		if err != nil {
			t.Fatal(err)
		}
		want := filepath.Join(home, ".caretaker", "config.toml")
		if got != want {
			t.Fatalf("Path() = %q, want %q", got, want)
		}
	})

	t.Run("env override", func(t *testing.T) {
		t.Setenv("CT_CONFIG", "/custom/path/config.toml")
		got, err := Path()
		if err != nil {
			t.Fatal(err)
		}
		if got != "/custom/path/config.toml" {
			t.Fatalf("Path() = %q, want /custom/path/config.toml", got)
		}
	})
}

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

func TestDefaultUsage(t *testing.T) {
	d := Default()
	if d.Usage.Threshold != 50 {
		t.Fatalf("usage threshold = %d, want 50", d.Usage.Threshold)
	}
	if d.Keys.Usage != "alt+u" {
		t.Fatalf("usage key = %q, want alt+u", d.Keys.Usage)
	}
}

func TestDefaultKeymap(t *testing.T) {
	d := Default()
	cases := map[string]string{
		"cycle":       d.Keys.Cycle,
		"cycle_back":  d.Keys.CycleBack,
		"goto_editor": d.Keys.GotoEditor,
		"goto_agent":  d.Keys.GotoAgent,
		"goto_term":   d.Keys.GotoTerm,
		"palette":     d.Keys.Palette,
		"global":      d.Keys.GlobalConfig,
		"prompt":      d.Keys.Prompt,
		"split_v":     d.Keys.TermSplitV,
		"split_h":     d.Keys.TermSplitH,
		"zoom":        d.Keys.TermZoom,
		"close":       d.Keys.TermClose,
		"focus_left":  d.Keys.TermFocusLeft,
		"focus_down":  d.Keys.TermFocusDown,
		"focus_up":    d.Keys.TermFocusUp,
		"focus_right": d.Keys.TermFocusRight,
	}
	want := map[string]string{
		"cycle": "alt+]", "cycle_back": "alt+[",
		"goto_editor": "alt+1", "goto_agent": "alt+2", "goto_term": "alt+3",
		"palette": "alt+a", "global": "alt+g", "prompt": "alt+y",
		"split_v": "alt+v", "split_h": "alt+s", "zoom": "alt+z", "close": "alt+x",
		"focus_left": "alt+h", "focus_down": "alt+j", "focus_up": "alt+k", "focus_right": "alt+l",
	}
	for k, got := range cases {
		if got != want[k] {
			t.Errorf("default %s = %q, want %q", k, got, want[k])
		}
	}
	// The legacy notif alias and the pane-cycle key are retired by default.
	if d.Keys.Notif != "" {
		t.Errorf("default notif = %q, want empty (retired)", d.Keys.Notif)
	}
	if d.Keys.TermCycle != "" {
		t.Errorf("default term_cycle = %q, want empty (retired)", d.Keys.TermCycle)
	}
}

func TestDefaultCommandPalette(t *testing.T) {
	if d := Default(); d.Keys.CommandPalette != "alt+p" {
		t.Fatalf("command_palette default = %q, want alt+p", d.Keys.CommandPalette)
	}
}

func TestLoadCommandPaletteOverride(t *testing.T) {
	cfg := loadTOML(t, "[keys]\ncommand_palette = \"ctrl+k\"\n")
	if cfg.Keys.CommandPalette != "ctrl+k" {
		t.Fatalf("command_palette = %q, want ctrl+k", cfg.Keys.CommandPalette)
	}
	// A field left unset keeps its default.
	if cfg.Keys.Cycle != "alt+]" {
		t.Fatalf("unset default clobbered: cycle = %q", cfg.Keys.Cycle)
	}
}

func TestLoadDecodesNewKeys(t *testing.T) {
	cfg := loadTOML(t, `[keys]
cycle_back = "alt+p"
goto_editor = "alt+e"
goto_agent = "alt+c"
goto_term = "alt+t"
term_focus_left = "ctrl+h"
term_focus_right = "ctrl+l"
notif = "ctrl+n"
term_cycle = "ctrl+w"
`)
	if cfg.Keys.CycleBack != "alt+p" {
		t.Errorf("cycle_back = %q", cfg.Keys.CycleBack)
	}
	if cfg.Keys.GotoEditor != "alt+e" || cfg.Keys.GotoAgent != "alt+c" || cfg.Keys.GotoTerm != "alt+t" {
		t.Errorf("goto keys = %q/%q/%q", cfg.Keys.GotoEditor, cfg.Keys.GotoAgent, cfg.Keys.GotoTerm)
	}
	if cfg.Keys.TermFocusLeft != "ctrl+h" || cfg.Keys.TermFocusRight != "ctrl+l" {
		t.Errorf("focus keys = %q/%q", cfg.Keys.TermFocusLeft, cfg.Keys.TermFocusRight)
	}
	// A user can still re-enable the retired aliases.
	if cfg.Keys.Notif != "ctrl+n" || cfg.Keys.TermCycle != "ctrl+w" {
		t.Errorf("retired keys not re-enabled: notif=%q term_cycle=%q", cfg.Keys.Notif, cfg.Keys.TermCycle)
	}
	// Fields left unset keep their defaults.
	if cfg.Keys.Cycle != "alt+]" || cfg.Keys.TermFocusDown != "alt+j" {
		t.Errorf("unset defaults not preserved: cycle=%q focus_down=%q", cfg.Keys.Cycle, cfg.Keys.TermFocusDown)
	}
}

// loadTOML writes body to a temp config file (with root pointed at a real dir)
// and loads it, so tests can assert how TOML decodes over the defaults.
func loadTOML(t *testing.T, body string) Config {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	content := "root = " + strconv.Quote(dir) + "\n" + body
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CT_CONFIG", path)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return cfg
}

func TestLoadUsageDefaultsWhenAbsent(t *testing.T) {
	cfg := loadTOML(t, "")
	if cfg.Usage.Threshold != 50 {
		t.Fatalf("threshold = %d, want default 50", cfg.Usage.Threshold)
	}
	if cfg.Keys.Usage != "alt+u" {
		t.Fatalf("usage key = %q, want default alt+u", cfg.Keys.Usage)
	}
}

func TestLoadUsageOverride(t *testing.T) {
	cfg := loadTOML(t, "[usage]\nthreshold = 80\n[keys]\nusage = \"ctrl+p\"\n")
	if cfg.Usage.Threshold != 80 {
		t.Fatalf("threshold = %d, want 80", cfg.Usage.Threshold)
	}
	if cfg.Keys.Usage != "ctrl+p" {
		t.Fatalf("usage key = %q, want ctrl+p", cfg.Keys.Usage)
	}
}

func TestLoadUsageNegativeClamped(t *testing.T) {
	cfg := loadTOML(t, "[usage]\nthreshold = -10\n")
	if cfg.Usage.Threshold != 0 {
		t.Fatalf("threshold = %d, want clamped to 0", cfg.Usage.Threshold)
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
