// Package repo handles discovery of repos under the configured root and the git
// worktree operations ct performs against them.
package repo

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// Repo is a git repository discovered directly under the config root.
type Repo struct {
	Name string // directory name
	Path string // absolute path to the repo's main working tree
}

// Worktree is a single git worktree belonging to a Repo.
type Worktree struct {
	Repo   string // owning repo name
	Name   string // worktree name (directory leaf, or "(main)" for the primary)
	Path   string // absolute path
	Branch string // checked-out branch (short name), or "" if detached
	IsMain bool   // true for the repo's primary working tree
}

// Status is the coarse git state of a worktree.
type Status struct {
	Dirty      bool  // uncommitted changes present
	CommitTime int64 // HEAD commit time (unix seconds), 0 if unknown
}

// DiscoverRepos returns the git repositories that are immediate children of root,
// sorted by name.
func DiscoverRepos(root string) ([]Repo, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, fmt.Errorf("reading root %q: %w", root, err)
	}

	var repos []Repo
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		path := filepath.Join(root, e.Name())
		if !isGitRepo(path) {
			continue
		}
		repos = append(repos, Repo{Name: e.Name(), Path: path})
	}
	sort.Slice(repos, func(i, j int) bool { return repos[i].Name < repos[j].Name })
	return repos, nil
}

// isGitRepo reports whether path contains a .git entry (dir or file).
func isGitRepo(path string) bool {
	_, err := os.Stat(filepath.Join(path, ".git"))
	return err == nil
}

// ListWorktrees returns the worktrees of r, with the primary worktree first.
func ListWorktrees(r Repo) ([]Worktree, error) {
	out, err := git(r.Path, "worktree", "list", "--porcelain")
	if err != nil {
		return nil, err
	}
	wts := parseWorktreeList(out)
	for i := range wts {
		wts[i].Repo = r.Name
		if i == 0 {
			wts[i].IsMain = true
			wts[i].Name = "(main)"
		} else {
			wts[i].Name = filepath.Base(wts[i].Path)
		}
	}
	return wts, nil
}

// parseWorktreeList parses `git worktree list --porcelain` output. Records are
// separated by blank lines; we only need the worktree path and branch.
func parseWorktreeList(out string) []Worktree {
	var (
		wts []Worktree
		cur *Worktree
	)
	flush := func() {
		if cur != nil {
			wts = append(wts, *cur)
			cur = nil
		}
	}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimRight(line, "\r")
		switch {
		case strings.HasPrefix(line, "worktree "):
			flush()
			cur = &Worktree{Path: strings.TrimPrefix(line, "worktree ")}
		case cur == nil:
			// ignore anything before the first record
		case strings.HasPrefix(line, "branch "):
			ref := strings.TrimPrefix(line, "branch ")
			cur.Branch = strings.TrimPrefix(ref, "refs/heads/")
		case line == "":
			flush()
		}
	}
	flush()
	return wts
}

// WorktreeStatus returns the coarse git state of a worktree.
func WorktreeStatus(wt Worktree) (Status, error) {
	out, err := git(wt.Path, "status", "--porcelain")
	if err != nil {
		return Status{}, err
	}
	st := Status{Dirty: strings.TrimSpace(out) != ""}
	if ct, err := git(wt.Path, "log", "-1", "--format=%ct"); err == nil {
		if t, err := strconv.ParseInt(strings.TrimSpace(ct), 10, 64); err == nil {
			st.CommitTime = t
		}
	}
	return st, nil
}

// CreateWorktree adds a new worktree at relPath (relative to the repo) on a new
// branch, based on baseRef. If baseRef is empty, the repo's current HEAD is used.
// Returns the created Worktree.
func CreateWorktree(r Repo, relPath, branch, baseRef string) (Worktree, error) {
	abs := filepath.Join(r.Path, relPath)
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return Worktree{}, fmt.Errorf("creating worktree parent dir: %w", err)
	}

	args := []string{"worktree", "add", "-b", branch, abs}
	if baseRef != "" {
		args = append(args, baseRef)
	}
	if _, err := git(r.Path, args...); err != nil {
		return Worktree{}, err
	}
	return Worktree{
		Repo:   r.Name,
		Name:   filepath.Base(abs),
		Path:   abs,
		Branch: branch,
	}, nil
}

// RemoveWorktree removes a worktree. When deleteBranch is true, its branch is
// also deleted. The primary worktree cannot be removed.
func RemoveWorktree(r Repo, wt Worktree, deleteBranch bool) error {
	if wt.IsMain {
		return fmt.Errorf("refusing to remove the primary worktree")
	}
	// --force so removal works even when the worktree has uncommitted or
	// untracked changes; the caller confirms this destructive action.
	if _, err := git(r.Path, "worktree", "remove", "--force", wt.Path); err != nil {
		return err
	}
	if deleteBranch && wt.Branch != "" {
		if _, err := git(r.Path, "branch", "-D", wt.Branch); err != nil {
			return err
		}
	}
	return nil
}

// git runs a git command in dir and returns combined stdout, or an error that
// includes stderr.
func git(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("git %s: %s", strings.Join(args, " "), msg)
	}
	return stdout.String(), nil
}
