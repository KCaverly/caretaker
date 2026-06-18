package tui

import (
	"strings"

	"github.com/KCaverly/caretaker/internal/config"
	"github.com/KCaverly/caretaker/internal/repo"
	"github.com/KCaverly/caretaker/internal/session"
)

// Controller exposes ct's worktree operations and session specs to the TUI.
type Controller struct {
	cfg config.Config
}

// NewController builds a Controller from config.
func NewController(cfg config.Config) *Controller {
	return &Controller{cfg: cfg}
}

// WorktreeView is a worktree plus its current status.
type WorktreeView struct {
	WT    repo.Worktree
	Live  bool // has running sessions
	Dirty bool // uncommitted changes
}

// Group is a repo and its worktrees.
type Group struct {
	Repo      repo.Repo
	Worktrees []WorktreeView
}

// Load discovers repos and their worktrees with git status. Live status is
// filled in by the model from the session manager.
func (c *Controller) Load() ([]Group, error) {
	repos, err := repo.DiscoverRepos(c.cfg.Root)
	if err != nil {
		return nil, err
	}

	groups := make([]Group, 0, len(repos))
	for _, r := range repos {
		wts, err := repo.ListWorktrees(r)
		if err != nil {
			continue
		}
		g := Group{Repo: r, Worktrees: make([]WorktreeView, 0, len(wts))}
		for _, wt := range wts {
			st, _ := repo.WorktreeStatus(wt)
			g.Worktrees = append(g.Worktrees, WorktreeView{WT: wt, Dirty: st.Dirty})
		}
		groups = append(groups, g)
	}
	return groups, nil
}

// Create adds a new worktree + branch named `name` in repo r, based on baseRef
// (empty = current HEAD), returning the created worktree.
func (c *Controller) Create(r repo.Repo, name, baseRef string) (repo.Worktree, error) {
	relPath := strings.ReplaceAll(c.cfg.WorktreePath, "{name}", name)
	branch := strings.ReplaceAll(c.cfg.BranchName, "{name}", name)
	return repo.CreateWorktree(r, relPath, branch, baseRef)
}

// Remove deletes a worktree and its branch.
func (c *Controller) Remove(r repo.Repo, wt repo.Worktree) error {
	return repo.RemoveWorktree(r, wt, true)
}

// Keys returns the reserved navigation keystrokes (cycle, return-to-picker).
func (c *Controller) Keys() (cycle, picker string) {
	return c.cfg.Keys.Cycle, c.cfg.Keys.Picker
}

// Specs returns the default session set for a workspace: nvim, claude, a shell.
func (c *Controller) Specs() []session.Spec {
	return []session.Spec{
		{Kind: session.Editor, Title: "nvim", Argv: []string{c.cfg.Editor}},
		{Kind: session.Agent, Title: "claude", Argv: []string{c.cfg.Agent}},
		{Kind: session.Terminal, Title: "term", Argv: []string{c.cfg.Shell}},
	}
}
