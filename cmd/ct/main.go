package main

import (
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
	if err != nil {
		return err
	}

	mgr := session.NewManager()
	defer mgr.CloseAll() // reap all ptys on exit

	ctrl := tui.NewController(cfg)
	p := tea.NewProgram(tui.New(ctrl, mgr))
	_, err = p.Run()
	return err
}
