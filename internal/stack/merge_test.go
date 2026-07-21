package stack

import (
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
