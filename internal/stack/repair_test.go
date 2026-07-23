package stack

import (
	"strings"
	"testing"
)

func TestPlanRepairDeletedBase(t *testing.T) {
	const wt, main = "wt", "main"
	old := "ct/wt/aaaaaaaa"
	head := "ct/wt/bbbbbbbb"
	st := StackStatus{
		Worktree: wt, MainBranch: main,
		Commits: []Commit{{State: StateClosed, StackID: stringPtr("bbbbbbbb"), PR: &PR{Number: 9, Base: old}}},
	}
	prs := []prRecord{
		{Number: 8, State: "MERGED", Head: old, Base: main, HeadSHA: "aaaaaaa1111", MergedAt: "now"},
		{Number: 9, State: "CLOSED", Head: head, Base: old},
	}
	plan, err := planRepair(st, prs, 9)
	if err != nil {
		t.Fatal(err)
	}
	if plan.FormerBase != old || plan.FormerBaseSHA != "aaaaaaa1111" || plan.NewBase != main || plan.Head != head {
		t.Fatalf("repair plan = %+v", plan)
	}
	out := RenderRepairPlan(RepairResult{Status: st, Plan: plan, DryRun: true})
	for _, want := range []string{"repair plan", "would restore " + old, "would reopen PR #9", old + " -> main", "would delete temporary branch " + old} {
		if !strings.Contains(out, want) {
			t.Errorf("repair output missing %q:\n%s", want, out)
		}
	}
}

func TestPlanRepairGuards(t *testing.T) {
	st := StackStatus{Worktree: "wt", MainBranch: "main", Commits: []Commit{{State: StateClosed, PR: &PR{Number: 9}}}}
	base := prRecord{Number: 8, State: "MERGED", Head: "ct/wt/aaaaaaaa", HeadSHA: "aaaaaaa1111", MergedAt: "now"}
	for _, tc := range []struct {
		name string
		prs  []prRecord
		want string
	}{
		{"not closed", []prRecord{{Number: 9, State: "OPEN", Head: "ct/wt/bbbbbbbb", Base: base.Head}, base}, "requires a closed"},
		{"not stack base", []prRecord{{Number: 9, State: "CLOSED", Head: "ct/wt/bbbbbbbb", Base: "feature"}, base}, "not a stack branch"},
		{"no recoverable sha", []prRecord{{Number: 9, State: "CLOSED", Head: "ct/wt/bbbbbbbb", Base: base.Head}}, "no merged PR"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := planRepair(st, tc.prs, 9)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want containing %q", err, tc.want)
			}
		})
	}
}

func stringPtr(s string) *string { return &s }
