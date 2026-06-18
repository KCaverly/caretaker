package tui

import (
	"os/exec"
	"strings"

	"github.com/KCaverly/caretaker/internal/backend"
	"github.com/KCaverly/caretaker/internal/config"
	"github.com/KCaverly/caretaker/internal/repo"
	"github.com/KCaverly/caretaker/internal/workspace"
)

// Controller ties config, repo discovery, and the backend together and exposes
// ct's primary operations to the TUI.
type Controller struct {
	cfg  config.Config
	be   backend.Backend
	cmds workspace.Commands
}

// NewController builds a Controller from config and a backend.
func NewController(cfg config.Config, be backend.Backend) *Controller {
	return &Controller{
		cfg: cfg,
		be:  be,
		cmds: workspace.Commands{
			Editor: cfg.Editor,
			Agent:  cfg.Agent,
			Shell:  cfg.Shell,
		},
	}
}

// WorktreeView is a worktree plus its current status.
type WorktreeView struct {
	WT    repo.Worktree
	Live  bool
	Dirty bool
}

// Group is a repo and its worktrees.
type Group struct {
	Repo      repo.Repo
	Worktrees []WorktreeView
}

// Load discovers repos and their worktrees with status. Per-item status errors
// are tolerated (reported as false) so one bad repo doesn't break the deck.
func (c *Controller) Load() ([]Group, error) {
	repos, err := repo.DiscoverRepos(c.cfg.Root)
	if err != nil {
		return nil, err
	}

	groups := make([]Group, 0, len(repos))
	for _, r := range repos {
		wts, err := repo.ListWorktrees(r)
		if err != nil {
			// Skip repos we can't read worktrees for, but keep going.
			continue
		}
		g := Group{Repo: r, Worktrees: make([]WorktreeView, 0, len(wts))}
		for _, wt := range wts {
			live, _ := c.be.Exists(c.workspaceFor(wt))
			st, _ := repo.WorktreeStatus(wt)
			g.Worktrees = append(g.Worktrees, WorktreeView{WT: wt, Live: live, Dirty: st.Dirty})
		}
		groups = append(groups, g)
	}
	return groups, nil
}

func (c *Controller) workspaceFor(wt repo.Worktree) workspace.Workspace {
	return workspace.Default(wt.Repo, wt.Name, wt.Path, c.cmds)
}

// Create adds a new worktree + branch named `name` in repo r, based on baseRef
// (empty = current HEAD), returning the created worktree.
func (c *Controller) Create(r repo.Repo, name, baseRef string) (repo.Worktree, error) {
	relPath := strings.ReplaceAll(c.cfg.WorktreePath, "{name}", name)
	branch := strings.ReplaceAll(c.cfg.BranchName, "{name}", name)
	return repo.CreateWorktree(r, relPath, branch, baseRef)
}

// Ensure makes sure the backend session for wt exists.
func (c *Controller) Ensure(wt repo.Worktree) error {
	return c.be.Ensure(c.workspaceFor(wt))
}

// AttachCmd returns the command that attaches to wt's workspace full-screen.
func (c *Controller) AttachCmd(wt repo.Worktree) (*exec.Cmd, error) {
	return c.be.AttachCmd(c.workspaceFor(wt))
}

// Archive tears down wt's running session, leaving the worktree on disk.
func (c *Controller) Archive(wt repo.Worktree) error {
	return c.be.Archive(c.workspaceFor(wt))
}

// Remove archives then deletes wt's worktree (and its branch).
func (c *Controller) Remove(r repo.Repo, wt repo.Worktree) error {
	if err := c.be.Archive(c.workspaceFor(wt)); err != nil {
		return err
	}
	return repo.RemoveWorktree(r, wt, true)
}

// AddAgent adds a claude session to wt's running workspace.
func (c *Controller) AddAgent(wt repo.Worktree) error {
	return c.be.AddSession(c.workspaceFor(wt), workspace.AgentSession(c.cmds))
}

// AddTerminal adds a terminal session to wt's running workspace.
func (c *Controller) AddTerminal(wt repo.Worktree) error {
	return c.be.AddSession(c.workspaceFor(wt), workspace.TerminalSession())
}
