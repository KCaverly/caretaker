// Package config loads ct's configuration.
package config

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/BurntSushi/toml"

	"github.com/KCaverly/caretaker/internal/agent"
	"github.com/KCaverly/caretaker/internal/plasma"
)

// Config holds ct's runtime configuration.
type Config struct {
	// Root is the parent directory that contains the user's repos. Required.
	Root string `toml:"root"`
	// Editor is the command launched for the nvim session.
	Editor string `toml:"editor"`
	// Agent is the legacy command launched for a Claude session. Agents.Claude
	// is the canonical provider configuration; this field remains supported so
	// existing config files continue to work.
	Agent string `toml:"agent"`
	// Agents configures the available agent providers and their commands.
	Agents Agents `toml:"agents"`
	// Shell is the command launched for a terminal session.
	Shell string `toml:"shell"`
	// WorktreePath is the path template for new worktrees, relative to the repo.
	// Supports {name}.
	WorktreePath string `toml:"worktree_path"`
	// BranchName is the branch-name template for new worktrees. Supports {name}.
	BranchName string `toml:"branch_name"`
	// Keys configures the reserved navigation keystrokes.
	Keys Keys `toml:"keys"`
	// Usage configures the plan usage-limit gauge.
	Usage Usage `toml:"usage"`
	// Plasma configures the deck's ambient plasma panel.
	Plasma Plasma `toml:"plasma"`
	// Stack configures stacked-PR workflow behavior.
	Stack Stack `toml:"stack"`
	// Display configures terminal rendering choices.
	Display Display `toml:"display"`
}

const (
	IconsNerd  = "nerd"
	IconsText  = "text"
	IconsASCII = "ascii"
)

// Display configures terminal rendering choices.
type Display struct {
	// Icons selects persistent navigation and pane symbols: nerd, text, or ascii.
	Icons string `toml:"icons"`
}

// Stack configures stacked-PR workflow behavior.
type Stack struct {
	// AutoMerge bypasses ct's merge confirmation panel. This is distinct from
	// GitHub auto-merge: an eligible merge runs immediately when requested.
	AutoMerge bool `toml:"auto_merge"`
}

// Agents configures which agent providers can be launched.
type Agents struct {
	// Default is selected when the new-agent form opens.
	Default agent.Provider `toml:"default"`
	// Enabled lists the providers offered for new agents.
	Enabled []agent.Provider `toml:"enabled"`
	Claude  AgentProvider    `toml:"claude"`
	Codex   AgentProvider    `toml:"codex"`
}

// AgentProvider configures one provider's executable and base arguments.
type AgentProvider struct {
	Command string   `toml:"command"`
	Args    []string `toml:"args"`
}

// Provider returns the configuration for p. The zero value is returned for an
// unknown provider.
func (a Agents) Provider(p agent.Provider) AgentProvider {
	switch p {
	case agent.Claude:
		return a.Claude
	case agent.Codex:
		return a.Codex
	default:
		return AgentProvider{}
	}
}

// Plasma configures the ambient animation panel on the right of the deck.
type Plasma struct {
	// Pattern picks the field shape: classic, waves, interference, lava,
	// or ripple.
	Pattern string `toml:"pattern"`
	// Palette picks the color ramp: aurora (blue/purple), ember
	// (yellow/red), or mono (grayscale).
	Palette string `toml:"palette"`
	// Charset picks the density ramp: dots (braille), shade, or blocks.
	Charset string `toml:"charset"`
	// Speed scales the animation rate (0 freezes the pattern; capped at 3).
	Speed float64 `toml:"speed"`
	// Width is the panel's share of the deck as a percent of the terminal
	// width (0 disables the panel; capped at 70).
	Width int `toml:"width"`
}

// Usage configures the plan usage-limit gauge.
type Usage struct {
	// Threshold is the utilization percent at/above which the status-bar
	// usage gauge appears (0 shows it always; values above 100 never show it).
	Threshold int `toml:"threshold"`
}

// Keys are the reserved keystrokes ct handles instead of forwarding to an
// embedded session.
type Keys struct {
	// Cycle moves one session view to the right (nvim → claude → term → nvim).
	Cycle string `toml:"cycle"`
	// CycleBack moves one session view to the left (the reverse of Cycle).
	CycleBack string `toml:"cycle_back"`
	// GotoEditor / GotoAgent / GotoTerm jump straight to that session view.
	GotoEditor string `toml:"goto_editor"`
	GotoAgent  string `toml:"goto_agent"`
	GotoTerm   string `toml:"goto_term"`
	// Picker returns to the CT picker.
	Picker string `toml:"picker"`
	// Palette opens the agent board: every agent across all open worktrees,
	// attention first, plus the new-agent launcher. Legacy-named: the
	// fuzzy-searchable command palette is CommandPalette (below), not this.
	Palette string `toml:"palette"`
	// NextAgent / PrevAgent cycle the focused agent within the worktree.
	NextAgent string `toml:"next_agent"`
	PrevAgent string `toml:"prev_agent"`
	// Help toggles the key/legend overlay (works in the deck and in sessions).
	Help string `toml:"help"`
	// GlobalConfig opens the home-directory workspace for editing global config.
	GlobalConfig string `toml:"global_config"`
	// Attention jumps straight into the session of the agent that most needs the
	// user — live-waiting agents first, then unread completions — and cycles to
	// the next such agent on each press, collapsing the open-board/scan/select
	// flow into one chord. An empty string disables it.
	Attention string `toml:"attention"`
	// Back returns to the work location captured before the last attention jump
	// or cross-worktree activation. Repeated presses toggle between the two.
	Back string `toml:"back"`
	// Terminal pane management (only intercepted on the terminal screen).
	TermSplitV string `toml:"term_split_v"` // new pane to the right
	TermSplitH string `toml:"term_split_h"` // new pane below
	TermZoom   string `toml:"term_zoom"`    // toggle full-size
	TermClose  string `toml:"term_close"`   // close active pane
	// Directional terminal-pane focus (only intercepted on the terminal screen).
	TermFocusLeft  string `toml:"term_focus_left"`
	TermFocusDown  string `toml:"term_focus_down"`
	TermFocusUp    string `toml:"term_focus_up"`
	TermFocusRight string `toml:"term_focus_right"`
	// Usage opens the usage overlay on the agent screen.
	Usage string `toml:"usage"`
	// CommandPalette opens the command palette: a fuzzy-searchable list of every
	// ct action, each row showing its live keybinding. It teaches the chords the
	// help overlay documents by letting the user run any action without them.
	// Distinct from Palette, which is the (legacy-named) agent board.
	CommandPalette string `toml:"command_palette"`
}

// Default returns a Config populated with defaults (Root left empty).
func Default() Config {
	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = "/bin/sh"
	}
	return Config{
		Editor: "nvim",
		Agent:  "claude",
		Agents: Agents{
			Default: agent.Claude,
			Enabled: []agent.Provider{agent.Claude, agent.Codex},
			Claude:  AgentProvider{Command: "claude"},
			Codex:   AgentProvider{Command: "codex"},
		},
		Shell:        shell,
		WorktreePath: ".worktrees/{name}",
		BranchName:   "{name}",
		Keys: Keys{
			Cycle: "alt+]", CycleBack: "alt+[",
			GotoEditor: "alt+1", GotoAgent: "alt+2", GotoTerm: "alt+3",
			Picker:  "ctrl+g",
			Palette: "alt+a", NextAgent: "f4", PrevAgent: "f3",
			Help: "f1", GlobalConfig: "alt+g", Attention: "alt+n", Back: "alt+b",
			TermSplitV: "alt+v", TermSplitH: "alt+s",
			TermZoom: "alt+z", TermClose: "alt+x",
			TermFocusLeft: "alt+h", TermFocusDown: "alt+j",
			TermFocusUp: "alt+k", TermFocusRight: "alt+l",
			Usage: "alt+u", CommandPalette: "alt+p",
		},
		Usage:   Usage{Threshold: 50},
		Display: Display{Icons: IconsNerd},
		Plasma: Plasma{
			Pattern: "classic", Palette: "aurora", Charset: "dots",
			Speed: 0.3, Width: 40,
		},
	}
}

// Path returns the config file path. Checks CT_CONFIG env var first, then
// defaults to ~/.caretaker/config.toml.
func Path() (string, error) {
	if p := os.Getenv("CT_CONFIG"); p != "" {
		return p, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".caretaker", "config.toml"), nil
}

// ErrNoConfig is returned by Load when the config file does not exist.
type ErrNoConfig struct {
	Path string
}

func (e *ErrNoConfig) Error() string {
	return fmt.Sprintf("no config file at %s", e.Path)
}

// Load reads the config file, applying defaults for any unset fields. Root must
// resolve to an existing directory.
func Load() (Config, error) {
	cfg := Default()

	path, err := Path()
	if err != nil {
		return cfg, err
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, &ErrNoConfig{Path: path}
		}
		return cfg, fmt.Errorf("reading %s: %w", path, err)
	}

	// Decode over the defaults so unset fields keep their default values.
	metadata, err := toml.Decode(string(data), &cfg)
	if err != nil {
		return cfg, fmt.Errorf("parsing %s: %w", path, err)
	}
	// The top-level agent key predates provider configuration. Keep it working
	// as the Claude command unless the new provider-specific key is explicit.
	if metadata.IsDefined("agents", "claude", "command") {
		cfg.Agent = cfg.Agents.Claude.Command
	} else {
		cfg.Agents.Claude.Command = cfg.Agent
	}

	if err := cfg.validate(); err != nil {
		return cfg, err
	}
	return cfg, nil
}

// Save writes a minimal config containing just the root path, creating the
// config directory if needed.
func Save(path, root string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("creating config dir: %w", err)
	}
	content := fmt.Sprintf("root = %q\n", root)
	return os.WriteFile(path, []byte(content), 0o644)
}

// ResolveRoot expands ~ and makes root absolute, returning an error if it
// doesn't point to an existing directory.
func ResolveRoot(root string) (string, error) {
	expanded, err := expandTilde(root)
	if err != nil {
		return "", err
	}
	abs, err := filepath.Abs(expanded)
	if err != nil {
		return "", fmt.Errorf("resolving %q: %w", root, err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return "", fmt.Errorf("%q: %w", abs, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("%q is not a directory", abs)
	}
	return abs, nil
}

func (c *Config) validate() error {
	if c.Root == "" {
		return fmt.Errorf("config `root` is required (the directory containing your repos)")
	}
	abs, err := ResolveRoot(c.Root)
	if err != nil {
		return err
	}
	c.Root = abs
	switch c.Display.Icons {
	case IconsNerd, IconsText, IconsASCII:
	default:
		return fmt.Errorf("config `display.icons` must be %q, %q, or %q", IconsNerd, IconsText, IconsASCII)
	}
	if !c.Agents.Default.Valid() {
		return fmt.Errorf("config `agents.default` must be %q or %q", agent.Claude, agent.Codex)
	}
	enabled := make(map[agent.Provider]bool, len(c.Agents.Enabled))
	for _, provider := range c.Agents.Enabled {
		if !provider.Valid() {
			return fmt.Errorf("unknown agent provider %q in `agents.enabled`", provider)
		}
		if enabled[provider] {
			return fmt.Errorf("duplicate agent provider %q in `agents.enabled`", provider)
		}
		enabled[provider] = true
		if c.Agents.Provider(provider).Command == "" {
			return fmt.Errorf("config `agents.%s.command` is required when %q is enabled", provider, provider)
		}
	}
	if !enabled[c.Agents.Default] {
		return fmt.Errorf("config `agents.default` %q must be present in `agents.enabled`", c.Agents.Default)
	}
	// A negative threshold is meaningless; treat it as "always show".
	if c.Usage.Threshold < 0 {
		c.Usage.Threshold = 0
	}
	// Numeric plasma fields clamp quietly; variant names must exist so a
	// typo fails at startup with the valid options instead of a silent
	// fallback. Width 0 disables the panel, in which case the names are
	// irrelevant and skipped.
	c.Plasma.Speed = min(max(c.Plasma.Speed, 0), 3)
	c.Plasma.Width = min(max(c.Plasma.Width, 0), 70)
	if c.Plasma.Width > 0 {
		if err := plasma.Validate(plasma.Options{
			Pattern: c.Plasma.Pattern,
			Palette: c.Plasma.Palette,
			Charset: c.Plasma.Charset,
		}); err != nil {
			return err
		}
	}
	return nil
}

func expandTilde(path string) (string, error) {
	if path == "~" || len(path) >= 2 && path[:2] == "~/" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		if path == "~" {
			return home, nil
		}
		return filepath.Join(home, path[2:]), nil
	}
	return path, nil
}
