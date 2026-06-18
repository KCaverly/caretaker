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
		Keys:         Keys{Cycle: "ctrl+o", Picker: "ctrl+g"},
	}
}

// Path returns the config file path (honoring XDG_CONFIG_HOME).
func Path() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "ct", "config.toml"), nil
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
			return cfg, fmt.Errorf("no config file at %s: set `root` there to your repos directory", path)
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

func (c *Config) validate() error {
	if c.Root == "" {
		return fmt.Errorf("config `root` is required (the directory containing your repos)")
	}
	expanded, err := expandTilde(c.Root)
	if err != nil {
		return err
	}
	abs, err := filepath.Abs(expanded)
	if err != nil {
		return fmt.Errorf("resolving root %q: %w", c.Root, err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return fmt.Errorf("root %q: %w", abs, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("root %q is not a directory", abs)
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
