package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	tea "charm.land/bubbletea/v2"

	"github.com/KCaverly/caretaker/internal/config"
	"github.com/KCaverly/caretaker/internal/repo"
	"github.com/KCaverly/caretaker/internal/session"
	"github.com/KCaverly/caretaker/internal/stack"
	"github.com/KCaverly/caretaker/internal/tui"
)

// These values are replaced by the release build. They live in main.go so
// `go run ./cmd/ct/main.go` remains a complete development entrypoint.
var (
	version = "dev"
	commit  = "unknown"
	date    = "unknown"
)

func versionString() string {
	return fmt.Sprintf("ct %s (commit %s, built %s)", version, commit, date)
}

func main() {
	if len(os.Args) > 1 && (os.Args[1] == "version" || os.Args[1] == "--version") {
		fmt.Println(versionString())
		return
	}
	// The `stack` command group runs headless, before any TUI setup: it prints to
	// stdout and exits, so it must not touch the Bubble Tea program or config.
	if len(os.Args) > 1 && os.Args[1] == "stack" {
		os.Exit(runStack(os.Args[2:]))
	}
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "ct:", err)
		os.Exit(1)
	}
}

// runStack dispatches the `ct stack …` subcommands and returns a process exit
// code. Only usage/resolution errors are non-zero; a successfully computed
// status exits 0 regardless of how troubled the stack is, because the state
// lives in the output, not the exit code.
func runStack(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: ct stack (status|submit|restack|finish|merge|setup) [flags] [-C <dir>]")
		return 2
	}
	switch args[0] {
	case "status":
		return runStackStatus(args[1:])
	case "submit":
		return runStackSubmit(args[1:])
	case "restack":
		return runStackRestack(args[1:])
	case "finish":
		return runStackFinish(args[1:])
	case "merge":
		return runStackMerge(args[1:])
	case "setup":
		return runStackSetup(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "ct stack: unknown subcommand %q\n", args[0])
		fmt.Fprintln(os.Stderr, "usage: ct stack (status|submit|restack|finish|merge|setup) [flags] [-C <dir>]")
		return 2
	}
}

func runStackSetup(args []string) int {
	var asJSON, enable bool
	var dir string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--json":
			asJSON = true
		case "--enable-auto-delete":
			enable = true
		case "-C":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "ct stack setup: -C requires a directory argument")
				return 2
			}
			i++
			dir = args[i]
		default:
			fmt.Fprintf(os.Stderr, "ct stack setup: unknown argument %q\n", args[i])
			return 2
		}
	}
	if dir == "" {
		var err error
		dir, err = os.Getwd()
		if err != nil {
			fmt.Fprintln(os.Stderr, "ct stack setup:", err)
			return 1
		}
	}
	settings, err := stack.Setup(stack.SetupOptions{Dir: dir, EnableAutoDelete: enable})
	if err != nil {
		fmt.Fprintln(os.Stderr, "ct stack setup:", err)
		return 1
	}
	if asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(settings); err != nil {
			fmt.Fprintln(os.Stderr, "ct stack setup:", err)
			return 1
		}
		return 0
	}
	state := "disabled"
	if settings.DeleteBranchOnMerge {
		state = "enabled"
	}
	fmt.Printf("%s\n  automatic branch deletion: %s\n", settings.Repository, state)
	if !settings.DeleteBranchOnMerge {
		fmt.Println("  enable with: ct stack setup --enable-auto-delete")
	}
	return 0
}

func runStackFinish(args []string) int {
	var asJSON, dryRun bool
	var dir string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--json":
			asJSON = true
		case "--dry-run":
			dryRun = true
		case "-C":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "ct stack finish: -C requires a directory argument")
				return 2
			}
			i++
			dir = args[i]
		default:
			fmt.Fprintf(os.Stderr, "ct stack finish: unknown argument %q\n", args[i])
			return 2
		}
	}
	if dir == "" {
		var err error
		dir, err = os.Getwd()
		if err != nil {
			fmt.Fprintln(os.Stderr, "ct stack finish:", err)
			return 1
		}
	}
	params, err := resolveStackParams(dir, false)
	if err != nil {
		fmt.Fprintln(os.Stderr, "ct stack finish:", err)
		return 1
	}
	res, err := stack.Finish(stack.FinishOptions{Params: params, DryRun: dryRun})
	if err != nil {
		for _, done := range res.Executed {
			fmt.Fprintln(os.Stderr, "  did:", done)
		}
		fmt.Fprintln(os.Stderr, "ct stack finish:", err)
		return 1
	}
	if dryRun {
		if asJSON {
			return encodeStackJSON(res.Status, &res.Plan)
		}
		fmt.Print(stack.RenderFinishPlan(res))
		return 0
	}
	if asJSON {
		return encodeStackJSON(res.Status, nil)
	}
	fmt.Print(stack.Render(res.Status))
	return 0
}

func runStackMerge(args []string) int {
	var dir string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-C":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "ct stack merge: -C requires a directory argument")
				return 2
			}
			i++
			dir = args[i]
		default:
			fmt.Fprintf(os.Stderr, "ct stack merge: unknown argument %q\n", args[i])
			return 2
		}
	}
	if dir == "" {
		var err error
		dir, err = os.Getwd()
		if err != nil {
			fmt.Fprintln(os.Stderr, "ct stack merge:", err)
			return 1
		}
	}
	params, err := resolveStackParams(dir, false)
	if err != nil {
		fmt.Fprintln(os.Stderr, "ct stack merge:", err)
		return 1
	}
	res, err := stack.Merge(stack.MergeOptions{Params: params})
	if err != nil {
		fmt.Fprintln(os.Stderr, "ct stack merge:", err)
		return 1
	}
	fmt.Print(stack.Render(res.Status))
	return 0
}

// runStackRestack resolves the containing worktree, then runs the restack
// pipeline: guards, fetch, a rebase --onto that drops the landed bottom prefix,
// deletion of the landed remote branches, and the submit convergence pipeline for
// the survivors. --dry-run plans without mutating (the fetch aside); --json emits
// the schema-1 status (plus a top-level "plan" field under --dry-run). Any
// usage/resolution/pipeline failure exits non-zero.
func runStackRestack(args []string) int {
	var (
		asJSON bool
		dryRun bool
		dir    string
	)
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--json":
			asJSON = true
		case "--dry-run":
			dryRun = true
		case "-C":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "ct stack restack: -C requires a directory argument")
				return 2
			}
			i++
			dir = args[i]
		default:
			fmt.Fprintf(os.Stderr, "ct stack restack: unknown argument %q\n", args[i])
			return 2
		}
	}

	if dir == "" {
		cwd, err := os.Getwd()
		if err != nil {
			fmt.Fprintln(os.Stderr, "ct stack restack:", err)
			return 1
		}
		dir = cwd
	}

	// Restack always fetches internally; the resolve step needn't.
	params, err := resolveStackParams(dir, false)
	if err != nil {
		fmt.Fprintln(os.Stderr, "ct stack restack:", err)
		return 1
	}

	res, err := stack.Restack(stack.RestackOptions{Params: params, DryRun: dryRun})
	if err != nil {
		for _, done := range res.Executed {
			fmt.Fprintln(os.Stderr, "  did:", done)
		}
		fmt.Fprintln(os.Stderr, "ct stack restack:", err)
		return 1
	}

	if res.Nothing {
		if asJSON {
			return encodeStackJSON(res.Status, nil)
		}
		fmt.Println("nothing to restack")
		return 0
	}

	if dryRun {
		if asJSON {
			return encodeStackJSON(res.Status, &res.Plan)
		}
		fmt.Print(stack.RenderRestackPlan(res))
		return 0
	}

	if asJSON {
		return encodeStackJSON(res.Status, nil)
	}
	fmt.Print(stack.Render(res.Status))
	return 0
}

// runStackSubmit resolves the containing worktree, then runs the submit pipeline
// (fetch, guard, trailer injection, push, PR create/retarget/retitle, nav-table
// splice), converging the remote to the local stack. --dry-run plans without
// mutating; --draft opens PRs as drafts; --json emits the schema-1 status (plus a
// top-level "plan" field under --dry-run). Any usage/resolution/pipeline failure
// exits non-zero.
func runStackSubmit(args []string) int {
	var (
		asJSON bool
		dryRun bool
		draft  bool
		dir    string
	)
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--json":
			asJSON = true
		case "--dry-run":
			dryRun = true
		case "--draft":
			draft = true
		case "-C":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "ct stack submit: -C requires a directory argument")
				return 2
			}
			i++
			dir = args[i]
		default:
			fmt.Fprintf(os.Stderr, "ct stack submit: unknown argument %q\n", args[i])
			return 2
		}
	}

	if dir == "" {
		cwd, err := os.Getwd()
		if err != nil {
			fmt.Fprintln(os.Stderr, "ct stack submit:", err)
			return 1
		}
		dir = cwd
	}

	// Submit always fetches internally; the resolve step needn't.
	params, err := resolveStackParams(dir, false)
	if err != nil {
		fmt.Fprintln(os.Stderr, "ct stack submit:", err)
		return 1
	}

	res, err := stack.Submit(stack.SubmitOptions{Params: params, DryRun: dryRun, Draft: draft})
	if err != nil {
		for _, done := range res.Executed {
			fmt.Fprintln(os.Stderr, "  did:", done)
		}
		fmt.Fprintln(os.Stderr, "ct stack submit:", err)
		return 1
	}

	if res.Nothing {
		if asJSON {
			return encodeStackJSON(res.Status, nil)
		}
		fmt.Println("nothing to submit")
		return 0
	}

	if dryRun {
		if asJSON {
			return encodeStackJSON(res.Status, &res.Plan)
		}
		fmt.Print(stack.RenderPlan(res.Status, res.Plan))
		return 0
	}

	if asJSON {
		return encodeStackJSON(res.Status, nil)
	}
	fmt.Print(stack.Render(res.Status))
	return 0
}

// encodeStackJSON prints a StackStatus as indented JSON. When plan is non-nil (a
// dry-run), it adds a top-level "plan" field — additive to schema 1 — by
// embedding the status so its fields stay at the top level.
func encodeStackJSON(st stack.StackStatus, plan *stack.Plan) int {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	var payload any = st
	if plan != nil {
		payload = struct {
			stack.StackStatus
			Plan stack.Plan `json:"plan"`
		}{st, *plan}
	}
	if err := enc.Encode(payload); err != nil {
		fmt.Fprintln(os.Stderr, "ct stack submit:", err)
		return 1
	}
	return 0
}

// runStackStatus resolves the containing worktree (and its repo's primary tree,
// for main_branch), computes the read-only stack status, and prints it as JSON or
// a compact table.
func runStackStatus(args []string) int {
	var (
		asJSON bool
		fetch  bool
		dir    string
	)
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--json":
			asJSON = true
		case "--fetch":
			fetch = true
		case "-C":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "ct stack status: -C requires a directory argument")
				return 2
			}
			i++
			dir = args[i]
		default:
			fmt.Fprintf(os.Stderr, "ct stack status: unknown argument %q\n", args[i])
			return 2
		}
	}

	if dir == "" {
		cwd, err := os.Getwd()
		if err != nil {
			fmt.Fprintln(os.Stderr, "ct stack status:", err)
			return 1
		}
		dir = cwd
	}

	params, err := resolveStackParams(dir, fetch)
	if err != nil {
		fmt.Fprintln(os.Stderr, "ct stack status:", err)
		return 1
	}

	st, err := stack.Status(params)
	if err != nil {
		fmt.Fprintln(os.Stderr, "ct stack status:", err)
		return 1
	}

	if asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(st); err != nil {
			fmt.Fprintln(os.Stderr, "ct stack status:", err)
			return 1
		}
	} else {
		fmt.Print(stack.Render(st))
	}
	return 0
}

// resolveStackParams locates the git worktree containing dir, confirms it is a
// linked (non-primary) worktree with a branch checked out, and derives the
// primary worktree's branch as main_branch. It reuses repo.ListWorktrees rather
// than re-parsing `git worktree list`, since that output is repo-global and
// lists the primary tree first.
func resolveStackParams(dir string, fetch bool) (stack.Params, error) {
	toplevel, err := repo.Git(dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return stack.Params{}, fmt.Errorf("not inside a git worktree: %w", err)
	}
	toplevel = trimLine(toplevel)

	branch, err := repo.Git(dir, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return stack.Params{}, err
	}
	branch = trimLine(branch)
	if branch == "HEAD" {
		return stack.Params{}, fmt.Errorf("worktree at %s has a detached HEAD", toplevel)
	}

	wts, err := repo.ListWorktrees(repo.Repo{Path: toplevel})
	if err != nil {
		return stack.Params{}, err
	}
	if len(wts) == 0 {
		return stack.Params{}, fmt.Errorf("could not list worktrees for %s", toplevel)
	}
	primary := wts[0]

	cur := findWorktree(wts, toplevel, branch)
	if cur == nil {
		return stack.Params{}, fmt.Errorf("could not locate the worktree for %s among the repo's worktrees", toplevel)
	}
	if cur.IsMain {
		return stack.Params{}, fmt.Errorf("stack status must be run from a linked worktree, not the primary tree %s", toplevel)
	}

	return stack.Params{
		RepoName:     filepath.Base(primary.Path),
		WorktreeName: cur.Name,
		WorktreeDir:  cur.Path,
		Branch:       cur.Branch,
		MainBranch:   primary.Branch,
		Fetch:        fetch,
	}, nil
}

// findWorktree picks the worktree matching the current tree, preferring a path
// match (symlinks resolved, since macOS /tmp is a symlink) and falling back to a
// branch match in ct's one-branch-per-worktree model.
func findWorktree(wts []repo.Worktree, toplevel, branch string) *repo.Worktree {
	target := resolveSymlinks(toplevel)
	for i := range wts {
		if resolveSymlinks(wts[i].Path) == target {
			return &wts[i]
		}
	}
	for i := range wts {
		if wts[i].Branch == branch {
			return &wts[i]
		}
	}
	return nil
}

// resolveSymlinks canonicalises a path for comparison, falling back to the input
// when it can't be resolved (e.g. a path that no longer exists).
func resolveSymlinks(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return p
}

// trimLine strips the trailing newline git appends to single-line command output.
func trimLine(s string) string {
	if len(s) > 0 && s[len(s)-1] == '\n' {
		s = s[:len(s)-1]
	}
	if len(s) > 0 && s[len(s)-1] == '\r' {
		s = s[:len(s)-1]
	}
	return s
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
