// Package config loads ct's configuration.
package config

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/BurntSushi/toml"
)

// Config holds ct's runtime configuration.
type Config struct {
	// Root is the parent directory that contains the user's repos. Required.
	Root string `toml:"root"`
	// Editor is the command launched for the nvim session.
	Editor string `toml:"editor"`
	// Agent is the command launched for a claude session.
	Agent string `toml:"agent"`
	// Shell is the command launched for a terminal session.
	Shell string `toml:"shell"`
	// Backend selects the workspace backend ("zellij").
	Backend string `toml:"backend"`
	// WorktreePath is the path template for new worktrees, relative to the repo.
	// Supports {name}.
	WorktreePath string `toml:"worktree_path"`
	// BranchName is the branch-name template for new worktrees. Supports {name}.
	BranchName string `toml:"branch_name"`
	// Keys configures the reserved navigation keystrokes.
	Keys Keys `toml:"keys"`
}

// Keys are the reserved keystrokes ct handles instead of forwarding to an
// embedded session.
type Keys struct {
	// Cycle moves one session view to the right (nvim → claude → term → nvim).
	Cycle string `toml:"cycle"`
	// Picker returns to the CT picker.
	Picker string `toml:"picker"`
	// Palette opens the agent switcher for the current worktree.
	Palette string `toml:"palette"`
	// NextAgent / PrevAgent cycle the focused agent within the worktree.
	NextAgent string `toml:"next_agent"`
	PrevAgent string `toml:"prev_agent"`
	// Help toggles the key/legend overlay (works in the deck and in sessions).
	Help string `toml:"help"`
}

// Default returns a Config populated with defaults (Root left empty).
func Default() Config {
	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = "/bin/sh"
	}
	return Config{
		Editor:       "nvim",
		Agent:        "claude",
		Shell:        shell,
		Backend:      "zellij",
		WorktreePath: ".worktrees/{name}",
		BranchName:   "{name}",
		Keys: Keys{
			Cycle: "ctrl+o", Picker: "ctrl+g",
			Palette: "ctrl+a", NextAgent: "f4", PrevAgent: "f3",
			Help: "f1",
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
	if err := toml.Unmarshal(data, &cfg); err != nil {
		return cfg, fmt.Errorf("parsing %s: %w", path, err)
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
