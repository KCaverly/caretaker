package tui

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/KCaverly/caretaker/internal/config"
	"github.com/KCaverly/caretaker/internal/repo"
	"github.com/KCaverly/caretaker/internal/session"
)

// Controller exposes ct's worktree operations and session specs to the TUI.
type Controller struct {
	cfg config.Config
	// projectsDir overrides where ct looks for claude's stored transcripts when
	// deciding whether a persisted agent can be resumed. Empty means the default
	// ~/.claude/projects; tests point it at a temp dir.
	projectsDir string
}

// NewController builds a Controller from config.
func NewController(cfg config.Config) *Controller {
	return &Controller{cfg: cfg}
}

// SetRoot updates the repos root after first-run setup.
func (c *Controller) SetRoot(root string) {
	c.cfg.Root = root
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

// GlobalConfigKey returns the key that opens the home-directory workspace.
func (c *Controller) GlobalConfigKey() string { return c.cfg.Keys.GlobalConfig }

// NotifKey returns the key that opens the notification overlay.
func (c *Controller) NotifKey() string { return c.cfg.Keys.Notif }

// TermPaneKeys returns the reserved keys for terminal pane management. These
// are only intercepted when the terminal screen is active.
func (c *Controller) TermPaneKeys() (splitV, splitH, cycle, zoom, close string) {
	k := c.cfg.Keys
	return k.TermSplitV, k.TermSplitH, k.TermCycle, k.TermZoom, k.TermClose
}

// GlobalConfigDir returns the home directory path for the global config workspace.
func (c *Controller) GlobalConfigDir() (string, error) { return os.UserHomeDir() }

// EnsureHomeDirTrusted writes hasTrustDialogAccepted to Claude's internal
// project settings for the home directory so that interactive sessions started
// there (e.g. background agents) don't pause to show the workspace trust dialog.
// It is idempotent and safe to call before every spawn.
func (c *Controller) EnsureHomeDirTrusted() error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	// Claude encodes the project path by replacing every path separator with '-'.
	encoded := strings.ReplaceAll(home, string(filepath.Separator), "-")
	dir := filepath.Join(home, ".claude", "projects", encoded)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	settingsPath := filepath.Join(dir, "settings.json")
	if _, err := os.Stat(settingsPath); err == nil {
		return nil // already exists
	}
	data := []byte(`{"hasTrustDialogAccepted":true}` + "\n")
	return os.WriteFile(settingsPath, data, 0o644)
}

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
	return c.freshAgentSpec(newSessionID(), label)
}

// freshAgentSpec builds the spec for a brand-new claude session running under
// the given id (shared by NewAgentSpec and the resume-fallback path).
func (c *Controller) freshAgentSpec(id, label string) session.Spec {
	argv := []string{c.cfg.Agent, "--session-id", id, "--teammate-mode", "in-process"}
	if label != "" {
		argv = append(argv, "-n", label)
	}
	return session.Spec{Kind: session.Agent, Title: agentTitle(label), Argv: argv, SessionID: id}
}

// AgentSpec returns the spec to (re)launch a persisted agent. It resumes the
// saved conversation when claude still has a transcript for id, and otherwise
// starts a fresh session under the same id. That way a transcript claude has
// since dropped — the 30-day retention sweep, a session that never persisted
// (spawned but never used), or a moved/recreated worktree — comes back as a
// working empty agent instead of a "no conversation found" error in the pane.
// Reusing the id keeps ct's persisted pool stable and lets the revived session
// resume normally on the next open.
func (c *Controller) AgentSpec(id, label string) session.Spec {
	if c.transcriptExists(id) {
		return c.ResumeAgentSpec(id, label)
	}
	return c.freshAgentSpec(id, label)
}

// transcriptExists reports whether claude still has a stored conversation for
// the session id. Session UUIDs are globally unique, so it globs every project
// dir rather than reconstructing claude's cwd-derived path key (which is
// internal and version-dependent).
func (c *Controller) transcriptExists(id string) bool {
	if id == "" {
		return false
	}
	dir := c.projectsDir
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return false
		}
		dir = filepath.Join(home, ".claude", "projects")
	}
	matches, _ := filepath.Glob(filepath.Join(dir, "*", id+".jsonl"))
	return len(matches) > 0
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
