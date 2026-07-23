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

	"github.com/KCaverly/caretaker/internal/agent"
	"github.com/KCaverly/caretaker/internal/codex"
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
	// startCodex is replaceable in tests; production uses the pane-local Codex
	// App Server observer.
	startCodex func(context.Context, codex.Config) (codexRuntime, error)
}

type codexRuntime interface {
	Close() error
	Remote() string
	EventStream() <-chan agent.Event
}

// NewController builds a Controller from config.
func NewController(cfg config.Config) *Controller {
	// Keep programmatic callers that populate only the legacy Agent field on a
	// safe Claude-only fallback. Config loaded through config.Load already has
	// both built-in providers enabled by default.
	if len(cfg.Agents.Enabled) == 0 {
		cfg.Agents.Enabled = []agent.Provider{agent.Claude}
	}
	if !cfg.Agents.Default.Valid() {
		cfg.Agents.Default = agent.Claude
	}
	if cfg.Agents.Claude.Command == "" {
		cfg.Agents.Claude.Command = cfg.Agent
	}
	return &Controller{cfg: cfg}
}

func (c *Controller) startCodexRuntime(ctx context.Context, cfg codex.Config) (codexRuntime, error) {
	if c.startCodex != nil {
		return c.startCodex(ctx, cfg)
	}
	return codex.Start(ctx, cfg)
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

// Keys returns the reserved keystrokes (not forwarded to embedded sessions).
// config.Load decodes the user's TOML over config.Default, so every field is
// already populated with its default when unset — the TUI can use them directly.
func (c *Controller) Keys() config.Keys { return c.cfg.Keys }

// UsageThreshold returns the utilization percent at/above which the bar's
// usage gauge appears (0 = always, >100 = never).
func (c *Controller) UsageThreshold() int { return c.cfg.Usage.Threshold }

// PlasmaConfig returns the deck plasma-panel settings.
func (c *Controller) PlasmaConfig() config.Plasma { return c.cfg.Plasma }

// StackAutoMerge reports whether an eligible stack merge should bypass ct's
// confirmation panel. The configuration default is deliberately false.
func (c *Controller) StackAutoMerge() bool { return c.cfg.Stack.AutoMerge }

// GlobalConfigDir returns the home directory path for the global config workspace.
func (c *Controller) GlobalConfigDir() (string, error) { return os.UserHomeDir() }

// EnsureHomeDirTrusted marks the home directory as trusted in Claude's global
// config (~/.claude.json, under projects[home].hasTrustDialogAccepted) so that
// agents started in the home workspace don't pause to show the workspace trust
// dialog. It is idempotent and safe to call before
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

// agentTitle is the palette/display title for a provider agent with the given
// label, falling back to its provider name when it has none.
func agentTitle(provider agent.Provider, label string) string {
	if label == "" {
		return provider.String()
	}
	return label
}

// EnabledAgentProviders returns the configured provider choices in palette
// order. The returned slice is independent of the controller's configuration.
// A zero-value/legacy controller remains Claude-only.
func (c *Controller) EnabledAgentProviders() []agent.Provider {
	if len(c.cfg.Agents.Enabled) == 0 {
		return []agent.Provider{agent.Claude}
	}
	return append([]agent.Provider(nil), c.cfg.Agents.Enabled...)
}

// DefaultAgentProvider returns the provider initially selected by the new-agent
// form. A zero-value/legacy controller defaults to Claude.
func (c *Controller) DefaultAgentProvider() agent.Provider {
	if c.cfg.Agents.Default.Valid() {
		return c.cfg.Agents.Default
	}
	return agent.Claude
}

// providerConfig resolves a provider's command and base arguments. Claude's
// legacy top-level Agent command remains the fallback for direct/programmatic
// configs that have not populated Agents.
func (c *Controller) providerConfig(provider agent.Provider) (config.AgentProvider, error) {
	if !provider.Valid() {
		return config.AgentProvider{}, fmt.Errorf("unknown agent provider %q", provider)
	}
	providerCfg := c.cfg.Agents.Provider(provider)
	if provider == agent.Claude && providerCfg.Command == "" {
		providerCfg.Command = c.cfg.Agent
	}
	if providerCfg.Command == "" {
		return config.AgentProvider{}, fmt.Errorf("agent provider %q has no command", provider)
	}
	return providerCfg, nil
}

// PrepareAgentSpec attaches any provider-side runtime required before the
// interactive process starts. Claude needs no companion. Each Codex pane owns
// a private App Server observed by caretaker; the stock Codex TUI connects to
// it through --remote and remains the sole approval/input responder.
func (c *Controller) PrepareAgentSpec(ctx context.Context, dir string, spec session.Spec) (session.Spec, error) {
	if normalizedProvider(spec.Provider) != agent.Codex {
		return spec, nil
	}
	providerCfg, err := c.providerConfig(agent.Codex)
	if err != nil {
		return session.Spec{}, err
	}
	insertAt := 1 + len(providerCfg.Args)
	if len(spec.Argv) < insertAt || len(spec.Argv) == 0 || spec.Argv[0] != providerCfg.Command {
		return session.Spec{}, fmt.Errorf("Codex session command does not match configured provider")
	}
	runtime, err := c.startCodexRuntime(ctx, codex.Config{
		Command: providerCfg.Command,
		Args:    providerCfg.Args,
		Dir:     dir,
	})
	if err != nil {
		return session.Spec{}, err
	}
	argv := make([]string, 0, len(spec.Argv)+2)
	argv = append(argv, spec.Argv[:insertAt]...)
	argv = append(argv, "--remote", runtime.Remote())
	argv = append(argv, spec.Argv[insertAt:]...)
	spec.Argv = argv
	spec.Events = runtime.EventStream()
	spec.Companion = runtime
	return spec, nil
}

// NewProviderAgentSpec constructs a brand-new provider session. Claude owns a
// caller-generated UUID from launch; Codex assigns its thread ID after launch,
// so a fresh Codex spec deliberately starts with an empty SessionID.
func (c *Controller) NewProviderAgentSpec(provider agent.Provider, label, prompt string) (session.Spec, error) {
	providerCfg, err := c.providerConfig(provider)
	if err != nil {
		return session.Spec{}, err
	}

	switch provider {
	case agent.Claude:
		return c.freshClaudeAgentSpec(providerCfg, newSessionID(), label, prompt), nil
	case agent.Codex:
		argv := append([]string{providerCfg.Command}, providerCfg.Args...)
		if prompt != "" {
			argv = append(argv, prompt)
		}
		return session.Spec{
			Kind: session.Agent, Title: agentTitle(provider, label), Argv: argv,
			Provider: provider,
		}, nil
	default:
		// providerConfig already validates this, but keep the switch exhaustive.
		return session.Spec{}, fmt.Errorf("unknown agent provider %q", provider)
	}
}

// RestoreProviderAgentSpec constructs a persisted provider session. Claude
// keeps its missing-transcript fallback; Codex resumes through its resume
// subcommand.
func (c *Controller) RestoreProviderAgentSpec(provider agent.Provider, id, label, prompt string) (session.Spec, error) {
	// A provider may not have reported its conversation ID before caretaker was
	// stopped. Relaunch that entry as a fresh session rather than constructing a
	// malformed resume command with an empty identifier.
	if id == "" {
		return c.NewProviderAgentSpec(provider, label, prompt)
	}
	providerCfg, err := c.providerConfig(provider)
	if err != nil {
		return session.Spec{}, err
	}

	switch provider {
	case agent.Claude:
		if !c.transcriptExists(id) {
			return c.freshClaudeAgentSpec(providerCfg, id, label, prompt), nil
		}
		return c.resumeClaudeAgentSpec(providerCfg, id, label, prompt), nil
	case agent.Codex:
		argv := append([]string{providerCfg.Command}, providerCfg.Args...)
		argv = append(argv, "resume", id)
		if prompt != "" {
			argv = append(argv, prompt)
		}
		return session.Spec{
			Kind: session.Agent, Title: agentTitle(provider, label), Argv: argv,
			Provider: provider, SessionID: id,
		}, nil
	default:
		return session.Spec{}, fmt.Errorf("unknown agent provider %q", provider)
	}
}

// NewAgentSpec returns the spec for a brand-new Claude agent session, optionally
// named. ct generates the session UUID and passes it as --session-id so it can
// persist the id and resume the same conversation in a later run. The label is
// passed to claude as -n (so it appears in `claude agents --json` and the agent
// picker), and teammate split-pane mode is pinned off so any agent team renders
// in-process inside the pane ct controls.
func (c *Controller) NewAgentSpec(label string) session.Spec {
	providerCfg, _ := c.providerConfig(agent.Claude)
	return c.freshClaudeAgentSpec(providerCfg, newSessionID(), label, "")
}

// freshAgentSpec builds the spec for a brand-new claude session running under
// the given id (shared by NewAgentSpec and the resume-fallback path).
func (c *Controller) freshAgentSpec(id, label string) session.Spec {
	providerCfg, _ := c.providerConfig(agent.Claude)
	return c.freshClaudeAgentSpec(providerCfg, id, label, "")
}

func (c *Controller) freshClaudeAgentSpec(providerCfg config.AgentProvider, id, label, prompt string) session.Spec {
	argv := append([]string{providerCfg.Command}, providerCfg.Args...)
	argv = append(argv, "--session-id", id, "--teammate-mode", "in-process")
	if label != "" {
		argv = append(argv, "-n", label)
	}
	if prompt != "" {
		argv = append(argv, prompt)
	}
	return session.Spec{
		Kind: session.Agent, Title: agentTitle(agent.Claude, label), Argv: argv,
		Provider: agent.Claude, SessionID: id, UnsetEnv: []string{"TMUX", "TERM_PROGRAM"},
	}
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
	providerCfg, _ := c.providerConfig(agent.Claude)
	return c.resumeClaudeAgentSpec(providerCfg, id, label, "")
}

func (c *Controller) resumeClaudeAgentSpec(providerCfg config.AgentProvider, id, label, prompt string) session.Spec {
	argv := append([]string{providerCfg.Command}, providerCfg.Args...)
	argv = append(argv, "--resume", id, "--teammate-mode", "in-process")
	if prompt != "" {
		argv = append(argv, prompt)
	}
	return session.Spec{
		Kind: session.Agent, Title: agentTitle(agent.Claude, label), Argv: argv,
		Provider: agent.Claude, SessionID: id, UnsetEnv: []string{"TMUX", "TERM_PROGRAM"},
	}
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
	Provider   agent.Provider
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
		out[r.Pid] = AgentStatus{Provider: agent.Claude, Status: r.Status, WaitingFor: r.WaitingFor, Cwd: r.Cwd, StartedAt: r.StartedAt}
	}
	return out, nil
}

// AgentStatuses runs `claude agents --json` and returns live session statuses
// keyed by pid. It requires no TTY and exits immediately.
func (c *Controller) AgentStatuses(ctx context.Context) (map[int]AgentStatus, error) {
	providerCfg, err := c.providerConfig(agent.Claude)
	if err != nil {
		return nil, err
	}
	argv := append(append([]string(nil), providerCfg.Args...), "agents", "--json")
	out, err := exec.CommandContext(ctx, providerCfg.Command, argv...).Output()
	if err != nil {
		return nil, err
	}
	return parseAgentStatuses(out)
}
