package tui

import (
	"errors"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/KCaverly/caretaker/internal/repo"
	"github.com/KCaverly/caretaker/internal/stack"
)

// stackModel builds a deck with a single stackable active worktree (2 commits
// ahead of a known main) and stubbed pipeline funcs, so tests never touch git/gh.
func stackModel() (Model, string) {
	m := sampleModel()
	m.focus = focusActive
	m.active = []activeItem{{
		repo: repo.Repo{Name: "repo"},
		view: WorktreeView{
			WT:         repo.Worktree{Repo: "repo", Name: "wt", Branch: "wt", Path: "/repo/wt"},
			HasBase:    true,
			BaseBranch: "main",
			Ahead:      2,
			CommitTime: time.Now().Add(-2 * time.Hour).Unix(),
			Subject:    "tip subject",
		},
	}}
	m.activeCursor = 0
	return m, wsKey("repo", "wt")
}

// openPR is a small helper for building an open commit with a PR at a given
// checks summary.
func openPR(number int, checks string) stack.Commit {
	return stack.Commit{
		Position: number,
		State:    stack.StateOpen,
		PR:       &stack.PR{Number: number, Checks: stack.Checks{Summary: checks}},
	}
}

// statusWith assembles a StackStatus from a rollup and commits, GitHub available.
func statusWith(stk stack.Stack, commits ...stack.Commit) stack.StackStatus {
	return stack.StackStatus{
		Repo: "repo", Worktree: "wt", Branch: "wt", MainBranch: "main",
		GitHub:  stack.GitHub{Available: true},
		Stack:   stk,
		Commits: commits,
	}
}

func TestDeckStackGlyph(t *testing.T) {
	cases := []struct {
		name  string
		st    stack.StackStatus
		glyph string // "" means nothing should show
	}{
		{
			name: "restack needed",
			st: statusWith(
				stack.Stack{Size: 2, BaseChainOK: true, NextAction: "restack",
					Counts: map[stack.State]int{stack.StateMerged: 1, stack.StateOpen: 1}},
				stack.Commit{State: stack.StateMerged}, openPR(1, "passing")),
			glyph: "⟳",
		},
		{
			name: "all open passing",
			st: statusWith(
				stack.Stack{Size: 2, BaseChainOK: true, NextAction: "merge",
					Counts: map[stack.State]int{stack.StateOpen: 2}},
				openPR(1, "passing"), openPR(2, "passing")),
			glyph: "✓",
		},
		{
			name: "checks pending",
			st: statusWith(
				stack.Stack{Size: 2, BaseChainOK: true, NextAction: "wait",
					Counts: map[stack.State]int{stack.StateOpen: 2}},
				openPR(1, "passing"), openPR(2, "pending")),
			glyph: "…",
		},
		{
			name: "closed PR escalates",
			st: statusWith(
				stack.Stack{Size: 1, BaseChainOK: true, NextAction: "escalate",
					Counts: map[stack.State]int{stack.StateClosed: 1}},
				stack.Commit{State: stack.StateClosed}),
			glyph: "!",
		},
		{
			name: "duplicate id escalates",
			st: statusWith(
				stack.Stack{Size: 1, BaseChainOK: true, NextAction: "escalate",
					Counts: map[stack.State]int{stack.StateDuplicateID: 1}},
				stack.Commit{State: stack.StateDuplicateID}),
			glyph: "!",
		},
		{
			name: "broken base chain escalates",
			st: statusWith(
				stack.Stack{Size: 1, BaseChainOK: false, NextAction: "submit",
					Counts: map[stack.State]int{stack.StateDiverged: 1}},
				stack.Commit{State: stack.StateDiverged}),
			glyph: "!",
		},
		{
			name: "entirely unsubmitted shows nothing",
			st: statusWith(
				stack.Stack{Size: 2, BaseChainOK: true, NextAction: "submit",
					Counts: map[stack.State]int{stack.StateUnsubmitted: 2}},
				stack.Commit{State: stack.StateUnsubmitted}, stack.Commit{State: stack.StateUnsubmitted}),
			glyph: "",
		},
		{
			name: "github unavailable shows nothing",
			st: stack.StackStatus{
				GitHub: stack.GitHub{Available: false},
				Stack: stack.Stack{Size: 1, BaseChainOK: true, NextAction: "merge",
					Counts: map[stack.State]int{stack.StateOpen: 1}},
				Commits: []stack.Commit{openPR(1, "passing")},
			},
			glyph: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g, _, show := deckStackGlyph(tc.st)
			if tc.glyph == "" {
				if show {
					t.Fatalf("expected no glyph, got %q", g)
				}
				return
			}
			if !show || g != tc.glyph {
				t.Fatalf("want glyph %q, got %q (show=%v)", tc.glyph, g, show)
			}
		})
	}
}

func TestStackDetailSegment(t *testing.T) {
	// Single-commit stack reads the PR number, state, and a check glyph.
	single := statusWith(
		stack.Stack{Size: 1, BaseChainOK: true, NextAction: "merge",
			Counts: map[stack.State]int{stack.StateOpen: 1}},
		stack.Commit{Position: 1, State: stack.StateOpen,
			PR: &stack.PR{Number: 42, Checks: stack.Checks{Summary: "passing"}}})
	if got, want := stackDetailSegment(single), "PR #42 open · checks ✓"; got != want {
		t.Errorf("single: want %q, got %q", want, got)
	}

	// Multi-commit stack rolls up size, merged count, and next action.
	multi := statusWith(
		stack.Stack{Size: 3, BaseChainOK: true, NextAction: "restack",
			Counts: map[stack.State]int{stack.StateMerged: 1, stack.StateOpen: 2}},
		stack.Commit{State: stack.StateMerged}, openPR(2, "passing"), openPR(3, "passing"))
	if got, want := stackDetailSegment(multi), "stack 3 · 1 merged · next: restack"; got != want {
		t.Errorf("multi: want %q, got %q", want, got)
	}

	// No segment without GitHub data.
	if got := stackDetailSegment(stack.StackStatus{GitHub: stack.GitHub{Available: false}}); got != "" {
		t.Errorf("gh-unavailable should yield no segment, got %q", got)
	}
}

// TestDeckByteIdenticalWithoutData is the acceptance guard: with no cache entry,
// a loading entry, an errored entry, or a gh-unavailable status, the deck row +
// detail must render exactly as they do with no stack data at all.
func TestDeckByteIdenticalWithoutData(t *testing.T) {
	render := func(m Model) string {
		lines, _ := m.activeDisplay(m.width - 4)
		return strings.Join(lines, "\n")
	}

	base, key := stackModel()
	want := render(base)

	variants := map[string]stackEntry{
		"loading":         {loading: true},
		"errored":         {err: errors.New("boom"), fetchedAt: time.Now()},
		"gh-unavailable":  {status: stack.StackStatus{GitHub: stack.GitHub{Available: false}}, fetchedAt: time.Now()},
		"empty-stack":     {status: statusWith(stack.Stack{Size: 0, Counts: map[stack.State]int{}}), fetchedAt: time.Now()},
		"all-unsubmitted": {status: statusWith(stack.Stack{Size: 2, Counts: map[stack.State]int{stack.StateUnsubmitted: 2}}), fetchedAt: time.Now()},
	}
	for name, e := range variants {
		m, _ := stackModel()
		m.stackInfo[key] = e
		if got := render(m); got != want {
			t.Errorf("%s: deck should render byte-identically\n want:\n%s\n got:\n%s", name, want, got)
		}
	}

	// A real glyph, by contrast, must change the row — proving the guard above is
	// meaningful and not just always-equal.
	m, _ := stackModel()
	m.stackInfo[key] = stackEntry{status: statusWith(
		stack.Stack{Size: 1, BaseChainOK: true, NextAction: "merge",
			Counts: map[stack.State]int{stack.StateOpen: 1}},
		openPR(1, "passing")), fetchedAt: time.Now()}
	if got := render(m); got == want {
		t.Fatal("a passing stack should add a glyph and change the row")
	}
}

// TestStackCacheKickAndFreshness covers the passive cache lifecycle: a kick marks
// entries loading and issues a command, a status msg fills the cache, a second
// kick respects the freshness window, and force ignores it.
func TestStackCacheKickAndFreshness(t *testing.T) {
	m, key := stackModel()
	var calls int
	m.stackFetch = func(p stack.Params) (stack.StackStatus, error) {
		calls++
		if p.RepoName != "repo" || p.WorktreeName != "wt" || p.MainBranch != "main" || p.Fetch {
			t.Fatalf("unexpected params: %+v", p)
		}
		return statusWith(stack.Stack{Size: 1, NextAction: "merge", Counts: map[stack.State]int{stack.StateOpen: 1}}, openPR(1, "passing")), nil
	}

	cmds := m.kickStackFetches(false)
	if len(cmds) != 1 {
		t.Fatalf("expected one kick command, got %d", len(cmds))
	}
	if !m.stackInfo[key].loading {
		t.Fatal("kick should mark the entry loading")
	}
	// Running the command yields the status msg; applying it fills the cache.
	msg := cmds[0]()
	sm, ok := msg.(stackStatusMsg)
	if !ok {
		t.Fatalf("kick command should return a stackStatusMsg, got %T", msg)
	}
	m.applyStackStatus(sm)
	if calls != 1 || m.stackInfo[key].loading || m.stackInfo[key].err != nil {
		t.Fatalf("status should be cached and no longer loading (calls=%d)", calls)
	}

	// A fresh entry is skipped by a normal kick, re-issued by a forced one.
	if got := m.kickStackFetches(false); len(got) != 0 {
		t.Fatalf("fresh entry should not re-kick, got %d", len(got))
	}
	if got := m.kickStackFetches(true); len(got) != 1 {
		t.Fatalf("forced kick should re-issue, got %d", len(got))
	}

	// A stale entry re-kicks without force.
	m.stackInfo[key] = stackEntry{status: sm.status, fetchedAt: time.Now().Add(-2 * stackFreshFor)}
	if got := m.kickStackFetches(false); len(got) != 1 {
		t.Fatalf("stale entry should re-kick, got %d", len(got))
	}
}

// TestStackPaletteRows checks the verb rows appear and disappear with cached
// state: status is always offered, restack only when the rollup calls for it, and
// submit only when there is submit-able work.
func TestStackPaletteRows(t *testing.T) {
	has := func(m Model, prefix string) bool {
		for _, c := range m.paletteCommands() {
			if strings.HasPrefix(c.title, prefix) {
				return true
			}
		}
		return false
	}

	// No cache yet: status row present, restack/submit absent.
	m, key := stackModel()
	if !has(m, "stack status: repo/wt") {
		t.Error("status row should always be present for a stackable worktree")
	}
	if has(m, "restack: repo/wt") || has(m, "submit stack: repo/wt") {
		t.Error("restack/submit rows should be absent without a matching cache")
	}

	// Restack-needed cache: restack row appears (with a landed-count hint).
	m.stackInfo[key] = stackEntry{status: statusWith(
		stack.Stack{Size: 2, BaseChainOK: true, NextAction: "restack",
			Counts: map[stack.State]int{stack.StateMerged: 1, stack.StateOpen: 1}},
		stack.Commit{State: stack.StateMerged}, openPR(2, "passing")), fetchedAt: time.Now()}
	if !has(m, "restack: repo/wt") {
		t.Error("restack row should appear when the rollup calls for a restack")
	}
	if has(m, "submit stack: repo/wt") {
		t.Error("submit row should be absent with no submit-able work")
	}

	// Submit-able cache: submit row appears, restack does not.
	m.stackInfo[key] = stackEntry{status: statusWith(
		stack.Stack{Size: 1, BaseChainOK: true, NextAction: "submit",
			Counts: map[stack.State]int{stack.StateUnpushed: 1}},
		stack.Commit{State: stack.StateUnpushed}), fetchedAt: time.Now()}
	if !has(m, "submit stack: repo/wt") {
		t.Error("submit row should appear with submit-able work")
	}
	if has(m, "restack: repo/wt") {
		t.Error("restack row should be absent when no restack is needed")
	}
}

// TestStackOverlayStatus opens the overlay via the palette status row and checks
// the title and body render from the fetched status.
func TestStackOverlayStatus(t *testing.T) {
	m, key := stackModel()
	m.stackFetch = func(stack.Params) (stack.StackStatus, error) {
		return statusWith(stack.Stack{Size: 1, BaseChainOK: true, NextAction: "merge",
			Counts: map[stack.State]int{stack.StateOpen: 1}}, openPR(7, "passing")), nil
	}
	cmd := runPaletteRow(t, &m, "stack status: repo/wt")
	if !m.stackOpen || m.stackView.key != key || !m.stackView.working {
		t.Fatal("status row should open the overlay in its working state")
	}
	m.applyStackStatus(cmd().(stackStatusMsg))
	if m.stackView.working {
		t.Fatal("overlay should leave the working state after the status lands")
	}
	out := m.renderStack(m.height - barHeight)
	if !strings.Contains(out, "STACK") || !strings.Contains(strings.ToLower(out), "repo") {
		t.Errorf("overlay should show the STACK title:\n%s", out)
	}
	if !strings.Contains(out, "next=merge") {
		t.Errorf("overlay body should include the rendered status:\n%s", out)
	}
}

// TestStackOverlayRestackConfirm walks the restack path: the dry-run plan renders
// first, enter runs the real restack (stub records both calls), and — from a fresh
// plan — esc cancels without ever executing.
func TestStackOverlayRestackConfirm(t *testing.T) {
	restackCache := stackEntry{status: statusWith(
		stack.Stack{Size: 2, BaseChainOK: true, NextAction: "restack",
			Counts: map[stack.State]int{stack.StateMerged: 1, stack.StateOpen: 1}},
		stack.Commit{State: stack.StateMerged}, openPR(2, "passing")), fetchedAt: time.Now()}

	newModel := func(rec *[]bool) (Model, string) {
		m, key := stackModel()
		m.stackInfo[key] = restackCache
		m.stackRestack = func(o stack.RestackOptions) (stack.RestackResult, error) {
			*rec = append(*rec, o.DryRun)
			res := stack.RestackResult{
				Status:    statusWith(stack.Stack{Size: 1, NextAction: "wait", Counts: map[stack.State]int{stack.StateOpen: 1}}, openPR(2, "passing")),
				RebaseCmd: []string{"git", "rebase", "--onto", "main"},
				Drops:     []stack.DropAction{{Position: 1, ShortSHA: "abc1234", Subject: "landed commit", Number: 5}},
			}
			res.DryRun = o.DryRun
			return res, nil
		}
		return m, key
	}

	// Enter executes.
	var calls []bool
	m, _ := newModel(&calls)
	cmd := runPaletteRow(t, &m, "restack: repo/wt")
	m.applyStackRestack(cmd().(stackRestackMsg))
	if !m.stackView.confirmRestack {
		t.Fatal("dry-run should arm the restack confirm state")
	}
	if !strings.Contains(m.renderStack(m.height-barHeight), "restack plan") {
		t.Errorf("overlay should show the dry-run plan:\n%s", m.renderStack(m.height-barHeight))
	}
	mm, cmd2 := m.handleStack(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = mm.(Model)
	if cmd2 == nil {
		t.Fatal("enter on the plan should issue the real restack")
	}
	m.applyStackRestack(cmd2().(stackRestackMsg))
	if len(calls) != 2 || calls[0] != true || calls[1] != false {
		t.Fatalf("expected dry-run then real run, got %v", calls)
	}
	if m.stackView.confirmRestack {
		t.Error("a completed restack should clear the confirm state")
	}

	// Esc cancels without executing for real.
	var calls2 []bool
	m2, _ := newModel(&calls2)
	cmd = runPaletteRow(t, &m2, "restack: repo/wt")
	m2.applyStackRestack(cmd().(stackRestackMsg))
	mm, _ = m2.handleStack(tea.KeyPressMsg{Code: tea.KeyEscape})
	m2 = mm.(Model)
	if m2.stackOpen {
		t.Error("esc should close the overlay")
	}
	if len(calls2) != 1 || calls2[0] != true {
		t.Fatalf("esc must not execute the restack, got calls %v", calls2)
	}
}

// TestStackOverlayScroll checks the body scrolls only when it overflows the
// viewport.
func TestStackOverlayScroll(t *testing.T) {
	m, key := stackModel()
	m = m.enterStackOverlay(key, "repo", "wt", stack.Params{})
	m.stackView.working = false

	// A short body cannot scroll.
	m.stackView.body = []string{"one", "two"}
	mm, _ := m.handleStack(tea.KeyPressMsg{Code: tea.KeyDown})
	if mm.(Model).stackView.offset != 0 {
		t.Error("a body that fits should not scroll")
	}

	// A body taller than the viewport scrolls, clamped to the last window.
	long := make([]string, m.stackViewport()+10)
	for i := range long {
		long[i] = "line"
	}
	m.stackView.body = long
	mm, _ = m.handleStack(tea.KeyPressMsg{Code: tea.KeyDown})
	m = mm.(Model)
	if m.stackView.offset != 1 {
		t.Fatalf("down should scroll by one, got %d", m.stackView.offset)
	}
}

// runPaletteRow finds the palette row with the given title prefix, runs it, and
// stores the resulting model back through p. It fails the test if no row matches.
func runPaletteRow(t *testing.T, p *Model, prefix string) tea.Cmd {
	t.Helper()
	for _, c := range p.paletteCommands() {
		if strings.HasPrefix(c.title, prefix) {
			mm, cmd := c.run(*p)
			*p = mm.(Model)
			return cmd
		}
	}
	t.Fatalf("no palette row with prefix %q", prefix)
	return nil
}
