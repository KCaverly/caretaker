package stack

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/KCaverly/caretaker/internal/repo"
)

type ReorderOptions struct {
	Params
	DryRun  bool
	Yes     bool
	Confirm func(ReorderPlan) bool
}

type ReorderEntry struct {
	OldPosition int    `json:"old_position"`
	NewPosition int    `json:"new_position"`
	SHA         string `json:"sha"`
	ShortSHA    string `json:"short_sha"`
	Subject     string `json:"subject"`
}

type ReorderPlan struct {
	Entries []ReorderEntry `json:"entries"`
}

func (p ReorderPlan) Changed() bool {
	for _, e := range p.Entries {
		if e.OldPosition != e.NewPosition {
			return true
		}
	}
	return false
}

type ReorderResult struct {
	Status   StackStatus `json:"status"`
	Plan     ReorderPlan `json:"plan"`
	DryRun   bool        `json:"dry_run"`
	Nothing  bool        `json:"nothing"`
	Executed []string    `json:"executed,omitempty"`
}

func renderReorderTodo(commits []Commit) string {
	var b strings.Builder
	b.WriteString("# Reorder the pick lines. Do not add, remove, or edit commits.\n")
	for _, c := range commits {
		fmt.Fprintf(&b, "pick %s %s\n", c.SHA, c.Subject)
	}
	return b.String()
}

func parseReorderTodo(text string, commits []Commit) (ReorderPlan, error) {
	bySHA := map[string]Commit{}
	oldPosition := map[string]int{}
	for _, c := range commits {
		bySHA[c.SHA] = c
		oldPosition[c.SHA] = c.Position
	}
	seen := map[string]bool{}
	var entries []ReorderEntry
	s := bufio.NewScanner(strings.NewReader(text))
	for s.Scan() {
		line := strings.TrimSpace(s.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 3 || fields[0] != "pick" {
			return ReorderPlan{}, fmt.Errorf("invalid todo line %q; expected: pick <full-sha> <subject>", line)
		}
		sha := fields[1]
		c, ok := bySHA[sha]
		if !ok {
			return ReorderPlan{}, fmt.Errorf("todo contains unknown or abbreviated commit %s", sha)
		}
		if seen[sha] {
			return ReorderPlan{}, fmt.Errorf("todo contains commit %s more than once", sha)
		}
		wantLine := "pick " + sha + " " + c.Subject
		if line != wantLine {
			return ReorderPlan{}, fmt.Errorf("todo changed commit %s; only reorder complete lines", c.ShortSHA)
		}
		seen[sha] = true
		entries = append(entries, ReorderEntry{OldPosition: oldPosition[sha], NewPosition: len(entries) + 1, SHA: sha, ShortSHA: c.ShortSHA, Subject: c.Subject})
	}
	if err := s.Err(); err != nil {
		return ReorderPlan{}, err
	}
	if len(entries) != len(commits) {
		return ReorderPlan{}, fmt.Errorf("todo has %d commits; expected all %d", len(entries), len(commits))
	}
	return ReorderPlan{Entries: entries}, nil
}

func editReorderTodo(dir, initial string) (string, error) {
	f, err := os.CreateTemp("", "ct-reorder-*.todo")
	if err != nil {
		return "", err
	}
	path := f.Name()
	defer os.Remove(path)
	if _, err := f.WriteString(initial); err != nil {
		f.Close()
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	editor, err := repo.Git(dir, "var", "GIT_EDITOR")
	if err != nil {
		return "", fmt.Errorf("resolving Git editor: %w", err)
	}
	editor = strings.TrimSpace(editor)
	if editor == "" {
		return "", fmt.Errorf("Git editor resolved to an empty command")
	}
	cmd := exec.Command("sh", "-c", editor+` "$1"`, "ct-editor", path)
	cmd.Dir = dir
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("editor failed: %w", err)
	}
	data, err := os.ReadFile(path)
	return string(data), err
}

func rebaseSequence(plan ReorderPlan) string {
	var b strings.Builder
	for _, e := range plan.Entries {
		fmt.Fprintf(&b, "pick %s %s\n", e.SHA, e.Subject)
	}
	return b.String()
}

func applyReorder(dir, mainBranch, branch, preSHA string, plan ReorderPlan) error {
	todo, err := os.CreateTemp("", "ct-rebase-*.todo")
	if err != nil {
		return err
	}
	defer os.Remove(todo.Name())
	if _, err := todo.WriteString(rebaseSequence(plan)); err != nil {
		todo.Close()
		return err
	}
	if err := todo.Close(); err != nil {
		return err
	}

	script, err := os.CreateTemp("", "ct-sequence-editor-*.sh")
	if err != nil {
		return err
	}
	defer os.Remove(script.Name())
	if _, err := script.WriteString("#!/bin/sh\ncp \"$CT_REORDER_TODO\" \"$1\"\n"); err != nil {
		script.Close()
		return err
	}
	if err := script.Close(); err != nil {
		return err
	}
	if err := os.Chmod(script.Name(), 0700); err != nil {
		return err
	}

	_, err = gitEnv(dir, []string{"GIT_SEQUENCE_EDITOR=" + script.Name(), "GIT_EDITOR=true", "CT_REORDER_TODO=" + todo.Name()}, "rebase", "-i", mainBranch, branch)
	if err == nil {
		return nil
	}
	conflicts := unmergedFiles(dir)
	if _, abortErr := repo.Git(dir, "rebase", "--abort"); abortErr != nil {
		return fmt.Errorf("reorder failed (%v) and rebase --abort failed (%v)", err, abortErr)
	}
	after, shaErr := branchSHA(dir, branch)
	if shaErr != nil || after != preSHA {
		return fmt.Errorf("DANGER: reorder abort did not restore %s to %s (now %s, read error %v)", branch, preSHA, after, shaErr)
	}
	msg := "reorder conflicted and was aborted; original branch restored"
	if len(conflicts) > 0 {
		msg += "; conflicting files: " + strings.Join(conflicts, ", ")
	}
	return fmt.Errorf("%s: %w", msg, err)
}

func Reorder(o ReorderOptions) (ReorderResult, error) {
	res := ReorderResult{DryRun: o.DryRun}
	if err := ensureCleanTree(o.WorktreeDir); err != nil {
		return res, err
	}
	if err := ensureNoRebaseInProgress(o.WorktreeDir); err != nil {
		return res, err
	}
	if err := requireGH(); err != nil {
		return res, err
	}
	if err := fetchOrigin(o.WorktreeDir); err != nil {
		return res, fmt.Errorf("git fetch origin failed: %w", err)
	}
	p := o.Params
	p.Fetch = false
	st, _, err := gatherStatus(p)
	if err != nil {
		return res, err
	}
	st.Fetched = true
	res.Status = st
	if !st.GitHub.Available {
		return res, fmt.Errorf("GitHub is unavailable: %s", strings.Join(st.GitHub.Warnings, "; "))
	}
	if len(st.Commits) < 2 {
		res.Nothing = true
		return res, nil
	}
	for _, c := range st.Commits {
		if c.State == StateMerged || c.State == StateClosed || c.State == StateDuplicateID {
			return res, fmt.Errorf("commit %s is %s; resolve landed or troubled commits before reordering", c.ShortSHA, c.State)
		}
	}
	if len(st.Stack.Orphans) > 0 {
		return res, fmt.Errorf("resolve orphan PRs before reordering")
	}
	text, err := editReorderTodo(o.WorktreeDir, renderReorderTodo(st.Commits))
	if err != nil {
		return res, err
	}
	res.Plan, err = parseReorderTodo(text, st.Commits)
	if err != nil {
		return res, err
	}
	if !res.Plan.Changed() {
		res.Nothing = true
		return res, nil
	}
	if o.DryRun {
		return res, nil
	}
	if !o.Yes && (o.Confirm == nil || !o.Confirm(res.Plan)) {
		res.Nothing = true
		return res, nil
	}
	preSHA, err := branchSHA(o.WorktreeDir, st.Branch)
	if err != nil {
		return res, err
	}
	if err := applyReorder(o.WorktreeDir, st.MainBranch, st.Branch, preSHA, res.Plan); err != nil {
		return res, err
	}
	res.Executed = append(res.Executed, fmt.Sprintf("reordered %d commits", len(res.Plan.Entries)))
	sub, err := Submit(SubmitOptions{Params: o.Params})
	res.Executed = append(res.Executed, sub.Executed...)
	res.Status = sub.Status
	return res, err
}
