package tui

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"os/exec"
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
	WT         repo.Worktree
	Live       bool  // has running sessions
	Dirty      bool  // uncommitted changes
	CommitTime int64 // HEAD commit time (unix seconds), fallback sort key
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
			g.Worktrees = append(g.Worktrees, WorktreeView{WT: wt, Dirty: st.Dirty, CommitTime: st.CommitTime})
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

// AgentKeys returns the reserved agent-pool keystrokes (open palette, next
// agent, previous agent).
func (c *Controller) AgentKeys() (palette, next, prev string) {
	return c.cfg.Keys.Palette, c.cfg.Keys.NextAgent, c.cfg.Keys.PrevAgent
}

// HelpKey returns the reserved key that toggles the help overlay.
func (c *Controller) HelpKey() string { return c.cfg.Keys.Help }

// EditorSpec returns the spec for a workspace's nvim session.
func (c *Controller) EditorSpec() session.Spec {
	return session.Spec{Kind: session.Editor, Title: "nvim", Argv: []string{c.cfg.Editor}}
}

// TermSpec returns the spec for a workspace's shell session.
func (c *Controller) TermSpec() session.Spec {
	return session.Spec{Kind: session.Terminal, Title: "term", Argv: []string{c.cfg.Shell}}
}

// agentTitle is the palette/display title for an agent with the given label.
func agentTitle(label string) string {
	if label == "" {
		return "claude"
	}
	return label
}

// NewAgentSpec returns the spec for a brand-new Claude agent session, optionally
// named. ct generates the session UUID and passes it as --session-id so it can
// persist the id and resume the same conversation in a later run. The label is
// passed to claude as -n (so it appears in `claude agents --json` and the agent
// picker), and teammate split-pane mode is pinned off so any agent team renders
// in-process inside the pane ct controls.
func (c *Controller) NewAgentSpec(label string) session.Spec {
	id := newSessionID()
	argv := []string{c.cfg.Agent, "--session-id", id, "--teammate-mode", "in-process"}
	if label != "" {
		argv = append(argv, "-n", label)
	}
	return session.Spec{Kind: session.Agent, Title: agentTitle(label), Argv: argv, SessionID: id}
}

// ResumeAgentSpec returns the spec for a Claude agent session that resumes the
// conversation with the given session id (recorded by a previous run). The label
// drives only ct's palette title; the conversation's own name is restored by
// claude on resume.
func (c *Controller) ResumeAgentSpec(id, label string) session.Spec {
	argv := []string{c.cfg.Agent, "--resume", id, "--teammate-mode", "in-process"}
	return session.Spec{Kind: session.Agent, Title: agentTitle(label), Argv: argv, SessionID: id}
}

// newSessionID returns a random RFC-4122 v4 UUID for a fresh claude session.
func newSessionID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// AgentStatus is the live state of a Claude session as reported by
// `claude agents --json`, keyed elsewhere by pid.
type AgentStatus struct {
	Status     string // "busy", "idle", or "waiting"
	WaitingFor string // when waiting: "permission prompt" / "input needed"
	Cwd        string
	StartedAt  int64 // unix milliseconds
}

// rawAgent mirrors one entry of `claude agents --json`.
type rawAgent struct {
	Pid        int    `json:"pid"`
	Cwd        string `json:"cwd"`
	Status     string `json:"status"`
	WaitingFor string `json:"waitingFor"`
	StartedAt  int64  `json:"startedAt"`
}

// parseAgentStatuses decodes `claude agents --json` output into a map keyed by
// pid. Entries without a live pid (background-only sessions) are skipped.
func parseAgentStatuses(data []byte) (map[int]AgentStatus, error) {
	var raw []rawAgent
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, err
	}
	out := make(map[int]AgentStatus, len(raw))
	for _, r := range raw {
		if r.Pid == 0 {
			continue
		}
		out[r.Pid] = AgentStatus{Status: r.Status, WaitingFor: r.WaitingFor, Cwd: r.Cwd, StartedAt: r.StartedAt}
	}
	return out, nil
}

// AgentStatuses runs `claude agents --json` and returns live session statuses
// keyed by pid. It requires no TTY and exits immediately.
func (c *Controller) AgentStatuses(ctx context.Context) (map[int]AgentStatus, error) {
	out, err := exec.CommandContext(ctx, c.cfg.Agent, "agents", "--json").Output()
	if err != nil {
		return nil, err
	}
	return parseAgentStatuses(out)
}
