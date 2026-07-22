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
