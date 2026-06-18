package main

import (
	"fmt"
	"os"

	tea "charm.land/bubbletea/v2"

	"github.com/KCaverly/caretaker/internal/backend/zellij"
	"github.com/KCaverly/caretaker/internal/config"
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

	be, err := zellij.New()
	if err != nil {
		return err
	}

	ctrl := tui.NewController(cfg, be)
	p := tea.NewProgram(tui.New(ctrl))
	_, err = p.Run()
	return err
}
