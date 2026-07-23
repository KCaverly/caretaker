package stack

import (
	"os"
	"strings"
	"testing"

	"github.com/KCaverly/caretaker/internal/repo"
)

func reorderCommits() []Commit {
	return []Commit{
		{Position: 1, SHA: "aaaaaaaa11111111111111111111111111111111", ShortSHA: "aaaaaaa", Subject: "first"},
		{Position: 2, SHA: "bbbbbbbb22222222222222222222222222222222", ShortSHA: "bbbbbbb", Subject: "second subject"},
		{Position: 3, SHA: "cccccccc33333333333333333333333333333333", ShortSHA: "ccccccc", Subject: "third"},
	}
}

func TestApplyReorderIntegration(t *testing.T) {
	dir := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		out, err := repo.Git(dir, args...)
		if err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
		return strings.TrimSpace(out)
	}
	git("init", "-b", "main")
	git("config", "user.name", "CT Test")
	git("config", "user.email", "ct@example.test")
	if err := os.WriteFile(dir+"/base", []byte("base\n"), 0600); err != nil {
		t.Fatal(err)
	}
	git("add", "base")
	git("commit", "-m", "base")
	git("checkout", "-b", "feat")
	for _, subject := range []string{"A", "B", "C"} {
		if err := os.WriteFile(dir+"/"+subject, []byte(subject+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
		git("add", subject)
		git("commit", "-m", subject)
	}
	locals, err := localCommits(dir, "main")
	if err != nil {
		t.Fatal(err)
	}
	var commits []Commit
	for i, c := range locals {
		commits = append(commits, Commit{Position: i + 1, SHA: c.SHA, ShortSHA: c.ShortSHA, Subject: c.Subject})
	}
	text := ""
	for i := len(commits) - 1; i >= 0; i-- {
		text += "pick " + commits[i].SHA + " " + commits[i].Subject + "\n"
	}
	plan, err := parseReorderTodo(text, commits)
	if err != nil {
		t.Fatal(err)
	}
	pre := git("rev-parse", "feat")
	if err := applyReorder(dir, "main", "feat", pre, plan); err != nil {
		t.Fatal(err)
	}
	if got := git("log", "--format=%s", "--reverse", "main..feat"); got != "C\nB\nA" {
		t.Fatalf("reordered subjects = %q, want C/B/A", got)
	}
	if got := git("status", "--porcelain"); got != "" {
		t.Fatalf("dirty tree after reorder: %s", got)
	}
}

func TestParseReorderTodoPermutation(t *testing.T) {
	commits := reorderCommits()
	text := "# comment\n" +
		"pick " + commits[2].SHA + " " + commits[2].Subject + "\n" +
		"pick " + commits[0].SHA + " " + commits[0].Subject + "\n" +
		"pick " + commits[1].SHA + " " + commits[1].Subject + "\n"
	plan, err := parseReorderTodo(text, commits)
	if err != nil {
		t.Fatal(err)
	}
	if !plan.Changed() || plan.Entries[0].OldPosition != 3 || plan.Entries[0].NewPosition != 1 {
		t.Fatalf("parsed plan = %+v", plan)
	}
	sequence := rebaseSequence(plan)
	if !strings.HasPrefix(sequence, "pick "+commits[2].SHA) {
		t.Fatalf("rebase sequence did not preserve requested order:\n%s", sequence)
	}
}

func TestParseReorderTodoGuards(t *testing.T) {
	commits := reorderCommits()
	valid := renderReorderTodo(commits)
	for _, tc := range []struct{ name, text, want string }{
		{"missing", strings.Replace(valid, "pick "+commits[1].SHA+" "+commits[1].Subject+"\n", "", 1), "expected all"},
		{"duplicate", valid + "pick " + commits[0].SHA + " " + commits[0].Subject + "\n", "more than once"},
		{"changed subject", strings.Replace(valid, commits[1].Subject, "edited", 1), "only reorder"},
		{"changed verb", strings.Replace(valid, "pick "+commits[0].SHA, "drop "+commits[0].SHA, 1), "invalid todo"},
		{"abbreviated", strings.Replace(valid, commits[0].SHA, commits[0].ShortSHA, 1), "unknown or abbreviated"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseReorderTodo(tc.text, commits)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want containing %q", err, tc.want)
			}
		})
	}
}

func TestRenderReorderPlan(t *testing.T) {
	plan, err := parseReorderTodo("pick cccccccc33333333333333333333333333333333 third\npick aaaaaaaa11111111111111111111111111111111 first\npick bbbbbbbb22222222222222222222222222222222 second subject\n", reorderCommits())
	if err != nil {
		t.Fatal(err)
	}
	out := RenderReorderPlan(ReorderResult{Status: StackStatus{Repo: "demo", Worktree: "feat"}, Plan: plan, DryRun: true})
	if !strings.Contains(out, "reorder plan (dry-run)") || !strings.Contains(out, "3 -> 1") {
		t.Fatalf("unexpected reorder rendering:\n%s", out)
	}
}
