package tui

import (
	"errors"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

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
			glyph: "↻",
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
	if glyph := glyphFor(c); !strings.Contains(glyph, "!") {
		t.Errorf("conflicting PR should use the error glyph, got %q", glyph)
	}
}

func TestStackCommitRowsUseOneSemanticGlyphAndWordedFacts(t *testing.T) {
	m, _ := stackModel()
	cases := []struct {
		name   string
		commit stack.Commit
		glyph  string
		fact   string
	}{
		{"passing", openPR(1, "passing"), stackGlyphReady, "checks passing"},
		{"pending", openPR(2, "pending"), stackGlyphPending, "checks pending"},
		{"failing", openPR(3, "failing"), stackGlyphAttention, "checks failing"},
		{"draft", stack.Commit{State: stack.StateOpen, PR: &stack.PR{Number: 4, Draft: true}}, stackGlyphInactive, "draft"},
		{"diverged", stack.Commit{State: stack.StateDiverged, PR: &stack.PR{Number: 5}}, stackGlyphRestack, "restack needed"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ansi.Strip(glyphFor(tc.commit)); got != tc.glyph {
				t.Fatalf("want glyph %q, got %q", tc.glyph, got)
			}
			plainFacts := factsPlain(tc.commit)
			if !strings.Contains(plainFacts, tc.fact) {
				t.Fatalf("want facts to contain %q, got %q", tc.fact, plainFacts)
			}
			for _, duplicate := range []string{stackGlyphReady, stackGlyphPending, stackGlyphAttention, stackGlyphRestack} {
				if strings.Contains(plainFacts, duplicate) {
					t.Fatalf("facts should use words, not duplicate status glyph %q: %q", duplicate, plainFacts)
				}
			}
			selected := ansi.Strip(m.stackCommitRow(tc.commit, true, 60))
			if !strings.Contains(selected, "▸") || !strings.Contains(selected, tc.fact) {
				t.Fatalf("selected row should retain cursor and state facts: %q", selected)
			}
		})
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
	// Single-commit stack reads the PR number, state, and check status.
	single := statusWith(
		stack.Stack{Size: 1, BaseChainOK: true, NextAction: "merge",
			Counts: map[stack.State]int{stack.StateOpen: 1}},
		stack.Commit{Position: 1, State: stack.StateOpen,
			PR: &stack.PR{Number: 42, Checks: stack.Checks{Summary: "passing"}}})
	if got, want := stackDetailSegment(single), "PR #42 open · checks passing"; got != want {
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
	mm, _ = m.handleStack(ctrlKey('n'))
	m = mm.(Model)
	if m.stackView.offset != 1 {
		t.Fatalf("ctrl+n should not scroll, got %d", m.stackView.offset)
	}
	mm, _ = m.handleStack(ctrlKey('p'))
	if got := mm.(Model).stackView.offset; got != 1 {
		t.Fatalf("ctrl+p should not scroll, got %d", got)
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
	m, _ = stackNavModel(t)
	mm, _ = m.handleStack(ctrlKey('n'))
	m = mm.(Model)
	if m.stackView.cursor != 0 {
		t.Fatalf("ctrl+n should not move the cursor, got %d", m.stackView.cursor)
	}
	mm, _ = m.handleStack(ctrlKey('p'))
	if got := mm.(Model).stackView.cursor; got != 0 {
		t.Fatalf("ctrl+p should not move the cursor, got %d", got)
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

// --- split mode (per-commit diff preview) ---

// splitCommitDiff is a small but structurally real patch body: one file header,
// one hunk, one deletion and one addition. Paired with splitCommitStat it feeds
// applyStackCommitDiff the same shape a git fetch would.
const splitCommitDiff = `diff --git a/core.go b/core.go
index 1111111..2222222 100644
--- a/core.go
+++ b/core.go
@@ -1,2 +1,2 @@
-old token line
+new token line
`

func splitCommitStat() []repo.FileStat {
	return []repo.FileStat{{Path: "core.go", Add: 1, Del: 1}}
}

// stackSplitModel opens the stack overlay on a two-commit status whose commits
// carry SHAs, which split mode keys its per-commit diff cache by. The params
// point at a directory that does not exist: the tests only ever assert that a
// fetch command was issued, never run it, so no git call is made.
func stackSplitModel() (Model, string) {
	m, key := stackModel()
	st := statusWith(
		stack.Stack{Size: 2, BaseChainOK: true, NextAction: "merge",
			Counts: map[stack.State]int{stack.StateOpen: 2}},
		stack.Commit{Position: 1, SHA: "aaa111", ShortSHA: "aaa111", State: stack.StateOpen,
			Subject: "core tokens",
			PR:      &stack.PR{Number: 11, URL: "https://example.test/pr/11", Checks: stack.Checks{Summary: "passing"}}},
		stack.Commit{Position: 2, SHA: "bbb222", ShortSHA: "bbb222", State: stack.StateOpen,
			Subject: "refresh flow",
			PR:      &stack.PR{Number: 12, URL: "https://example.test/pr/12", Checks: stack.Checks{Summary: "passing"}}})
	m = m.enterStackOverlay(key, "repo", "wt", stack.Params{
		RepoName: "repo", WorktreeName: "wt", WorktreeDir: "/repo/wt",
		Branch: "wt", MainBranch: "main"})
	m.stackView.working = false
	m.stackView.status = &st
	return m, key
}

// keyPress builds a plain printable key press (Text set, so String() returns it
// verbatim — matching how the handler switches on msg.String()).
func keyPress(r rune) tea.KeyPressMsg {
	return tea.KeyPressMsg{Code: r, Text: string(r)}
}

// enterSplit presses d and fails unless split mode opened with a fetch issued.
func enterSplit(t *testing.T, m Model) Model {
	t.Helper()
	mm, cmd := m.handleStack(keyPress('d'))
	m = mm.(Model)
	if !m.stackView.split || m.stackView.splitFocus != paneStack || cmd == nil {
		t.Fatalf("d should open split mode focused on the list with a fetch: split=%v focus=%v cmd=%v",
			m.stackView.split, m.stackView.splitFocus, cmd != nil)
	}
	return m
}

// TestStackSplitToggle covers the d round trip: it opens split mode, marks the
// cursored commit's diff loading and issues its fetch, and a second d closes the
// preview while leaving the overlay itself open.
func TestStackSplitToggle(t *testing.T) {
	m, _ := stackSplitModel()
	m = enterSplit(t, m)

	cd, ok := m.stackView.diffCache["aaa111"]
	if !ok || !cd.loading {
		t.Fatalf("d should mark the cursored commit's diff loading, cache = %+v", m.stackView.diffCache)
	}

	// A second d closes the preview but not the overlay, and issues no fetch.
	mm, cmd := m.handleStack(keyPress('d'))
	m = mm.(Model)
	if m.stackView.split || m.stackView.splitFocus != paneStack {
		t.Fatalf("a second d should leave split mode, got split=%v focus=%v",
			m.stackView.split, m.stackView.splitFocus)
	}
	if !m.stackOpen {
		t.Fatal("leaving split mode must keep the stack overlay open")
	}
	if cmd != nil {
		t.Fatal("closing the preview should not issue a command")
	}
	// Re-entering reuses the cache rather than re-fetching.
	mm, cmd = m.handleStack(keyPress('d'))
	if !mm.(Model).stackView.split || cmd != nil {
		t.Fatal("re-entering split should reuse the cached diff, not re-fetch")
	}
}

// TestStackSplitDiffMsgPopulatesCacheAndRenders checks that an arriving
// stackCommitDiffMsg becomes a rendered scope in the cache and that the split
// layout then shows both the left column's commit subjects and the patch text.
func TestStackSplitDiffMsgPopulatesCacheAndRenders(t *testing.T) {
	m, key := stackSplitModel()
	m = enterSplit(t, m)

	m.applyStackCommitDiff(stackCommitDiffMsg{
		key: key, sha: "aaa111", body: splitCommitDiff, stat: splitCommitStat()})

	cd, ok := m.stackView.diffCache["aaa111"]
	if !ok || cd.loading || cd.err != nil {
		t.Fatalf("diff msg should land as a loaded cache entry, got %+v (ok=%v)", cd, ok)
	}
	if cd.scope.files != 1 || cd.scope.add != 1 || cd.scope.del != 1 {
		t.Errorf("scope summary = files %d +%d −%d, want 1 file +1 −1",
			cd.scope.files, cd.scope.add, cd.scope.del)
	}
	if len(cd.scope.fileLines) != 1 {
		t.Errorf("the file header should be indexed for ]/[ jumps, got %v", cd.scope.fileLines)
	}

	out := ansi.Strip(m.renderStack(m.height - barHeight))
	for _, want := range []string{
		"core tokens",     // left column (and the section title)
		"refresh flow",    // the second commit still listed on the left
		"[split]",         // the mode label in the header
		"core.go",         // the diff's file index row
		"-old token line", // the patch body
		"+new token line",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("split render missing %q:\n%s", want, out)
		}
	}
}

// TestStackSplitLayout pins the two-column geometry: the commit list occupies a
// fixed-width left column, a │ divider sits at a stable offset on every body
// row, and the cursored commit is the one whose diff fills the right pane.
func TestStackSplitLayout(t *testing.T) {
	m, key := stackSplitModel()
	m = enterSplit(t, m)
	m.applyStackCommitDiff(stackCommitDiffMsg{
		key: key, sha: "aaa111", body: splitCommitDiff, stat: splitCommitStat()})

	lines := strings.Split(ansi.Strip(m.renderStack(m.height-barHeight)), "\n")
	if len(lines) < 4 {
		t.Fatalf("split render should have a header, rule, body and footer:\n%s", strings.Join(lines, "\n"))
	}

	// At width 72 the left column is 24 columns wide, so the divider lands at
	// display column 25 (leftW + one space) on every body row. The rows carry
	// multi-byte glyphs (▸, ○, │), so index by rune, not byte.
	const wantDividerCol = 25
	body := lines[2 : len(lines)-1]
	left := make([]string, len(body))
	right := make([]string, len(body))
	for i, row := range body {
		runes := []rune(row)
		col := -1
		for j, r := range runes {
			if r == '│' {
				col = j
				break
			}
		}
		if col != wantDividerCol {
			t.Fatalf("body row %d: divider at column %d, want %d:\n%q", i, col, wantDividerCol, row)
		}
		left[i], right[i] = string(runes[:col]), string(runes[col+1:])
	}

	// Row 0 is the cursored commit, drawn as the selection bar; row 1 is its
	// neighbour. Both live entirely inside the left column.
	if !strings.HasPrefix(left[0], "▸ core tokens") {
		t.Errorf("first left row should be the cursored commit: %q", left[0])
	}
	if !strings.Contains(left[1], "refresh flow") {
		t.Errorf("second left row should list the next commit: %q", left[1])
	}

	// The right pane carries the cursored commit's patch, not the neighbour's.
	joined := strings.Join(right, "\n")
	if !strings.Contains(joined, "core.go") || !strings.Contains(joined, "+new token line") {
		t.Errorf("right pane should hold the cursored commit's patch:\n%s", joined)
	}
}

// TestStackSplitStaleDiffDropped checks the key guard: a per-commit diff that
// lands for a different worktree (the overlay moved on) is discarded rather than
// poisoning the current cache.
func TestStackSplitStaleDiffDropped(t *testing.T) {
	m, _ := stackSplitModel()
	m = enterSplit(t, m)

	m.applyStackCommitDiff(stackCommitDiffMsg{
		key: wsKey("other", "tree"), sha: "aaa111", body: splitCommitDiff, stat: splitCommitStat()})

	if cd := m.stackView.diffCache["aaa111"]; !cd.loading || cd.scope.files != 0 {
		t.Fatalf("a diff for another worktree must be dropped, cache entry = %+v", cd)
	}

	// The same message under the right key does land, proving the guard is the
	// key and not something else rejecting it.
	m.applyStackCommitDiff(stackCommitDiffMsg{
		key: m.stackView.key, sha: "aaa111", body: splitCommitDiff, stat: splitCommitStat()})
	if cd := m.stackView.diffCache["aaa111"]; cd.loading || cd.scope.files != 1 {
		t.Fatalf("the matching-key diff should land, got %+v", cd)
	}
}

// TestStackSplitDiffError renders a failed per-commit fetch as an error line
// instead of a blank or stale pane.
func TestStackSplitDiffError(t *testing.T) {
	m, key := stackSplitModel()
	m = enterSplit(t, m)
	m.applyStackCommitDiff(stackCommitDiffMsg{key: key, sha: "aaa111", err: errors.New("bad object")})

	if cd := m.stackView.diffCache["aaa111"]; cd.err == nil || cd.loading {
		t.Fatalf("an errored fetch should cache the error, got %+v", cd)
	}
	if out := ansi.Strip(m.renderStack(m.height - barHeight)); !strings.Contains(out, "bad object") {
		t.Errorf("split render should surface the diff error:\n%s", out)
	}
}

// TestStackSplitFocusFlip checks tab moves focus between the two panes and that
// the footer legend follows it.
func TestStackSplitFocusFlip(t *testing.T) {
	m, _ := stackSplitModel()
	m = enterSplit(t, m)

	listOut := m.renderStack(m.height - barHeight)
	if !strings.Contains(ansi.Strip(listOut), "PR") {
		t.Errorf("the list pane's footer should offer the list-only actions:\n%s", ansi.Strip(listOut))
	}

	mm, cmd := m.handleStack(tea.KeyPressMsg{Code: tea.KeyTab})
	m = mm.(Model)
	if m.stackView.splitFocus != paneDiff || cmd != nil {
		t.Fatalf("tab should focus the diff pane, got focus=%v cmd=%v", m.stackView.splitFocus, cmd != nil)
	}
	diffOut := m.renderStack(m.height - barHeight)
	stripped := ansi.Strip(diffOut)
	if !strings.Contains(stripped, "scroll") || !strings.Contains(stripped, "n / p") {
		t.Errorf("the diff pane's footer should offer scroll and commit stepping:\n%s", stripped)
	}
	if strings.Contains(stripped, "commit\u00a0") || strings.Contains(stripped, "] / [ file") == false {
		t.Errorf("the diff pane's footer should carry its own hints:\n%s", stripped)
	}
	// The header's active-pane label is styled, so the raw frames must differ
	// even where the stripped text overlaps.
	if listOut == diffOut {
		t.Error("the active pane should be visibly highlighted in the header")
	}

	// shift+tab flips back.
	mm, _ = m.handleStack(tea.KeyPressMsg{Code: tea.KeyTab, Mod: tea.ModShift})
	if got := mm.(Model).stackView.splitFocus; got != paneStack {
		t.Fatalf("shift+tab should return focus to the list, got %v", got)
	}
}

// TestStackSplitCursorMovement covers both ways to walk commits: j with the list
// focused and n with the diff focused. Both advance the cursor and fetch the new
// commit's diff on a cache miss; n must not steal focus back to the list.
func TestStackSplitCursorMovement(t *testing.T) {
	// j from the list pane.
	m, _ := stackSplitModel()
	m = enterSplit(t, m)
	mm, cmd := m.handleStack(keyPress('j'))
	m = mm.(Model)
	if m.stackView.cursor != 1 || cmd == nil {
		t.Fatalf("j should advance the cursor and fetch: cursor=%d cmd=%v", m.stackView.cursor, cmd != nil)
	}
	if m.stackView.splitFocus != paneStack {
		t.Fatalf("j should keep the list focused, got %v", m.stackView.splitFocus)
	}
	if cd, ok := m.stackView.diffCache["bbb222"]; !ok || !cd.loading {
		t.Fatalf("j should mark the newly cursored commit loading, cache = %+v", m.stackView.diffCache)
	}
	// k walks back; that diff is already cached, so no second fetch.
	mm, cmd = m.handleStack(keyPress('k'))
	if got := mm.(Model).stackView.cursor; got != 0 || cmd != nil {
		t.Fatalf("k should step back without re-fetching: cursor=%d cmd=%v", got, cmd != nil)
	}

	// n from the diff pane.
	m, _ = stackSplitModel()
	m = enterSplit(t, m)
	mm, _ = m.handleStack(tea.KeyPressMsg{Code: tea.KeyTab})
	m = mm.(Model)
	mm, cmd = m.handleStack(keyPress('n'))
	m = mm.(Model)
	if m.stackView.cursor != 1 || cmd == nil {
		t.Fatalf("n should advance the cursor and fetch: cursor=%d cmd=%v", m.stackView.cursor, cmd != nil)
	}
	if m.stackView.splitFocus != paneDiff {
		t.Fatalf("n must stay in the diff pane, got %v", m.stackView.splitFocus)
	}
	mm, cmd = m.handleStack(keyPress('p'))
	m = mm.(Model)
	if m.stackView.cursor != 0 || cmd != nil {
		t.Fatalf("p should step back without re-fetching: cursor=%d cmd=%v", m.stackView.cursor, cmd != nil)
	}
	if m.stackView.splitFocus != paneDiff {
		t.Fatalf("p must stay in the diff pane, got %v", m.stackView.splitFocus)
	}
}

// TestStackSplitDiffScroll checks the diff pane scrolls its cached commit and
// that ]/[ jump to file headers, with both clamped to the content.
func TestStackSplitDiffScroll(t *testing.T) {
	m, key := stackSplitModel()
	m = enterSplit(t, m)

	// A patch long enough to overflow the pane.
	var b strings.Builder
	b.WriteString("diff --git a/one.go b/one.go\n@@ -1,1 +1,1 @@\n")
	for i := 0; i < 60; i++ {
		b.WriteString("+line\n")
	}
	b.WriteString("diff --git a/two.go b/two.go\n@@ -1,1 +1,1 @@\n+tail\n")
	m.applyStackCommitDiff(stackCommitDiffMsg{key: key, sha: "aaa111", body: b.String(),
		stat: []repo.FileStat{{Path: "one.go", Add: 60}, {Path: "two.go", Add: 1}}})
	mm, _ := m.handleStack(tea.KeyPressMsg{Code: tea.KeyTab})
	m = mm.(Model)

	mm, _ = m.handleStack(keyPress('j'))
	m = mm.(Model)
	if got := m.stackView.diffCache["aaa111"].offset; got != 1 {
		t.Fatalf("j should scroll the diff pane by one, got %d", got)
	}
	mm, _ = m.handleStack(keyPress('k'))
	m = mm.(Model)
	if got := m.stackView.diffCache["aaa111"].offset; got != 0 {
		t.Fatalf("k should scroll back to the top, got %d", got)
	}
	// k at the top clamps rather than going negative.
	mm, _ = m.handleStack(keyPress('k'))
	m = mm.(Model)
	if got := m.stackView.diffCache["aaa111"].offset; got != 0 {
		t.Fatalf("k at the top should clamp to 0, got %d", got)
	}

	// ] jumps to the second file header, [ returns to the first.
	mm, _ = m.handleStack(keyPress(']'))
	m = mm.(Model)
	jumped := m.stackView.diffCache["aaa111"].offset
	if jumped == 0 {
		t.Fatal("] should jump forward to the next file header")
	}
	mm, _ = m.handleStack(keyPress('['))
	m = mm.(Model)
	if got := m.stackView.diffCache["aaa111"].offset; got >= jumped {
		t.Fatalf("[ should jump back above %d, got %d", jumped, got)
	}

	// G/g run to the ends, both clamped.
	mm, _ = m.handleStack(keyPress('G'))
	m = mm.(Model)
	bottom := m.stackView.diffCache["aaa111"].offset
	if bottom == 0 {
		t.Fatal("G should scroll to the bottom of a long diff")
	}
	mm, _ = m.handleStack(keyPress('G'))
	if got := mm.(Model).stackView.diffCache["aaa111"].offset; got != bottom {
		t.Fatalf("G at the bottom should stay put, got %d want %d", got, bottom)
	}
	mm, _ = m.handleStack(keyPress('g'))
	if got := mm.(Model).stackView.diffCache["aaa111"].offset; got != 0 {
		t.Fatalf("g should return to the top, got %d", got)
	}
}

// TestStackSplitScrollWithoutCache is the no-panic guard: with the diff pane
// focused but nothing cached (or still loading), every scroll and file-jump key
// is an inert no-op and the pane renders its loading notice.
func TestStackSplitScrollWithoutCache(t *testing.T) {
	keys := []tea.KeyPressMsg{
		{Code: tea.KeyDown}, {Code: tea.KeyUp},
		keyPress('j'), keyPress('k'), keyPress('g'), keyPress('G'),
		keyPress(']'), keyPress('['), keyPress('J'), keyPress('K'),
		ctrlKey('d'), ctrlKey('u'),
	}

	for _, tc := range []struct {
		name  string
		cache map[string]stackCommitDiff
	}{
		{"missing entry", nil},
		{"still loading", map[string]stackCommitDiff{"aaa111": {loading: true}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, _ := stackSplitModel()
			m.stackView.split = true
			m.stackView.splitFocus = paneDiff
			m.stackView.diffCache = tc.cache

			for _, k := range keys {
				mm, _ := m.handleStack(k)
				m = mm.(Model)
				if cd := m.stackView.diffCache["aaa111"]; cd.offset != 0 {
					t.Fatalf("%s should leave the offset at 0, got %d after %q",
						tc.name, cd.offset, k.String())
				}
				if m.stackView.cursor != 0 {
					t.Fatalf("scroll keys must not move the commit cursor, got %d", m.stackView.cursor)
				}
			}
			if out := ansi.Strip(m.renderStack(m.height - barHeight)); !strings.Contains(out, "loading diff") {
				t.Errorf("%s should render the loading notice:\n%s", tc.name, out)
			}
		})
	}
}

// TestStackEscReturnsFromAnywhere checks esc means one thing everywhere on the
// stack screen: leave, and hand back whatever screen opened it. It no longer
// unwinds the preview first — d owns that — so the preview being open costs no
// extra keypress on the way out.
func TestStackEscReturnsFromAnywhere(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(Model) Model
	}{
		{"plain status", func(m Model) Model { return m }},
		{"preview open, list focused", func(m Model) Model { return enterSplit(t, m) }},
		{"preview open, diff focused", func(m Model) Model {
			m = enterSplit(t, m)
			mm, _ := m.handleStack(tea.KeyPressMsg{Code: tea.KeyTab})
			return mm.(Model)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, _ := stackSplitModel()
			// The screen the stack panel was opened over must be handed back
			// untouched — the overlay never owned it.
			m.screen = screenAgent
			m = tc.setup(m)

			mm, _ := m.handleStack(tea.KeyPressMsg{Code: tea.KeyEscape})
			m = mm.(Model)
			if m.stackOpen {
				t.Fatal("esc should leave the stack screen in one step")
			}
			if m.screen != screenAgent {
				t.Fatalf("esc should return to the originating screen, got %v", m.screen)
			}
		})
	}
}

// TestStackPreviewIsDToggle is the other half: d, not esc, opens and closes the
// preview, and closing it keeps you on the stack screen.
func TestStackPreviewIsDToggle(t *testing.T) {
	m, _ := stackSplitModel()
	m = enterSplit(t, m)

	mm, _ := m.handleStack(keyPress('d'))
	m = mm.(Model)
	if m.stackView.split {
		t.Fatal("d should close the preview")
	}
	if !m.stackOpen || m.stackView.status == nil {
		t.Fatal("closing the preview must keep the stack screen and its status")
	}
	// And it toggles back open from the diff pane too, not just the list. This
	// re-open reuses the cached patch, so unlike enterSplit it issues no fetch.
	mm, _ = m.handleStack(keyPress('d'))
	m = mm.(Model)
	if !m.stackView.split {
		t.Fatal("d should re-open the preview")
	}
	mm, _ = m.handleStack(tea.KeyPressMsg{Code: tea.KeyTab})
	m = mm.(Model)
	if m.stackView.splitFocus != paneDiff {
		t.Fatal("precondition: tab should focus the diff pane")
	}
	mm, _ = m.handleStack(keyPress('d'))
	if mm.(Model).stackView.split {
		t.Fatal("d should close the preview from the diff pane too")
	}
	if !mm.(Model).stackOpen {
		t.Fatal("closing the preview from the diff pane must keep the screen open")
	}
}

// TestStackSplitActionsStillReachTheStack checks split mode does not shadow the
// whole-stack actions: o still opens the cursored commit's PR from either pane.
func TestStackSplitActionsStillReachTheStack(t *testing.T) {
	// o from the list pane.
	m, _ := stackSplitModel()
	m = enterSplit(t, m)
	if _, cmd := m.handleStack(keyPress('o')); cmd == nil {
		t.Error("o in split mode should still issue the open-PR command")
	}
	// o from the diff pane too.
	mm, _ := m.handleStack(tea.KeyPressMsg{Code: tea.KeyTab})
	if _, cmd := mm.(Model).handleStack(keyPress('o')); cmd == nil {
		t.Error("o with the diff pane focused should still open the PR")
	}
}

// TestStackVIsGone pins the removal. v used to tear the stack screen down and
// teleport to the deck's whole-branch diff, losing the cursor on the way; the
// preview covers reading a diff now, and the uncommitted row covers the one
// thing the preview could not reach. v must be inert here — and in particular
// must not still be leaking through split mode's fall-through list.
func TestStackVIsGone(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(Model) Model
	}{
		{"plain status", func(m Model) Model { return m }},
		{"preview open", func(m Model) Model { return enterSplit(t, m) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, _ := stackSplitModel()
			m.screen = screenAgent
			m = tc.setup(m)
			wantSplit := m.stackView.split

			mm, cmd := m.handleStack(keyPress('v'))
			m = mm.(Model)
			if cmd != nil {
				t.Error("v should issue no command on the stack screen")
			}
			if !m.stackOpen {
				t.Error("v should not close the stack screen")
			}
			if m.diffOpen {
				t.Error("v should no longer hand off to the deck's diff viewer")
			}
			if m.screen != screenAgent {
				t.Errorf("v should not move the user to another screen, got %v", m.screen)
			}
			if m.stackView.split != wantSplit {
				t.Error("v should not disturb the preview")
			}
		})
	}
}

// TestStackFootersDropTheBranchDiff guards the hint alongside the binding, in
// both the plain list and the preview.
func TestStackFootersDropTheBranchDiff(t *testing.T) {
	m, _ := stackSplitModel()
	m.width, m.height = 160, 40

	status := ansi.Strip(m.renderStackStatus(*m.stackView.status, 0, m.height-barHeight))
	if strings.Contains(status, "v diff") {
		t.Errorf("the status footer should no longer offer v:\n%s", status)
	}

	m = enterSplit(t, m)
	for _, focus := range []splitPane{paneStack, paneDiff} {
		m.stackView.splitFocus = focus
		out := ansi.Strip(m.renderStackSplit(*m.stackView.status, 0, m.height-barHeight))
		if strings.Contains(out, "branch diff") {
			t.Errorf("focus %v: the preview footer should no longer offer v:\n%s", focus, out)
		}
	}
}

// TestStackSplitRefreshDropsCachedDiffs checks a fresh status invalidates the
// per-commit diffs, so a rewritten SHA can never show a stale patch.
func TestStackSplitRefreshDropsCachedDiffs(t *testing.T) {
	m, key := stackSplitModel()
	m = enterSplit(t, m)
	m.applyStackCommitDiff(stackCommitDiffMsg{
		key: key, sha: "aaa111", body: splitCommitDiff, stat: splitCommitStat()})
	if len(m.stackView.diffCache) == 0 {
		t.Fatal("precondition: the cache should hold the fetched diff")
	}

	m.applyStackStatus(stackStatusMsg{key: key, status: *m.stackView.status})
	if m.stackView.diffCache != nil {
		t.Fatalf("a fresh status should drop the per-commit diff cache, got %+v", m.stackView.diffCache)
	}
}

// TestStackSplitRefreshRekicksCursoredDiff is the other half of dropping the
// cache: because the drop leaves the open preview looking up a key nothing will
// fill, the message handler has to re-issue the cursored commit's fetch. Without
// it the pane sits on "loading diff…" until the user moves the cursor. This goes
// through Update rather than applyStackStatus directly, since that is where the
// re-kick lives.
func TestStackSplitRefreshRekicksCursoredDiff(t *testing.T) {
	m, key := stackSplitModel()
	m = enterSplit(t, m)
	m.applyStackCommitDiff(stackCommitDiffMsg{
		key: key, sha: "aaa111", body: splitCommitDiff, stat: splitCommitStat()})

	mm, cmd := m.Update(stackStatusMsg{key: key, status: *m.stackView.status})
	m = mm.(Model)

	if cd, ok := m.stackView.diffCache["aaa111"]; !ok || !cd.loading {
		t.Fatalf("a refresh under an open preview should re-mark the cursored diff loading, cache = %+v",
			m.stackView.diffCache)
	}
	if cmd == nil {
		t.Fatal("a refresh under an open preview should re-issue the cursored commit's fetch")
	}
	if dm, ok := cmd().(stackCommitDiffMsg); !ok || dm.sha != "aaa111" || dm.key != key {
		t.Fatalf("the re-kicked fetch should target the cursored commit, got %#v", cmd())
	}

	// With the preview closed there is nothing on screen to re-prime, so a
	// refresh must stay silent rather than fetch a patch nobody is looking at.
	m2, key2 := stackSplitModel()
	if _, c := m2.Update(stackStatusMsg{key: key2, status: *m2.stackView.status}); c != nil {
		t.Fatal("a refresh with the preview closed should not fetch a diff")
	}
}

// TestStackSplitRestackRekicksCursoredDiff covers the same recovery for the path
// that actually invalidates SHAs. A real restack rewrites the stack, so every
// cached patch is keyed to a commit that no longer exists; the cache is dropped
// and the preview re-primed against the new commits. The dry-run phase only
// shows a plan, so it must leave the cache alone.
func TestStackSplitRestackRekicksCursoredDiff(t *testing.T) {
	m, key := stackSplitModel()
	m = enterSplit(t, m)
	m.applyStackCommitDiff(stackCommitDiffMsg{
		key: key, sha: "aaa111", body: splitCommitDiff, stat: splitCommitStat()})

	// The dry run leaves the cached patch in place.
	mm, _ := m.Update(stackRestackMsg{
		key: key, res: stack.RestackResult{Status: *m.stackView.status}, dryRun: true})
	if _, ok := mm.(Model).stackView.diffCache["aaa111"]; !ok {
		t.Fatal("a restack dry run should not drop the per-commit diff cache")
	}

	// The real run rewrites SHAs: the stale patch goes and the new tip is fetched.
	after := statusWith(
		stack.Stack{Size: 1, BaseChainOK: true, NextAction: "submit",
			Counts: map[stack.State]int{stack.StateUnsubmitted: 1}},
		stack.Commit{Position: 1, SHA: "ccc333", ShortSHA: "ccc333",
			State: stack.StateUnsubmitted, Subject: "core tokens"})
	mm, cmd := m.Update(stackRestackMsg{key: key, res: stack.RestackResult{Status: after}})
	m = mm.(Model)

	if _, stale := m.stackView.diffCache["aaa111"]; stale {
		t.Fatalf("a real restack should drop patches keyed to rewritten SHAs, cache = %+v",
			m.stackView.diffCache)
	}
	if cmd == nil {
		t.Fatal("a real restack under an open preview should re-issue the fetch")
	}
	if dm, ok := cmd().(stackCommitDiffMsg); !ok || dm.sha != "ccc333" {
		t.Fatalf("the re-kicked fetch should target the post-restack commit, got %#v", cmd())
	}
}

// TestStackEnterStaysInertWithSplit is the regression guard for the key that was
// deliberately left alone: enter neither opens nor navigates the preview.
func TestStackEnterStaysInertWithSplit(t *testing.T) {
	// On a plain status row enter does nothing at all.
	m, _ := stackSplitModel()
	mm, cmd := m.handleStack(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = mm.(Model)
	if m.stackView.split || cmd != nil || !m.stackOpen {
		t.Fatalf("enter must not open split mode: split=%v cmd=%v open=%v",
			m.stackView.split, cmd != nil, m.stackOpen)
	}

	// Inside split mode enter does not move focus either — that is tab's job.
	m, _ = stackSplitModel()
	m = enterSplit(t, m)
	mm, cmd = m.handleStack(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = mm.(Model)
	if m.stackView.splitFocus != paneStack || !m.stackView.split || cmd != nil {
		t.Fatalf("enter must not change split focus: focus=%v split=%v cmd=%v",
			m.stackView.splitFocus, m.stackView.split, cmd != nil)
	}
}

// TestStackStatusFooterAdvertisesPreview checks the non-split footer tells the
// user the preview exists.
func TestStackStatusFooterAdvertisesPreview(t *testing.T) {
	m, _ := stackSplitModel()
	out := ansi.Strip(m.renderStack(m.height - barHeight))
	if !strings.Contains(out, "preview") {
		t.Errorf("the status footer should advertise the d preview:\n%s", out)
	}
}

// --- uncommitted row ---

// dirtyStackModel is stackSplitModel with the worktree carrying uncommitted
// work, which is what makes the synthetic trailing row appear.
func dirtyStackModel(add, del int) (Model, string) {
	m, key := stackSplitModel()
	m.active[0].view.Dirty = true
	m.active[0].view.Add, m.active[0].view.Del = add, del
	return m, key
}

// TestStackRowsAppendUncommitted pins where the row lives and when. Commits are
// bottom-first, so uncommitted work — which sits above the tip — is last.
func TestStackRowsAppendUncommitted(t *testing.T) {
	clean, _ := stackSplitModel()
	rows := clean.stackRows()
	if len(rows) != 2 {
		t.Fatalf("a clean worktree should show only its commits, got %d rows", len(rows))
	}
	for i, r := range rows {
		if r.uncommitted() {
			t.Fatalf("row %d should be a commit on a clean worktree", i)
		}
	}

	dirty, _ := dirtyStackModel(4, 2)
	rows = dirty.stackRows()
	if len(rows) != 3 {
		t.Fatalf("a dirty worktree should append one row, got %d", len(rows))
	}
	if !rows[2].uncommitted() {
		t.Fatal("the uncommitted row must come last — it sits above the tip")
	}
	if rows[2].diffKey() != uncommittedKey {
		t.Fatalf("uncommitted row key = %q, want the sentinel", rows[2].diffKey())
	}
	// The sentinel must not be mistakable for a SHA.
	for _, r := range rows[:2] {
		if r.diffKey() == uncommittedKey {
			t.Fatal("a commit key collided with the uncommitted sentinel")
		}
	}
}

// TestStackUncommittedRowIsReachableAndFetches walks the cursor onto the row and
// checks it drives the preview like any other.
func TestStackUncommittedRowIsReachableAndFetches(t *testing.T) {
	m, key := dirtyStackModel(4, 2)
	m = enterSplit(t, m)

	// G jumps to the last row, which is now the uncommitted one.
	mm, cmd := m.handleStack(keyPress('G'))
	m = mm.(Model)
	if m.stackView.cursor != 2 {
		t.Fatalf("G should land on the uncommitted row (index 2), got %d", m.stackView.cursor)
	}
	if cmd == nil {
		t.Fatal("landing on the uncommitted row should fetch its diff")
	}
	if cd, ok := m.stackView.diffCache[uncommittedKey]; !ok || !cd.loading {
		t.Fatalf("first landing should mark the row loading, cache = %+v", m.stackView.diffCache)
	}
	// The fetch must be the uncommitted one, keyed by the sentinel.
	msg, ok := cmd().(stackCommitDiffMsg)
	if !ok {
		t.Fatalf("expected a stackCommitDiffMsg, got %T", cmd())
	}
	if msg.sha != uncommittedKey || msg.key != key {
		t.Fatalf("fetch should target the uncommitted row, got sha=%q key=%q", msg.sha, msg.key)
	}
}

// TestStackUncommittedRowAlwaysRevalidates is the core of the caching split: a
// commit's patch is immutable and cached forever, but uncommitted work changes
// under the screen, so every landing must re-issue the fetch.
func TestStackUncommittedRowAlwaysRevalidates(t *testing.T) {
	m, key := dirtyStackModel(4, 2)
	m = enterSplit(t, m)

	// Populate both rows.
	m.applyStackCommitDiff(stackCommitDiffMsg{
		key: key, sha: "aaa111", body: splitCommitDiff, stat: splitCommitStat()})
	m.applyStackCommitDiff(stackCommitDiffMsg{
		key: key, sha: uncommittedKey, body: splitCommitDiff, stat: splitCommitStat()})

	// Re-landing on a cached commit issues nothing.
	m.stackView.cursor = 1
	mm, cmd := m.handleStack(keyPress('k')) // onto commit 0, already cached
	m = mm.(Model)
	if cmd != nil {
		t.Fatal("a cached commit patch should not re-fetch — commits are immutable")
	}

	// Re-landing on the uncommitted row always does.
	m.stackView.cursor = 1
	mm, cmd = m.handleStack(keyPress('j'))
	m = mm.(Model)
	if m.stackView.cursor != 2 {
		t.Fatalf("expected to land on the uncommitted row, got cursor %d", m.stackView.cursor)
	}
	if cmd == nil {
		t.Fatal("the uncommitted row must revalidate on every landing")
	}
	// And it must not flash: the already-fetched patch stays on screen.
	if cd := m.stackView.diffCache[uncommittedKey]; cd.loading || cd.scope.files == 0 {
		t.Fatalf("revalidation should keep the previous patch visible, got %+v", cd)
	}
}

// TestStackUncommittedScrollSurvivesRevalidation guards the thing that would
// make the row unusable while an agent writes to the worktree: re-fetching must
// not snap the reader back to the top.
func TestStackUncommittedScrollSurvivesRevalidation(t *testing.T) {
	m, key := dirtyStackModel(4, 2)
	m = enterSplit(t, m)
	long := strings.Repeat("+a line\n", 200)
	m.applyStackCommitDiff(stackCommitDiffMsg{
		key: key, sha: uncommittedKey, body: long, stat: splitCommitStat()})

	e := m.stackView.diffCache[uncommittedKey]
	e.offset = 42
	m.stackView.diffCache[uncommittedKey] = e

	// A fresh fetch of the same row lands.
	m.applyStackCommitDiff(stackCommitDiffMsg{
		key: key, sha: uncommittedKey, body: long, stat: splitCommitStat()})
	if got := m.stackView.diffCache[uncommittedKey].offset; got != 42 {
		t.Fatalf("scroll offset should survive revalidation, got %d want 42", got)
	}

	// But it must clamp when the new content is shorter than the old offset.
	m.applyStackCommitDiff(stackCommitDiffMsg{
		key: key, sha: uncommittedKey, body: "+one\n", stat: splitCommitStat()})
	cd := m.stackView.diffCache[uncommittedKey]
	if cd.offset >= len(cd.scope.lines) {
		t.Fatalf("offset %d should be clamped into %d lines", cd.offset, len(cd.scope.lines))
	}
}

// TestStackUncommittedRowHasNoPR checks o degrades to a flash rather than
// opening something wrong or panicking on the nil commit.
func TestStackUncommittedRowHasNoPR(t *testing.T) {
	m, _ := dirtyStackModel(4, 2)
	m.stackView.cursor = 2
	mm, cmd := m.handleStack(keyPress('o'))
	if cmd == nil {
		t.Fatal("o on the uncommitted row should flash, not no-op")
	}
	_ = mm
	// The commit rows still open their PR.
	m.stackView.cursor = 0
	if _, cmd := m.handleStack(keyPress('o')); cmd == nil {
		t.Fatal("o on a commit with a PR should still open it")
	}
}

// TestStackCursorSurvivesRefreshOnUncommittedRow is the regression guard for
// clamping against commits instead of rows: a passive refresh while the cursor
// sits on the uncommitted row must leave it there.
func TestStackCursorSurvivesRefreshOnUncommittedRow(t *testing.T) {
	m, key := dirtyStackModel(4, 2)
	m.stackView.cursor = 2
	m.applyStackStatus(stackStatusMsg{key: key, status: *m.stackView.status})
	if m.stackView.cursor != 2 {
		t.Fatalf("a refresh should leave the cursor on the uncommitted row, got %d", m.stackView.cursor)
	}

	// When the tree goes clean the row disappears and the cursor must come back
	// into range rather than dangle past the end.
	m.active[0].view.Dirty = false
	m.applyStackStatus(stackStatusMsg{key: key, status: *m.stackView.status})
	if m.stackView.cursor != 1 {
		t.Fatalf("losing the row should clamp the cursor to the last commit, got %d", m.stackView.cursor)
	}
}

// TestStackUncommittedRendersInListAndPane checks both surfaces actually show
// it: the list row with its diffstat, and the pane with the patch under an
// "uncommitted" section rule.
func TestStackUncommittedRendersInListAndPane(t *testing.T) {
	m, key := dirtyStackModel(47, 12)
	m.width, m.height = 160, 40

	// Full-width list.
	plain := ansi.Strip(m.renderStackStatus(*m.stackView.status, 2, m.height-barHeight))
	if !strings.Contains(plain, "uncommitted") {
		t.Fatalf("the stack list should show the uncommitted row:\n%s", plain)
	}
	if !strings.Contains(plain, "+47") || !strings.Contains(plain, "−12") {
		t.Fatalf("the row should carry the diffstat:\n%s", plain)
	}

	// Split pane, cursored onto the row with a patch cached.
	m = enterSplit(t, m)
	m.stackView.cursor = 2
	m.applyStackCommitDiff(stackCommitDiffMsg{
		key: key, sha: uncommittedKey, body: splitCommitDiff,
		stat: splitCommitStat(), untracked: []string{"brand-new.go"}})
	split := ansi.Strip(m.renderStackSplit(*m.stackView.status, 2, m.height-barHeight))
	for _, want := range []string{"uncommitted", "new token line", "brand-new.go"} {
		if !strings.Contains(split, want) {
			t.Errorf("split pane missing %q:\n%s", want, split)
		}
	}
}

// TestStackUncommittedUntrackedCapReported checks the surplus is surfaced rather
// than silently dropped.
func TestStackUncommittedUntrackedCapReported(t *testing.T) {
	m, key := dirtyStackModel(1, 0)
	m.width, m.height = 160, 40
	m = enterSplit(t, m)
	m.stackView.cursor = 2
	m.applyStackCommitDiff(stackCommitDiffMsg{
		key: key, sha: uncommittedKey, body: splitCommitDiff,
		stat: splitCommitStat(), untracked: []string{"a.go"}, untrackedExtra: 7})

	pane := ansi.Strip(m.renderStackSplit(*m.stackView.status, 2, m.height-barHeight))
	if !strings.Contains(pane, "7 more untracked") {
		t.Errorf("the capped surplus should be named in the section rule:\n%s", pane)
	}
}

// TestStackUncommittedEmptyReadsSensibly covers the row emptying out under the
// cursor — the work gets committed while the pane is open.
func TestStackUncommittedEmptyReadsSensibly(t *testing.T) {
	m, key := dirtyStackModel(0, 0)
	m.width, m.height = 160, 40
	m = enterSplit(t, m)
	m.stackView.cursor = 2
	m.applyStackCommitDiff(stackCommitDiffMsg{key: key, sha: uncommittedKey})

	pane := ansi.Strip(m.renderStackSplit(*m.stackView.status, 2, m.height-barHeight))
	if !strings.Contains(pane, "nothing uncommitted") {
		t.Errorf("an empty uncommitted row should say so, not talk about commits:\n%s", pane)
	}
	if strings.Contains(pane, "no changes in this commit") {
		t.Errorf("the commit-specific empty message leaked onto the uncommitted row:\n%s", pane)
	}
}

// TestStackFooterSeparatesPreviewFromLeaving pins the footers against the two
// keys blurring back together: d is advertised for the preview, esc for
// leaving, and esc never borrows a pane name from tab's hints.
func TestStackFooterSeparatesPreviewFromLeaving(t *testing.T) {
	m, _ := stackSplitModel()
	m.width, m.height = 160, 40

	// Plain status: d opens the preview, esc returns.
	status := ansi.Strip(m.renderStackStatus(*m.stackView.status, 0, m.height-barHeight))
	if !strings.Contains(status, "esc return") {
		t.Errorf("the status footer should say esc returns:\n%s", status)
	}
	if strings.Contains(status, "esc deck") {
		t.Errorf("esc no longer always lands on the deck, so it must not claim to:\n%s", status)
	}

	// Preview open, both focuses.
	m = enterSplit(t, m)
	for _, focus := range []splitPane{paneStack, paneDiff} {
		m.stackView.splitFocus = focus
		out := ansi.Strip(m.renderStackSplit(*m.stackView.status, 0, m.height-barHeight))
		footer := out[strings.LastIndex(out, "\n")+1:]

		if !strings.Contains(footer, "d") || !strings.Contains(footer, "close preview") {
			t.Errorf("focus %v: d should own closing the preview:\n%s", focus, footer)
		}
		if !strings.Contains(footer, "esc return") {
			t.Errorf("focus %v: esc should be named as the way out:\n%s", focus, footer)
		}
		if strings.Contains(footer, "esc list") || strings.Contains(footer, "esc close preview") {
			t.Errorf("focus %v: esc must not be labelled as a preview or pane action:\n%s", focus, footer)
		}
	}
}
