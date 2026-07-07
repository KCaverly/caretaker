package main

import (
	"errors"
	"fmt"
	"os"

	tea "charm.land/bubbletea/v2"

	"github.com/KCaverly/caretaker/internal/config"
	"github.com/KCaverly/caretaker/internal/session"
	"github.com/KCaverly/caretaker/internal/tui"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "ct:", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	configPath := ""
	needsSetup := false
	if err != nil {
		var noConfig *config.ErrNoConfig
		if errors.As(err, &noConfig) {
			cfg = config.Default()
			configPath = noConfig.Path
			needsSetup = true
		} else {
			return err
		}
	}

	mgr := session.NewManager()
	defer mgr.CloseAll() // reap all ptys on exit

	ctrl := tui.NewController(cfg)
	m := tui.New(ctrl, mgr)
	if needsSetup {
		m = m.EnterSetup(configPath)
	}
	p := tea.NewProgram(m)
	final, err := p.Run()
	// State writes run in the background during the session; flush once
	// synchronously so an in-flight or just-scheduled write can't be lost.
	if fm, ok := final.(tui.Model); ok {
		fm.FlushState()
	}
	return err
}
