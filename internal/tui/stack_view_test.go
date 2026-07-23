package tui

import (
	"errors"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/KCaverly/caretaker/internal/config"
	"github.com/KCaverly/caretaker/internal/repo"
	"github.com/KCaverly/caretaker/internal/session"
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
			name: "fully landed is complete",
			st: statusWith(
				stack.Stack{Size: 1, BaseChainOK: true, NextAction: "complete",
					Counts: map[stack.State]int{stack.StateMerged: 1}},
				stack.Commit{State: stack.StateMerged}),
			glyph: "✓",
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
			name: "conflicting PR needs attention",
			st: statusWith(
				stack.Stack{Size: 1, BaseChainOK: true, NextAction: "resolve-conflicts",
					Counts: map[stack.State]int{stack.StateOpen: 1}},
				stack.Commit{State: stack.StateOpen, PR: &stack.PR{Number: 10, Mergeable: "CONFLICTING"}}),
			glyph: "!",
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

func TestStackCommitRowShowsConflicts(t *testing.T) {
	m, _ := stackModel()
	c := stack.Commit{State: stack.StateOpen, Subject: "conflicting change",
		PR: &stack.PR{Number: 10, Mergeable: "CONFLICTING", Checks: stack.Checks{Summary: "passing"}}}

	for _, selected := range []bool{false, true} {
		row := m.stackCommitRow(c, selected, 60)
		if !strings.Contains(row, "PR #10 · conflicts") {
			t.Errorf("selected=%v: conflict missing from row:\n%s", selected, row)
		}
		if strings.Contains(row, "checks") {
			t.Errorf("selected=%v: checks should not obscure conflict:\n%s", selected, row)
		}
	}
	if glyph := glyphFor(c); !strings.Contains(glyph, "✗") {
		t.Errorf("conflicting PR should use the error glyph, got %q", glyph)
	}
}

func TestConflictingCascadeOffersRestackHotkey(t *testing.T) {
	m, key := stackModel()
	st := statusWith(
		stack.Stack{Size: 2, BaseChainOK: true, NextAction: "resolve-conflicts",
			Counts: map[stack.State]int{stack.StateMerged: 1, stack.StateOpen: 1}},
		stack.Commit{State: stack.StateMerged, Subject: "landed"},
		stack.Commit{State: stack.StateOpen, Subject: "conflicting",
			PR: &stack.PR{Number: 10, Mergeable: "CONFLICTING"}})
	m = m.enterStackOverlay(key, "repo", "wt", stack.Params{})
	m.stackView.working = false
	m.stackView.status = &st

	out := m.renderStack(m.height - barHeight)
	if !strings.Contains(out, "R") || !strings.Contains(out, "restack") {
		t.Errorf("conflicting cascade should advertise the restack hotkey:\n%s", out)
	}

	m.stackRestack = func(stack.RestackOptions) (stack.RestackResult, error) {
		return stack.RestackResult{}, nil
	}
	mm, cmd := m.handleStack(tea.KeyPressMsg{Code: 'R', Text: "R"})
	if !mm.(Model).stackView.working || cmd == nil {
		t.Fatal("R should start the restack dry-run for a conflicting cascade")
	}
}

func TestMergeActionRequiresMergeableMainPR(t *testing.T) {
	ready := statusWith(
		stack.Stack{Size: 1, BaseChainOK: true, NextAction: "merge",
			Counts: map[stack.State]int{stack.StateOpen: 1}},
		stack.Commit{State: stack.StateOpen, Subject: "ready",
			PR: &stack.PR{Number: 10, Base: "main", Mergeable: "MERGEABLE"}})
	ready.MergeHint = &stack.MergeHint{Number: 10, Subject: "ready", Body: "body"}
	if !stackCanMerge(ready) {
		t.Fatal("mergeable PR targeting main should offer merge")
	}

	for _, tc := range []struct {
		name, base, mergeable string
	}{
		{"wrong base", "feature", "MERGEABLE"},
		{"conflicting", "main", "CONFLICTING"},
		{"unknown", "main", "UNKNOWN"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := ready
			st.Commits = append([]stack.Commit(nil), ready.Commits...)
			pr := *ready.Commits[0].PR
			pr.Base, pr.Mergeable = tc.base, tc.mergeable
			st.Commits[0].PR = &pr
			if stackCanMerge(st) {
				t.Fatal("non-main or non-MERGEABLE PR should not offer merge")
			}
		})
	}

	m, key := stackModel()
	m = m.enterStackOverlay(key, "repo", "wt", stack.Params{})
	m.stackView.working = false
	m.stackView.status = &ready
	if out := m.renderStack(m.height - barHeight); !strings.Contains(out, "M") || !strings.Contains(out, "merge") {
		t.Errorf("ready stack should advertise M merge:\n%s", out)
	}
	m.stackMerge = func(stack.MergeOptions) (stack.MergeResult, error) { return stack.MergeResult{}, nil }
	mm, cmd := m.handleStack(tea.KeyPressMsg{Code: 'M', Text: "M"})
	m = mm.(Model)
	if m.mode != modeConfirmMerge || cmd != nil || m.confirm.cursor != 0 {
		t.Fatal("M should open the merge confirmation on its safe option")
	}
	if out := m.renderConfirm(m.height - barHeight); !strings.Contains(out, "PR #10") ||
		!strings.Contains(out, "merge PR #10 into main") || !strings.Contains(out, "checks: unknown") {
		t.Errorf("merge confirmation should identify the target and evidence:\n%s", out)
	}
	mm, cmd = m.handleConfirmMergeKey(tea.KeyPressMsg{Code: 'x', Text: "x"})
	m = mm.(Model)
	if m.mode != modeConfirmMerge || cmd != nil {
		t.Fatal("unrelated input should be swallowed by the merge confirmation")
	}
	mm, _ = m.handleConfirmMergeKey(tea.KeyPressMsg{Code: tea.KeyDown})
	m = mm.(Model)
	mm, cmd = m.handleConfirmMergeKey(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = mm.(Model)
	if m.mode != modeNormal || !m.stackView.working || cmd == nil {
		t.Fatal("confirming should restore the stack overlay and start the merge")
	}

	m, key = stackModel()
	m.stackInfo[key] = stackEntry{status: ready, fetchedAt: time.Now()}
	found := false
	for _, c := range m.paletteCommands() {
		found = found || strings.HasPrefix(c.title, "merge PR: repo/wt")
	}
	if !found {
		t.Fatal("command palette should offer merge for a mergeable main PR")
	}

	cmd = runPaletteRow(t, &m, "merge PR: repo/wt")
	if m.mode != modeConfirmMerge || cmd != nil || !m.stackOpen {
		t.Fatal("palette merge should open the shared confirmation")
	}
	mm, _ = m.handleConfirmMergeKey(tea.KeyPressMsg{Code: tea.KeyEscape})
	m = mm.(Model)
	if m.mode != modeNormal || !m.stackOpen || m.stackView.status == nil {
		t.Fatal("cancel should return to the same loaded stack context")
	}

	m.stackAutoMerge = true
	cmd = runPaletteRow(t, &m, "merge PR: repo/wt")
	if m.mode != modeNormal || !m.stackView.working || cmd == nil {
		t.Fatal("auto_merge should execute immediately from the palette")
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

	// Multi-commit stack omits the redundant size and states the useful outcome.
	multi := statusWith(
		stack.Stack{Size: 3, BaseChainOK: true, NextAction: "restack",
			Counts: map[stack.State]int{stack.StateMerged: 1, stack.StateOpen: 2}},
		stack.Commit{State: stack.StateMerged}, openPR(2, "passing"), openPR(3, "passing"))
	if got, want := stackDetailSegment(multi), "1 merged · restack needed"; got != want {
		t.Errorf("multi: want %q, got %q", want, got)
	}

	conflict := statusWith(
		stack.Stack{Size: 2, BaseChainOK: true, NextAction: "resolve-conflicts",
			Counts: map[stack.State]int{stack.StateOpen: 2}},
		openPR(9, "passing"),
		stack.Commit{State: stack.StateOpen, PR: &stack.PR{Number: 10, Mergeable: "CONFLICTING"}})
	if got, want := stackDetailSegment(conflict), "resolve conflicts"; got != want {
		t.Errorf("conflict: want %q, got %q", want, got)
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
// state: status is always offered, restack when the rollup calls for it or a
// landed-prefix conflict can be rebased, and submit only with submit-able work.
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

	// Fully landed stacks use the same cleanup pipeline under a clearer verb.
	m.stackInfo[key] = stackEntry{status: statusWith(
		stack.Stack{Size: 1, BaseChainOK: true, NextAction: "complete",
			Counts: map[stack.State]int{stack.StateMerged: 1}},
		stack.Commit{State: stack.StateMerged}), fetchedAt: time.Now()}
	if !has(m, "reuse worktree: repo/wt") || !has(m, "archive worktree: repo/wt") || has(m, "restack: repo/wt") {
		t.Error("complete stack should offer archive and reuse rather than restack")
	}

	// A conflicting cascade can use the same restack pipeline: drop the landed
	// prefix and rebase the survivor onto current main.
	m.stackInfo[key] = stackEntry{status: statusWith(
		stack.Stack{Size: 2, BaseChainOK: true, NextAction: "resolve-conflicts",
			Counts: map[stack.State]int{stack.StateMerged: 1, stack.StateOpen: 1}},
		stack.Commit{State: stack.StateMerged},
		stack.Commit{State: stack.StateOpen, PR: &stack.PR{Number: 10, Mergeable: "CONFLICTING"}}), fetchedAt: time.Now()}
	if !has(m, "restack: repo/wt") {
		t.Error("restack row should appear for a conflicting cascade")
	}

	// Without a landed prefix, restack has nothing to drop and must not be
	// advertised as a recovery action.
	m.stackInfo[key] = stackEntry{status: statusWith(
		stack.Stack{Size: 1, BaseChainOK: true, NextAction: "resolve-conflicts",
			Counts: map[stack.State]int{stack.StateOpen: 1}},
		stack.Commit{State: stack.StateOpen, PR: &stack.PR{Number: 10, Mergeable: "CONFLICTING"}}), fetchedAt: time.Now()}
	if has(m, "restack: repo/wt") {
		t.Error("restack row should stay hidden for a conflict without a landed prefix")
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

func TestPaletteListsFreshOpenPRsAcrossWorktrees(t *testing.T) {
	m, key := stackModel()
	open := openPR(42, "passing")
	open.Subject = "ship palette inventory"
	open.PR.URL = "https://example.test/pr/42"
	draft := openPR(43, "pending")
	draft.Subject = "draft follow-up"
	draft.PR.URL = "https://example.test/pr/43"
	draft.PR.Draft = true
	closed := stack.Commit{State: stack.StateClosed, Subject: "closed",
		PR: &stack.PR{Number: 44, URL: "https://example.test/pr/44"}}
	m.stackInfo[key] = stackEntry{status: statusWith(
		stack.Stack{Size: 3, Counts: map[stack.State]int{stack.StateOpen: 2, stack.StateClosed: 1}},
		open, draft, closed), fetchedAt: time.Now()}

	var titles []string
	for _, c := range m.paletteCommands() {
		if strings.HasPrefix(c.title, "open PR #") {
			titles = append(titles, c.title)
		}
	}
	joined := strings.Join(titles, "\n")
	for _, want := range []string{
		"open PR #42: repo/wt — ship palette inventory",
		"open PR #43: repo/wt — draft follow-up",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("palette missing %q from:\n%s", want, joined)
		}
	}
	if strings.Contains(joined, "#44") {
		t.Error("closed PR should not appear in the palette")
	}

	e := m.stackInfo[key]
	e.fetchedAt = time.Now().Add(-2 * stackFreshFor)
	m.stackInfo[key] = e
	for _, c := range m.paletteCommands() {
		if strings.HasPrefix(c.title, "open PR #") {
			t.Fatal("stale stack data should not produce open-PR palette rows")
		}
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
	if !strings.Contains(out, "next: merge") {
		t.Errorf("overlay should show the rollup summary:\n%s", out)
	}
	if !strings.Contains(out, "#7") {
		t.Errorf("overlay should show the commit's PR ref:\n%s", out)
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

// TestStackScreenStructured renders the structured chain view from a loaded
// status and checks the subjects, a PR ref, the rollup, the cursor marker, and
// the action footer (restack hint present, submit absent for this rollup).
func TestStackScreenStructured(t *testing.T) {
	m, key := stackModel()
	st := statusWith(
		stack.Stack{Size: 3, BaseChainOK: true, NextAction: "restack",
			Counts: map[stack.State]int{stack.StateMerged: 1, stack.StateOpen: 2}},
		stack.Commit{Position: 1, State: stack.StateMerged, Subject: "tokens core",
			PR: &stack.PR{Number: 36}},
		stack.Commit{Position: 2, State: stack.StateOpen, Subject: "auth tokens",
			PR: &stack.PR{Number: 38, Checks: stack.Checks{Summary: "passing"}}},
		stack.Commit{Position: 3, State: stack.StateOpen, Subject: "refresh flow",
			PR: &stack.PR{Number: 41, Draft: true}})
	m = m.enterStackOverlay(key, "repo", "wt", stack.Params{})
	m.stackView.working = false
	m.stackView.status = &st

	out := m.renderStack(m.height - barHeight)
	for _, want := range []string{"tokens core", "auth tokens", "refresh flow", "#38", "next:", "▸", "move"} {
		if !strings.Contains(out, want) {
			t.Errorf("structured view missing %q:\n%s", want, out)
		}
	}
	// A restack rollup advertises the restack action but not submit (no work).
	if !strings.Contains(out, "restack") {
		t.Errorf("restack rollup should advertise the restack action:\n%s", out)
	}
	if strings.Contains(out, "submit") {
		t.Errorf("no submit-able work, so submit should not appear:\n%s", out)
	}
}

// stackNavModel builds a stack screen open on a structured status with both
// submit-able work and an open PR, backed by a cheap cat/sh workspace so the
// enter→activate path runs without touching git/gh. The active item's key
// matches the overlay so the open/diff actions resolve it.
func stackNavModel(t *testing.T) (Model, string) {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	ctrl := &Controller{cfg: config.Config{
		Editor: "cat", Agent: "cat", Shell: "sh",
		Keys: config.Default().Keys,
	}}
	mgr := session.NewManager()
	t.Cleanup(mgr.CloseAll)
	m := New(ctrl, mgr)
	mm, _ := m.Update(tea.WindowSizeMsg{Width: 72, Height: 24})
	m = mm.(Model)

	dir := t.TempDir()
	m.focus = focusActive
	m.active = []activeItem{{
		repo: repo.Repo{Name: "repo"},
		view: WorktreeView{
			WT:         repo.Worktree{Repo: "repo", Name: "wt", Branch: "wt", Path: dir},
			HasBase:    true,
			BaseBranch: "main",
			Ahead:      2,
		},
	}}
	m.activeCursor = 0

	key := wsKey("repo", "wt")
	st := statusWith(
		stack.Stack{Size: 2, BaseChainOK: true, NextAction: "restack",
			Counts: map[stack.State]int{stack.StateUnpushed: 1, stack.StateOpen: 1}},
		stack.Commit{Position: 1, State: stack.StateUnpushed, Subject: "core"},
		stack.Commit{Position: 2, State: stack.StateOpen, Subject: "flow",
			PR: &stack.PR{Number: 9, URL: "https://example.test/pr/9", Checks: stack.Checks{Summary: "passing"}}})
	m = m.enterStackOverlay(key, "repo", "wt", stack.Params{
		RepoName: "repo", WorktreeName: "wt", WorktreeDir: dir, Branch: "wt", MainBranch: "main"})
	m.stackView.working = false
	m.stackView.status = &st
	m.stackSubmit = func(stack.SubmitOptions) (stack.SubmitResult, error) { return stack.SubmitResult{}, nil }
	m.stackRestack = func(stack.RestackOptions) (stack.RestackResult, error) { return stack.RestackResult{}, nil }
	return m, key
}

// TestStackScreenNav walks the structured view's actions: j moves the cursor, s
// submits, R restacks (dry-run), v jumps to the diff, and enter opens the
// worktree — each with stubbed pipelines so nothing real runs.
func TestStackScreenNav(t *testing.T) {
	// j moves the cursor down.
	m, _ := stackNavModel(t)
	mm, _ := m.handleStack(tea.KeyPressMsg{Code: tea.KeyDown})
	if got := mm.(Model).stackView.cursor; got != 1 {
		t.Fatalf("down should move the cursor to 1, got %d", got)
	}

	// s submits (submit-able work present): working set, command issued.
	m, _ = stackNavModel(t)
	mm, cmd := m.handleStack(tea.KeyPressMsg{Code: 's', Text: "s"})
	m = mm.(Model)
	if !m.stackView.working || cmd == nil {
		t.Fatalf("s should submit: working=%v cmd=%v", m.stackView.working, cmd != nil)
	}

	// R restacks (dry-run): working set, command issued.
	m, _ = stackNavModel(t)
	mm, cmd = m.handleStack(tea.KeyPressMsg{Code: 'R', Text: "R"})
	m = mm.(Model)
	if !m.stackView.working || cmd == nil {
		t.Fatalf("R should restack: working=%v cmd=%v", m.stackView.working, cmd != nil)
	}

	// v jumps to the deck's diff viewer.
	m, _ = stackNavModel(t)
	mm, _ = m.handleStack(tea.KeyPressMsg{Code: 'v', Text: "v"})
	m = mm.(Model)
	if m.stackOpen || !m.diffOpen || m.screen != screenPicker {
		t.Fatalf("v should open the diff: stackOpen=%v diffOpen=%v screen=%v", m.stackOpen, m.diffOpen, m.screen)
	}

	// enter is inert on a plain status: every row is the same worktree, so there
	// is nothing per-row to open; the screen stays put.
	m, _ = stackNavModel(t)
	mm, cmd = m.handleStack(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = mm.(Model)
	if !m.stackOpen || cmd != nil {
		t.Fatalf("enter on a status should be a no-op: open=%v cmd=%v", m.stackOpen, cmd != nil)
	}

	// o on the PR-bearing commit issues the browser-open command; the command is
	// only asserted, never run, so no browser launches during the test.
	m, _ = stackNavModel(t)
	m.stackView.cursor = 1
	_, cmd = m.handleStack(tea.KeyPressMsg{Code: 'o', Text: "o"})
	if cmd == nil {
		t.Fatal("o on a commit with a PR should issue the open-PR command")
	}
}

// TestStackDeckOpensScreen checks the deck's `s` key opens the stack screen for a
// stackable worktree and kicks a fetch.
func TestStackDeckOpensScreen(t *testing.T) {
	m, key := stackModel()
	m.stackFetch = func(stack.Params) (stack.StackStatus, error) {
		return statusWith(stack.Stack{Size: 1, NextAction: "merge",
			Counts: map[stack.State]int{stack.StateOpen: 1}}, openPR(1, "passing")), nil
	}
	mm, cmd := m.handleActiveKey(tea.KeyPressMsg{Code: 's', Text: "s"})
	m = mm.(Model)
	if !m.stackOpen || m.stackView.key != key || cmd == nil {
		t.Fatalf("s should open the stack screen and fetch: open=%v key=%q cmd=%v",
			m.stackOpen, m.stackView.key, cmd != nil)
	}
}

func TestStackOverlayReuseConfirm(t *testing.T) {
	m, key := stackModel()
	m.stackInfo[key] = stackEntry{status: statusWith(
		stack.Stack{Size: 1, BaseChainOK: true, NextAction: "complete", Actions: []string{"archive", "reuse"}, Counts: map[stack.State]int{stack.StateMerged: 1}},
		stack.Commit{State: stack.StateMerged}), fetchedAt: time.Now()}
	var calls []bool
	m.stackReuse = func(o stack.ReuseOptions) (stack.ReuseResult, error) {
		calls = append(calls, o.DryRun)
		return stack.ReuseResult{Status: statusWith(stack.Stack{NextAction: "clean"}), DryRun: o.DryRun, RebaseCmd: []string{"git", "rebase"}}, nil
	}
	cmd := runPaletteRow(t, &m, "reuse worktree: repo/wt")
	msg := cmd().(stackRestackMsg)
	if !msg.reuse {
		t.Fatal("reuse palette action should use the guarded reuse pipeline")
	}
	m.applyStackRestack(msg)
	if !m.stackView.confirmReuse || !strings.Contains(m.renderStack(m.height-barHeight), "reuse plan") {
		t.Fatal("reuse dry-run should render and arm reuse confirmation")
	}
	mm, cmd := m.handleStack(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = mm.(Model)
	if cmd == nil {
		t.Fatal("confirming reuse should execute it")
	}
	m.applyStackRestack(cmd().(stackRestackMsg))
	if len(calls) != 2 || !calls[0] || calls[1] {
		t.Fatalf("reuse calls = %v, want dry-run then live", calls)
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
