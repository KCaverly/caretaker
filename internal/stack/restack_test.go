package stack

import (
	"reflect"
	"strings"
	"testing"
)

// TestRebaseAndDeleteArgs pins the exact argv for the two git mutations restack
// runs directly, the same arg-slice-assertion contract the gh wrappers use.
func TestRebaseAndDeleteArgs(t *testing.T) {
	if got := rebaseOntoArgs("origin/main", "deadbeef", "feat"); !reflect.DeepEqual(got,
		[]string{"rebase", "--onto", "origin/main", "deadbeef", "feat"}) {
		t.Errorf("rebaseOntoArgs = %v", got)
	}
	if got := pushDeleteArgs("ct/wt/aaaaaaaa"); !reflect.DeepEqual(got,
		[]string{"push", "origin", "--delete", "ct/wt/aaaaaaaa"}) {
		t.Errorf("pushDeleteArgs = %v", got)
	}
}

func TestPlanRestackGuards(t *testing.T) {
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
			prs:     []prRecord{pr(5, "CLOSED", br("aaaaaaaa"), main, "", "none")},
			wantErr: "closed without merging",
		},
		{
			name:    "orphan PR refused",
			commits: []LocalCommit{commit("aaaaaaa1111", "aaaaaaaa", "one")},
			remotes: map[string]string{"aaaaaaaa": "aaaaaaa1111"},
			prs: []prRecord{
				pr(1, "MERGED", br("aaaaaaaa"), main, "APPROVED", "passing"),
				pr(2, "OPEN", br("deadbeef"), main, "APPROVED", "passing"),
			},
			wantErr: "orphan PR",
		},
		{
			name: "non-contiguous merged prefix refused",
			commits: []LocalCommit{
				commit("aaaaaaa1111", "aaaaaaaa", "one"),
				commit("bbbbbbb2222", "bbbbbbbb", "two"),
				commit("ccccccc3333", "cccccccc", "three"),
			},
			remotes: map[string]string{"aaaaaaaa": "aaaaaaa1111", "bbbbbbbb": "bbbbbbb2222", "cccccccc": "ccccccc3333"},
			prs: []prRecord{
				pr(1, "MERGED", br("aaaaaaaa"), main, "APPROVED", "passing"),
				pr(2, "OPEN", br("bbbbbbbb"), main, "APPROVED", "passing"),
				pr(3, "MERGED", br("cccccccc"), br("bbbbbbbb"), "APPROVED", "passing"),
			},
			wantErr: "out-of-order land",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := statusFrom(wt, main, tc.commits, tc.remotes, tc.prs)
			_, _, err := planRestack(st)
			if err == nil {
				t.Fatalf("expected a guard error containing %q, got nil", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error %q does not contain %q", err.Error(), tc.wantErr)
			}
		})
	}
}

func TestPlanRestackNothing(t *testing.T) {
	const wt, main = "wt", "main"
	br := func(id string) string { return "ct/" + wt + "/" + id }
	// A healthy all-open stack has nothing landed to drop.
	commits := []LocalCommit{
		commit("aaaaaaa1111", "aaaaaaaa", "one"),
		commit("bbbbbbb2222", "bbbbbbbb", "two"),
	}
	remotes := map[string]string{"aaaaaaaa": "aaaaaaa1111", "bbbbbbbb": "bbbbbbb2222"}
	prs := []prRecord{
		pr(1, "OPEN", br("aaaaaaaa"), main, "APPROVED", "passing"),
		pr(2, "OPEN", br("bbbbbbbb"), br("aaaaaaaa"), "APPROVED", "passing"),
	}
	st := statusFrom(wt, main, commits, remotes, prs)
	plan, nothing, err := planRestack(st)
	if err != nil {
		t.Fatal(err)
	}
	if !nothing {
		t.Errorf("expected nothing to restack, got plan %+v", plan)
	}
}

func TestPlanRestackDropsLandedPrefix(t *testing.T) {
	const wt, main = "wt", "main"
	br := func(id string) string { return "ct/" + wt + "/" + id }
	commits := []LocalCommit{
		commit("aaaaaaa1111", "aaaaaaaa", "one"),
		commit("bbbbbbb2222", "bbbbbbbb", "two"),
		commit("ccccccc3333", "cccccccc", "three"),
	}
	remotes := map[string]string{"aaaaaaaa": "aaaaaaa1111", "bbbbbbbb": "bbbbbbb2222", "cccccccc": "ccccccc3333"}
	// A and B landed (contiguous bottom prefix); C is still open.
	prs := []prRecord{
		pr(1, "MERGED", br("aaaaaaaa"), main, "APPROVED", "passing"),
		pr(2, "MERGED", br("bbbbbbbb"), br("aaaaaaaa"), "APPROVED", "passing"),
		pr(3, "OPEN", br("cccccccc"), main, "APPROVED", "passing"),
	}
	st := statusFrom(wt, main, commits, remotes, prs)
	plan, nothing, err := planRestack(st)
	if err != nil {
		t.Fatal(err)
	}
	if nothing {
		t.Fatal("expected a landed prefix to drop, got nothing")
	}
	if plan.LastMergedSHA != "bbbbbbb2222" {
		t.Errorf("LastMergedSHA = %s, want B's sha (top of the landed prefix)", plan.LastMergedSHA)
	}
	if !reflect.DeepEqual(plan.MergedIDs, []string{"aaaaaaaa", "bbbbbbbb"}) {
		t.Errorf("MergedIDs = %v, want [aaaaaaaa bbbbbbbb]", plan.MergedIDs)
	}
	if len(plan.Drops) != 2 || plan.Drops[0].Number != 1 || plan.Drops[1].Number != 2 {
		t.Errorf("Drops = %+v, want #1 then #2", plan.Drops)
	}
}

// TestRenderRestackPlan checks the human dry-run output names the dropped commit,
// the exact rebase command, the branch deletion, and the post-rebase submit steps.
func TestRenderRestackPlan(t *testing.T) {
	res := RestackResult{
		Status:        StackStatus{Repo: "demo", Worktree: "feat", MainBranch: "main"},
		Drops:         []DropAction{{Position: 1, ShortSHA: "aaaaaaa", Subject: "one", Number: 41}},
		RebaseCmd:     []string{"git", "rebase", "--onto", "origin/main", "aaaaaaa1111", "feat"},
		BranchDeletes: []string{"ct/feat/aaaaaaaa"},
		Plan: Plan{
			Pushes:    []PushAction{{Position: 1, Branch: "ct/feat/bbbbbbbb"}},
			Retargets: []RetargetAction{{Number: 42, OldBase: "ct/feat/aaaaaaaa", NewBase: "main"}},
			Bodies:    []BodyAction{{Number: 42}},
		},
	}
	out := RenderRestackPlan(res)
	for _, want := range []string{
		"restack plan (dry-run)",
		"would drop landed  aaaaaaa #41",
		"git rebase --onto origin/main aaaaaaa1111 feat",
		"would delete branch origin/ct/feat/aaaaaaaa",
		"ct/feat/bbbbbbbb (rebased, position 1)",
		"would retarget PR #42  ct/feat/aaaaaaaa -> main",
		"would update body PR #42",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("restack plan render missing %q; full output:\n%s", want, out)
		}
	}
}

func TestRenderFinishPlan(t *testing.T) {
	res := FinishResult{
		Status:    StackStatus{Repo: "demo", Worktree: "feat", MainBranch: "main"},
		Drops:     []DropAction{{Position: 1, ShortSHA: "aaaaaaa", Subject: "one", Number: 41}},
		RebaseCmd: []string{"git", "rebase", "--onto", "origin/main", "aaaaaaa", "feat"},
	}
	out := RenderFinishPlan(res)
	if !strings.Contains(out, "finish plan (dry-run)") || strings.Contains(out, "restack plan (dry-run)") {
		t.Fatalf("finish renderer did not rename the cleanup operation:\n%s", out)
	}
}

// TestPredictRestackSubmit checks the dry-run prediction: survivors are pushed
// (rebased), the new bottom is retargeted to main if GitHub has not, and every
// surviving PR's nav table is re-spliced. The landed commit is excluded.
func TestPredictRestackSubmit(t *testing.T) {
	const wt, main = "wt", "main"
	br := func(id string) string { return "ct/" + wt + "/" + id }
	commits := []LocalCommit{
		commit("aaaaaaa1111", "aaaaaaaa", "one"),
		commit("bbbbbbb2222", "bbbbbbbb", "two"),
	}
	remotes := map[string]string{"aaaaaaaa": "aaaaaaa1111", "bbbbbbbb": "bbbbbbb2222"}
	// A landed; B still bases on the (to-be-deleted) landed branch.
	prs := []prRecord{
		pr(1, "MERGED", br("aaaaaaaa"), main, "APPROVED", "passing"),
		pr(2, "OPEN", br("bbbbbbbb"), br("aaaaaaaa"), "APPROVED", "passing"),
	}
	st := statusFrom(wt, main, commits, remotes, prs)
	plan := predictRestackSubmit(st)

	if len(plan.Pushes) != 1 || plan.Pushes[0].Branch != br("bbbbbbbb") || plan.Pushes[0].Create {
		t.Errorf("pushes = %+v, want a single force-update of ct/wt/bbbbbbbb", plan.Pushes)
	}
	if plan.Pushes[0].Position != 1 {
		t.Errorf("survivor position = %d, want 1 (renumbered after the drop)", plan.Pushes[0].Position)
	}
	if len(plan.Retargets) != 1 || plan.Retargets[0].Number != 2 ||
		plan.Retargets[0].OldBase != br("aaaaaaaa") || plan.Retargets[0].NewBase != main {
		t.Errorf("retargets = %+v, want #2 %s -> main", plan.Retargets, br("aaaaaaaa"))
	}
	if len(plan.Bodies) != 1 || plan.Bodies[0].Number != 2 {
		t.Errorf("bodies = %+v, want a nav update for #2", plan.Bodies)
	}
	if len(plan.Creates) != 0 || len(plan.Assigns) != 0 {
		t.Errorf("no creates/assigns expected for an already-submitted survivor: %+v", plan)
	}
}
