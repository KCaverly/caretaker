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
	"sync"
	"sync/atomic"

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
	// homeTrusted records that EnsureHomeDirTrusted has already verified (or
	// set) the trust flag, so subsequent agent launches skip re-reading
	// ~/.claude.json. Only touched from the UI goroutine.
	homeTrusted bool
	// loadSeq stamps each issued Load so the loadedMsg handler can drop
	// results superseded by a newer in-flight load.
	loadSeq atomic.Uint64
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

	// Git work-state, filled in Load. Ahead/Behind count how far the branch has
	// diverged from the repo's main branch; HasBase gates whether that
	// comparison was available at all (false for the main worktree, a detached
	// primary tree, or on error). Add/Del are the uncommitted diffstat vs HEAD
	// (only computed when Dirty). Subject is the branch tip's commit subject.
	Ahead, Behind int
	HasBase       bool
	Add, Del      int
	Subject       string

	// BaseBranch is the repo's primary-worktree branch that Ahead/Behind was
	// measured against — the base the diff viewer diffs this branch against.
	// Empty when unavailable (the main worktree, a detached primary tree).
	BaseBranch string
}

// Group is a repo and its worktrees.
type Group struct {
	Repo      repo.Repo
	Worktrees []WorktreeView
}

// loadConcurrency bounds how many git subprocesses one Load runs at a time.
const loadConcurrency = 8

// Load discovers repos and their worktrees with git status. Live status is
// filled in by the model from the session manager. Repos and their worktree
// status checks run concurrently (bounded by loadConcurrency); result order
// stays deterministic because each repo writes into its own slot.
func (c *Controller) Load() ([]Group, error) {
	repos, err := repo.DiscoverRepos(c.cfg.Root)
	if err != nil {
		return nil, err
	}

	sem := make(chan struct{}, loadConcurrency)
	results := make([]*Group, len(repos))
	var wg sync.WaitGroup
	for i, r := range repos {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			wts, err := repo.ListWorktrees(r)
			if err != nil {
				<-sem
				return // skip repos that fail to list, as before
			}
			tips, _ := repo.BranchTips(r)
			<-sem

			// The primary worktree's branch is the base every other worktree's
			// ahead/behind is measured against; empty (a detached main) leaves
			// ahead/behind unavailable everywhere.
			mainBranch := ""
			if len(wts) > 0 {
				mainBranch = wts[0].Branch
			}

			views := make([]WorktreeView, len(wts))
			var wtWG sync.WaitGroup
			for j, wt := range wts {
				wtWG.Add(1)
				go func() {
					defer wtWG.Done()
					sem <- struct{}{}
					defer func() { <-sem }()
					st, _ := repo.WorktreeStatus(wt)
					tip := tips[wt.Branch]
					v := WorktreeView{WT: wt, Dirty: st.Dirty, CommitTime: tip.Time, Subject: tip.Subject}
					// Record the base every non-main worktree's ahead/behind was
					// measured against, so the diff viewer can diff against it.
					if !wt.IsMain {
						v.BaseBranch = mainBranch
					}
					if ahead, behind, ok := repo.AheadBehind(wt, mainBranch); ok {
						v.Ahead, v.Behind, v.HasBase = ahead, behind, true
					}
					// The dirty flag already covers untracked-only trees; the
					// diffstat (staged+unstaged vs HEAD) only adds value when there
					// are tracked changes, so gate the extra subprocess on Dirty.
					if st.Dirty {
						if add, del, err := repo.UncommittedDiffstat(wt); err == nil {
							v.Add, v.Del = add, del
						}
					}
					views[j] = v
				}()
			}
			wtWG.Wait()
			results[i] = &Group{Repo: r, Worktrees: views}
		}()
	}
	wg.Wait()

	groups := make([]Group, 0, len(repos))
	for _, g := range results {
		if g != nil {
			groups = append(groups, *g)
		}
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

// Remove deletes a worktree, and its branch when deleteBranch is set.
func (c *Controller) Remove(r repo.Repo, wt repo.Worktree, deleteBranch bool) error {
	return repo.RemoveWorktree(r, wt, deleteBranch)
}

// Keys returns the reserved navigation keystrokes (cycle, return-to-picker).
func (c *Controller) Keys() (cycle, picker string) {
	return c.cfg.Keys.Cycle, c.cfg.Keys.Picker
}

// CycleBackKey returns the key that cycles to the previous session view.
func (c *Controller) CycleBackKey() string { return c.cfg.Keys.CycleBack }

// GotoKeys returns the keys that jump straight to the editor, agent, and
// terminal views.
func (c *Controller) GotoKeys() (editor, agent, term string) {
	k := c.cfg.Keys
	return k.GotoEditor, k.GotoAgent, k.GotoTerm
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

// PromptKey returns the key that opens the quick-prompt overlay.
func (c *Controller) PromptKey() string { return c.cfg.Keys.Prompt }

// UsageKey returns the key that opens the usage overlay on the agent screen.
func (c *Controller) UsageKey() string { return c.cfg.Keys.Usage }

// CommandPaletteKey returns the key that opens the command palette.
func (c *Controller) CommandPaletteKey() string { return c.cfg.Keys.CommandPalette }

// UsageThreshold returns the utilization percent at/above which the bar's
// usage gauge appears (0 = always, >100 = never).
func (c *Controller) UsageThreshold() int { return c.cfg.Usage.Threshold }

// PlasmaConfig returns the deck plasma-panel settings.
func (c *Controller) PlasmaConfig() config.Plasma { return c.cfg.Plasma }

// TermPaneKeys returns the reserved keys for terminal pane management. These
// are only intercepted when the terminal screen is active.
func (c *Controller) TermPaneKeys() (splitV, splitH, cycle, zoom, close string) {
	k := c.cfg.Keys
	return k.TermSplitV, k.TermSplitH, k.TermCycle, k.TermZoom, k.TermClose
}

// TermFocusKeys returns the directional terminal-pane focus keys (left, down,
// up, right). These are only intercepted when the terminal screen is active.
func (c *Controller) TermFocusKeys() (left, down, up, right string) {
	k := c.cfg.Keys
	return k.TermFocusLeft, k.TermFocusDown, k.TermFocusUp, k.TermFocusRight
}

// GlobalConfigDir returns the home directory path for the global config workspace.
func (c *Controller) GlobalConfigDir() (string, error) { return os.UserHomeDir() }

// EnsureHomeDirTrusted marks the home directory as trusted in Claude's global
// config (~/.claude.json, under projects[home].hasTrustDialogAccepted) so that
// interactive sessions started there (e.g. background agents) don't pause to
// show the workspace trust dialog. It is idempotent and safe to call before
// every spawn: it only rewrites the file when the flag isn't already true, and
// preserves every other field byte-for-byte.
func (c *Controller) EnsureHomeDirTrusted() error {
	if c.homeTrusted {
		return nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	if err := ensureProjectTrusted(filepath.Join(home, ".claude.json"), home); err != nil {
		return err
	}
	c.homeTrusted = true
	return nil
}

// ensureProjectTrusted sets projects[projectPath].hasTrustDialogAccepted to
// true inside the Claude config at configPath. It round-trips unrelated
// fields as raw JSON so it never clobbers data it doesn't understand.
func ensureProjectTrusted(configPath, projectPath string) error {
	data, err := os.ReadFile(configPath)
	if err != nil {
		return err
	}
	var root map[string]json.RawMessage
	if err := json.Unmarshal(data, &root); err != nil {
		return err
	}
	projects := map[string]json.RawMessage{}
	if raw, ok := root["projects"]; ok {
		if err := json.Unmarshal(raw, &projects); err != nil {
			return err
		}
	}
	proj := map[string]json.RawMessage{}
	if raw, ok := projects[projectPath]; ok {
		if err := json.Unmarshal(raw, &proj); err != nil {
			return err
		}
	}
	if raw, ok := proj["hasTrustDialogAccepted"]; ok {
		var accepted bool
		if err := json.Unmarshal(raw, &accepted); err == nil && accepted {
			return nil // already trusted
		}
	}
	proj["hasTrustDialogAccepted"] = json.RawMessage("true")
	projBytes, err := json.Marshal(proj)
	if err != nil {
		return err
	}
	projects[projectPath] = projBytes
	projectsBytes, err := json.Marshal(projects)
	if err != nil {
		return err
	}
	root["projects"] = projectsBytes
	out, err := json.Marshal(root)
	if err != nil {
		return err
	}
	tmp := configPath + ".tmp"
	if err := os.WriteFile(tmp, out, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, configPath)
}

// EditorSpec returns the spec for a workspace's nvim session.
func (c *Controller) EditorSpec() session.Spec {
	return session.Spec{Kind: session.Editor, Title: "nvim", Argv: []string{c.cfg.Editor}}
}

// TermSpec returns the spec for a workspace's shell session.
func (c *Controller) TermSpec() session.Spec {
	return session.Spec{Kind: session.Terminal, Title: "term", Argv: []string{c.cfg.Shell}}
}

// agentTitle is the palette/display title for an agent with the given label,
// falling back to a plain "claude" when it has none.
func agentTitle(label string) string {
	if label == "" {
		return "claude"
	}
	return label
}

// PromptAgentSpec returns a spec for a brand-new Claude agent session with
// --dangerously-skip-permissions set, for autonomous background execution.
func (c *Controller) PromptAgentSpec(label string) session.Spec {
	id := newSessionID()
	argv := []string{c.cfg.Agent, "--session-id", id, "--teammate-mode", "in-process", "--dangerously-skip-permissions"}
	if label != "" {
		argv = append(argv, "-n", label)
	}
	return session.Spec{Kind: session.Agent, Title: agentTitle(label), Argv: argv, SessionID: id}
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
