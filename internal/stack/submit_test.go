package stack

import (
	"reflect"
	"strings"
	"testing"
)

// statusFrom reconciles synthetic gather data into a StackStatus, so plan tests
// exercise planSubmit against exactly the Commit views the real engine produces.
func statusFrom(wt, main string, commits []LocalCommit, remotes map[string]string, prs []prRecord) StackStatus {
	stk, cs := reconcile(wt, main, commits, remotes, prs)
	return StackStatus{
		Schema: 1, Repo: "demo", Worktree: wt, Branch: "feat", MainBranch: main,
		Stack: stk, Commits: cs,
	}
}

// prFull is a prRecord builder that also sets Title and Body (which planSubmit
// needs but the reconcile-only builder omits).
func prFull(num int, state, head, base, title, body string) prRecord {
	return prRecord{
		Number: num, State: state, Head: head, Base: base,
		Title: title, Body: body, Review: "APPROVED",
		Checks: Checks{Summary: "passing", Failing: []string{}},
	}
}

func TestPlanSubmitGuards(t *testing.T) {
	const wt, main = "wt", "main"
	br := func(id string) string { return "ct/" + wt + "/" + id }

	cases := []struct {
		name    string
		commits []LocalCommit
		remotes map[string]string
		prs     []prRecord
		wantErr string
	}{
		{
			name: "duplicate id refused",
			commits: []LocalCommit{
				commit("aaaaaaa1111", "aaaaaaaa", "one"),
				commit("bbbbbbb2222", "aaaaaaaa", "two"),
			},
			wantErr: "duplicate or malformed",
		},
		{
			name:    "closed PR refused",
			commits: []LocalCommit{commit("aaaaaaa1111", "aaaaaaaa", "one")},
			remotes: map[string]string{"aaaaaaaa": "aaaaaaa1111"},
			prs:     []prRecord{prFull(5, "CLOSED", br("aaaaaaaa"), main, "one", "")},
			wantErr: "closed without merging",
		},
		{
			name:    "merged commit still local refused",
			commits: []LocalCommit{commit("aaaaaaa1111", "aaaaaaaa", "one")},
			remotes: map[string]string{"aaaaaaaa": "aaaaaaa1111"},
			prs:     []prRecord{prFull(6, "MERGED", br("aaaaaaaa"), main, "one", "")},
			wantErr: "restack first",
		},
		{
			name:    "orphan PR refused",
			commits: []LocalCommit{commit("aaaaaaa1111", "aaaaaaaa", "one")},
			remotes: map[string]string{"aaaaaaaa": "aaaaaaa1111"},
			prs: []prRecord{
				prFull(1, "OPEN", br("aaaaaaaa"), main, "one", ""),
				prFull(2, "OPEN", br("deadbeef"), main, "gone", ""),
			},
			wantErr: "orphan PR",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := statusFrom(wt, main, tc.commits, tc.remotes, tc.prs)
			_, err := planSubmit(st, tc.prs)
			if err == nil {
				t.Fatalf("expected a guard error containing %q, got nil", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error %q does not contain %q", err.Error(), tc.wantErr)
			}
		})
	}
}

func TestPlanSubmitFreshStack(t *testing.T) {
	const wt, main = "wt", "main"
	commits := []LocalCommit{
		commit("aaaaaaa1111", "", "first"),
		commit("bbbbbbb2222", "", "second"),
	}
	st := statusFrom(wt, main, commits, nil, nil)
	plan, err := planSubmit(st, nil)
	if err != nil {
		t.Fatal(err)
	}

	if len(plan.Assigns) != 2 {
		t.Errorf("assigns = %d, want 2", len(plan.Assigns))
	}
	if len(plan.Pushes) != 2 {
		t.Fatalf("pushes = %d, want 2", len(plan.Pushes))
	}
	for _, p := range plan.Pushes {
		if !p.Create {
			t.Errorf("fresh stack push should be a create, got %+v", p)
		}
	}
	if len(plan.Creates) != 2 {
		t.Fatalf("creates = %d, want 2", len(plan.Creates))
	}
	if plan.Creates[0].Base != main {
		t.Errorf("bottom PR base = %q, want %q", plan.Creates[0].Base, main)
	}
	if plan.Creates[1].Base != "ct/wt/(new)" {
		t.Errorf("second PR base = %q, want the previous (new) branch", plan.Creates[1].Base)
	}
	if len(plan.Retargets)+len(plan.Retitles)+len(plan.Bodies) != 0 {
		t.Errorf("fresh stack should have no retarget/retitle/body actions: %+v", plan)
	}
}

func TestPlanSubmitRewriteForcesUpperPush(t *testing.T) {
	const wt, main = "wt", "main"
	// Bottom commit has no id (will be rewritten); the top commit is a healthy
	// open PR that the rewrite will move, so it must be force-pushed.
	commits := []LocalCommit{
		commit("aaaaaaa1111", "", "bottom new"),
		commit("bbbbbbb2222", "bbbbbbbb", "top"),
	}
	remotes := map[string]string{"bbbbbbbb": "bbbbbbb2222"}
	prs := []prRecord{prFull(9, "OPEN", "ct/wt/bbbbbbbb", main, "top", "")}
	st := statusFrom(wt, main, commits, remotes, prs)

	plan, err := planSubmit(st, prs)
	if err != nil {
		t.Fatal(err)
	}
	var forced *PushAction
	for i := range plan.Pushes {
		if plan.Pushes[i].Branch == "ct/wt/bbbbbbbb" {
			forced = &plan.Pushes[i]
		}
	}
	if forced == nil {
		t.Fatalf("expected a push for ct/wt/bbbbbbbb, pushes=%+v", plan.Pushes)
	}
	if forced.Create {
		t.Errorf("rewritten open branch should force-update, not create: %+v", forced)
	}
}

func TestPlanSubmitRetargetRetitleBody(t *testing.T) {
	const wt, main = "wt", "main"
	br := func(id string) string { return "ct/" + wt + "/" + id }
	commits := []LocalCommit{
		commit("aaaaaaa1111", "aaaaaaaa", "one"),
		commit("bbbbbbb2222", "bbbbbbbb", "new subject"),
	}
	remotes := map[string]string{"aaaaaaaa": "aaaaaaa1111", "bbbbbbbb": "bbbbbbb2222"}
	// PR1 correct. PR2 bases on main (wrong: should be ct/wt/aaaaaaaa), has a
	// stale title, and an empty body (missing nav).
	region1 := renderNavRegion(navNums(1, 2), 0)
	prs := []prRecord{
		prFull(1, "OPEN", br("aaaaaaaa"), main, "one", region1),
		prFull(2, "OPEN", br("bbbbbbbb"), main, "old subject", ""),
	}
	st := statusFrom(wt, main, commits, remotes, prs)

	plan, err := planSubmit(st, prs)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Retargets) != 1 || plan.Retargets[0].Number != 2 ||
		plan.Retargets[0].NewBase != br("aaaaaaaa") || plan.Retargets[0].OldBase != main {
		t.Errorf("retargets = %+v", plan.Retargets)
	}
	if len(plan.Retitles) != 1 || plan.Retitles[0].Number != 2 ||
		plan.Retitles[0].NewTitle != "new subject" || plan.Retitles[0].OldTitle != "old subject" {
		t.Errorf("retitles = %+v", plan.Retitles)
	}
	// PR2 body is empty -> needs a nav splice. PR1 body already carries the
	// correct region -> must NOT be flagged.
	if len(plan.Bodies) != 1 || plan.Bodies[0].Number != 2 {
		t.Errorf("bodies = %+v, want a single update for #2", plan.Bodies)
	}
}

// TestPlanSubmitIdempotentNoop is the key convergence property: a stack whose
// remote already matches (all open, in sync, correct bases/titles, nav already
// spliced) produces an empty plan.
func TestPlanSubmitIdempotentNoop(t *testing.T) {
	const wt, main = "wt", "main"
	br := func(id string) string { return "ct/" + wt + "/" + id }
	commits := []LocalCommit{
		commit("aaaaaaa1111", "aaaaaaaa", "one"),
		commit("bbbbbbb2222", "bbbbbbbb", "two"),
	}
	remotes := map[string]string{"aaaaaaaa": "aaaaaaa1111", "bbbbbbbb": "bbbbbbb2222"}
	body0 := spliceNav("", renderNavRegion(navNums(1, 2), 0))
	body1 := spliceNav("", renderNavRegion(navNums(1, 2), 1))
	prs := []prRecord{
		prFull(1, "OPEN", br("aaaaaaaa"), main, "one", body0),
		prFull(2, "OPEN", br("bbbbbbbb"), br("aaaaaaaa"), "two", body1),
	}
	st := statusFrom(wt, main, commits, remotes, prs)

	plan, err := planSubmit(st, prs)
	if err != nil {
		t.Fatal(err)
	}
	if !plan.IsEmpty() {
		t.Errorf("expected an empty plan for a converged stack, got %+v", plan)
	}
}

func TestNewStackIDFormatAndUniqueness(t *testing.T) {
	existing := map[string]bool{}
	for i := 0; i < 500; i++ {
		id, err := newStackID(existing)
		if err != nil {
			t.Fatal(err)
		}
		if !validStackID(id) {
			t.Fatalf("generated id %q is not 8 lowercase hex", id)
		}
		if existing[id] {
			t.Fatalf("generated a duplicate id %q", id)
		}
		existing[id] = true
	}
	// A pre-seeded id must never be returned.
	seed := map[string]bool{}
	for id := range existing {
		seed[id] = true
	}
	id, err := newStackID(seed)
	if err != nil {
		t.Fatal(err)
	}
	if seed[id] {
		t.Errorf("newStackID returned an id already in the existing set: %q", id)
	}
}

func TestComputePushes(t *testing.T) {
	commits := []LocalCommit{
		{SHA: "sha_a_new", ShortSHA: "sha_a_n", StackID: "aaaaaaaa"}, // remote missing -> create
		{SHA: "sha_b_new", ShortSHA: "sha_b_n", StackID: "bbbbbbbb"}, // remote differs -> force
		{SHA: "sha_c_new", ShortSHA: "sha_c_n", StackID: "cccccccc"}, // remote matches -> skip
		{SHA: "sha_d_new", ShortSHA: "sha_d_n", StackID: ""},         // no id -> skip
	}
	remotes := map[string]string{"bbbbbbbb": "sha_b_old", "cccccccc": "sha_c_new"}
	got := computePushes(commits, remotes, "wt")
	want := []pushCmd{
		{Branch: "ct/wt/aaaaaaaa", SHA: "sha_a_new", Create: true},
		{Branch: "ct/wt/bbbbbbbb", SHA: "sha_b_new", Expected: "sha_b_old"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("computePushes = %+v, want %+v", got, want)
	}
}

func TestPushArgs(t *testing.T) {
	create := pushArgs("ct/wt/aaaaaaaa", "deadbeef", "", true)
	wantCreate := []string{
		"push", "--force-with-lease=refs/heads/ct/wt/aaaaaaaa:",
		"origin", "deadbeef:refs/heads/ct/wt/aaaaaaaa",
	}
	if !reflect.DeepEqual(create, wantCreate) {
		t.Errorf("create pushArgs = %v, want %v", create, wantCreate)
	}
	force := pushArgs("ct/wt/aaaaaaaa", "newsha", "oldsha", false)
	wantForce := []string{
		"push", "--force-with-lease=refs/heads/ct/wt/aaaaaaaa:oldsha",
		"origin", "newsha:refs/heads/ct/wt/aaaaaaaa",
	}
	if !reflect.DeepEqual(force, wantForce) {
		t.Errorf("force pushArgs = %v, want %v", force, wantForce)
	}
}

// TestGHMutatingArgs asserts the exact argv the mutating gh wrappers would run —
// the contract's stand-in for actually invoking gh (which tests never do).
func TestGHMutatingArgs(t *testing.T) {
	if got := ghCreateArgs("ct/wt/aaaaaaaa", "main", "My Title", "the body", false); !reflect.DeepEqual(got,
		[]string{"pr", "create", "--head", "ct/wt/aaaaaaaa", "--base", "main", "--title", "My Title", "--body", "the body"}) {
		t.Errorf("ghCreateArgs = %v", got)
	}
	if got := ghCreateArgs("h", "b", "t", "body", true); got[len(got)-1] != "--draft" {
		t.Errorf("draft create should end with --draft, got %v", got)
	}
	if got := ghEditBaseArgs(42, "ct/wt/aaaaaaaa"); !reflect.DeepEqual(got,
		[]string{"pr", "edit", "42", "--base", "ct/wt/aaaaaaaa"}) {
		t.Errorf("ghEditBaseArgs = %v", got)
	}
	if got := ghEditTitleArgs(42, "New Title"); !reflect.DeepEqual(got,
		[]string{"pr", "edit", "42", "--title", "New Title"}) {
		t.Errorf("ghEditTitleArgs = %v", got)
	}
	if got := ghEditBodyArgs(42, "New Body"); !reflect.DeepEqual(got,
		[]string{"pr", "edit", "42", "--body", "New Body"}) {
		t.Errorf("ghEditBodyArgs = %v", got)
	}
}
