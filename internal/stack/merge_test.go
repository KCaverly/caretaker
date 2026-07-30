package stack

import (
	"reflect"
	"strings"
	"testing"
)

func mergeableStatus(base, mergeable string) StackStatus {
	return StackStatus{
		MainBranch: "main", GitHub: GitHub{Available: true}, Stack: Stack{NextAction: "merge"},
		Commits:   []Commit{{State: StateOpen, PR: &PR{Number: 10, Base: base, Mergeable: mergeable}}},
		MergeHint: &MergeHint{Number: 10, Subject: "subject", Body: "body"},
	}
}

func TestOpenPRsBasedOn(t *testing.T) {
	prs := []prRecord{
		{Number: 1, State: "MERGED", Base: "old"},
		{Number: 2, State: "OPEN", Base: "other"},
		{Number: 3, State: "OPEN", Base: "old"},
		{Number: 4, State: "OPEN", Base: "old"},
	}
	got := openPRsBasedOn(prs, "old")
	var numbers []int
	for _, p := range got {
		numbers = append(numbers, p.Number)
	}
	if want := []int{3, 4}; !reflect.DeepEqual(numbers, want) {
		t.Fatalf("open dependents = %v, want %v", numbers, want)
	}
	if !prOpenOnBase(prs, 3, "old") || prOpenOnBase(prs, 1, "old") || prOpenOnBase(prs, 3, "other") {
		t.Fatal("prOpenOnBase did not require matching number, open state, and base")
	}
}

func TestMergeArgsGuardsAndMessage(t *testing.T) {
	args, err := mergeArgs(mergeableStatus("main", "MERGEABLE"))
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(args, " ")
	for _, want := range []string{"pr merge 10", "--squash", "--subject subject", "--body body"} {
		if !strings.Contains(got, want) {
			t.Errorf("merge args missing %q: %s", want, got)
		}
	}
	if strings.Contains(got, "--delete-branch") {
		t.Errorf("branch deletion must be left to GitHub so stacked PRs are retargeted: %s", got)
	}
	for _, tc := range []struct{ name, base, mergeable, want string }{
		{"wrong base", "feature", "MERGEABLE", "not main branch"},
		{"conflicting", "main", "CONFLICTING", "not mergeable"},
		{"unknown", "main", "UNKNOWN", "not mergeable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := mergeArgs(mergeableStatus(tc.base, tc.mergeable))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want containing %q", err, tc.want)
			}
		})
	}
}

// TestMergeabilityPending is the other half of issue #57: GitHub reports UNKNOWN
// mergeability while it recomputes, which reconciles to "wait" and made a cascade
// merge issued moments after the previous land read as a stack that was not
// ready. Only that one in-flight input may be waited out — every other reason to
// refuse must still fail fast.
func TestMergeabilityPending(t *testing.T) {
	base := func(mergeable string, checks Checks, review, prBase string) StackStatus {
		return StackStatus{
			MainBranch: "main",
			GitHub:     GitHub{Available: true},
			Stack:      Stack{BaseChainOK: true, NextAction: "wait"},
			Commits: []Commit{{State: StateOpen, PR: &PR{
				Number: 11, Base: prBase, Mergeable: mergeable, Review: review, Checks: checks,
			}}},
		}
	}
	passing := Checks{Summary: "passing"}

	cases := []struct {
		name    string
		st      StackStatus
		pending bool
	}{
		{"UNKNOWN with everything else clear", base("UNKNOWN", passing, "", "main"), true},
		{"empty mergeability is also in flight", base("", passing, "", "main"), true},
		{"approved and UNKNOWN", base("UNKNOWN", passing, "APPROVED", "main"), true},
		{"no checks at all", base("UNKNOWN", Checks{Summary: "none"}, "", "main"), true},

		{"already mergeable is not pending", base("MERGEABLE", passing, "", "main"), false},
		{"conflicting is a real refusal", base("CONFLICTING", passing, "", "main"), false},
		{"failing checks are a real refusal", base("UNKNOWN", Checks{Summary: "failing"}, "", "main"), false},
		{"pending checks are a real refusal", base("UNKNOWN", Checks{Summary: "pending"}, "", "main"), false},
		{"review outstanding is a real refusal", base("UNKNOWN", passing, "REVIEW_REQUIRED", "main"), false},
		{"wrong base is a real refusal", base("UNKNOWN", passing, "", "ct/wt/aaaaaaaa"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := mergeabilityPending(tc.st); got != tc.pending {
				t.Fatalf("mergeabilityPending = %v, want %v", got, tc.pending)
			}
		})
	}

	t.Run("broken base chain is a real refusal", func(t *testing.T) {
		st := base("UNKNOWN", passing, "", "main")
		st.Stack.BaseChainOK = false
		if mergeabilityPending(st) {
			t.Fatal("a broken base chain is not something waiting will fix")
		}
	})
	t.Run("github unavailable is a real refusal", func(t *testing.T) {
		st := base("UNKNOWN", passing, "", "main")
		st.GitHub.Available = false
		if mergeabilityPending(st) {
			t.Fatal("without GitHub there is nothing to wait for")
		}
	})
	t.Run("no open PR is a real refusal", func(t *testing.T) {
		st := base("UNKNOWN", passing, "", "main")
		st.Commits = nil
		if mergeabilityPending(st) {
			t.Fatal("nothing to merge is not a pending calculation")
		}
	})
}

func TestPostMergeSettled(t *testing.T) {
	commit := func(state State, number int, base, mergeable string) Commit {
		return Commit{State: state, PR: &PR{Number: number, Base: base, Mergeable: mergeable}}
	}
	cases := []struct {
		name    string
		st      StackStatus
		settled bool
	}{
		{"merge not reflected", StackStatus{MainBranch: "main", Commits: []Commit{commit(StateOpen, 10, "main", "MERGEABLE")}}, false},
		{"old base still present", StackStatus{MainBranch: "main", Stack: Stack{BaseChainOK: false}, Commits: []Commit{commit(StateMerged, 10, "main", "UNKNOWN"), commit(StateOpen, 11, "old", "UNKNOWN")}}, false},
		{"retargeted but calculating", StackStatus{MainBranch: "main", Stack: Stack{BaseChainOK: true}, Commits: []Commit{commit(StateMerged, 10, "main", "UNKNOWN"), commit(StateOpen, 11, "main", "UNKNOWN")}}, false},
		{"next PR ready", StackStatus{MainBranch: "main", Stack: Stack{BaseChainOK: true}, Commits: []Commit{commit(StateMerged, 10, "main", "UNKNOWN"), commit(StateOpen, 11, "main", "MERGEABLE")}}, true},
		{"next PR conflicting", StackStatus{MainBranch: "main", Stack: Stack{BaseChainOK: true}, Commits: []Commit{commit(StateMerged, 10, "main", "UNKNOWN"), commit(StateOpen, 11, "main", "CONFLICTING")}}, true},
		{"fully landed", StackStatus{MainBranch: "main", Stack: Stack{BaseChainOK: true}, Commits: []Commit{commit(StateMerged, 10, "main", "UNKNOWN")}}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := postMergeSettled(tc.st, 10); got != tc.settled {
				t.Fatalf("postMergeSettled = %v, want %v", got, tc.settled)
			}
		})
	}
}
