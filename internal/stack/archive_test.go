package stack

import (
	"reflect"
	"strings"
	"testing"
)

func TestPlanArchiveCompleteStack(t *testing.T) {
	st := StackStatus{Worktree: "feat", MainBranch: "main", Stack: Stack{NextAction: "complete"}, Commits: []Commit{
		{Position: 1, SHA: "a", ShortSHA: "a", Subject: "one", StackID: stringPtr("aaaaaaaa"), State: StateMerged, PR: &PR{Number: 1}},
		{Position: 2, SHA: "b", ShortSHA: "b", Subject: "two", StackID: stringPtr("bbbbbbbb"), State: StateMerged, PR: &PR{Number: 2}},
	}}
	got, err := planArchive(st)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"ct/feat/aaaaaaaa", "ct/feat/bbbbbbbb"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("archive branches = %v, want %v", got, want)
	}
}

func TestPlanArchiveRejectsIncompleteStack(t *testing.T) {
	_, err := planArchive(StackStatus{Stack: Stack{NextAction: "submit"}})
	if err == nil || !strings.Contains(err.Error(), "no longer complete") {
		t.Fatalf("error = %v", err)
	}
}
