// Package repo handles discovery of repos under the configured root and the git
// worktree operations ct performs against them.
package repo

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
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
	Dirty bool // uncommitted changes present
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
	out, err := Git(r.Path, "worktree", "list", "--porcelain")
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

// WorktreeStatus returns the coarse git state of a worktree — one `git status`
// subprocess. Commit times come separately from BranchTips, which covers a
// whole repo in a single call.
func WorktreeStatus(wt Worktree) (Status, error) {
	out, err := Git(wt.Path, "status", "--porcelain")
	if err != nil {
		return Status{}, err
	}
	return Status{Dirty: strings.TrimSpace(out) != ""}, nil
}

// BranchTip is a local branch tip's metadata, gathered in one for-each-ref
// pass: the committer time (unix seconds) and the commit subject.
type BranchTip struct {
	Time    int64  // committer time (unix seconds)
	Subject string // first line of the tip commit message
}

// BranchTips returns the tip metadata of every local branch in r, keyed by
// short branch name — one subprocess for the whole repo, replacing a
// per-worktree `git log -1`. Worktree HEADs equal their branch tips in ct's
// branch-per-worktree model; detached worktrees simply miss the map (a zero
// BranchTip).
func BranchTips(r Repo) (map[string]BranchTip, error) {
	// %00 expands to a NUL in the output, an unambiguous separator no branch
	// name or subject can contain (a raw NUL can't be passed as an exec
	// argument). Two of them fence three fields; a NUL beats spaces here because
	// commit subjects contain spaces freely.
	out, err := Git(r.Path, "for-each-ref", "--format=%(refname:short)%00%(committerdate:unix)%00%(subject)", "refs/heads")
	if err != nil {
		return nil, err
	}
	return parseBranchTips(out), nil
}

// parseBranchTips parses the NUL-fenced for-each-ref output (three fields per
// line: short name, committer unix time, subject) into the tip map. Lines
// missing a field or with an unparseable time are skipped; the NUL separator
// keeps subjects — which contain spaces freely — intact.
func parseBranchTips(out string) map[string]BranchTip {
	tips := make(map[string]BranchTip)
	for _, line := range strings.Split(out, "\n") {
		fields := strings.SplitN(strings.TrimRight(line, "\r"), "\x00", 3)
		if len(fields) < 3 {
			continue
		}
		name, ts, subject := fields[0], fields[1], fields[2]
		t, err := strconv.ParseInt(strings.TrimSpace(ts), 10, 64)
		if err != nil {
			continue
		}
		tips[name] = BranchTip{Time: t, Subject: subject}
	}
	return tips
}

// AheadBehind reports how far a worktree's branch has diverged from the repo's
// main branch: how many commits it carries that main lacks (ahead) and how many
// main carries that it lacks (behind), via a symmetric difference against the
// merge-base. It runs one `git rev-list --left-right --count <main>...HEAD` in
// the worktree; git prints the left count (commits reachable from main but not
// HEAD = behind) then the right count (reachable from HEAD but not main =
// ahead), verified against git-rev-list's docs and a manual test.
//
// ok is false — divergence is simply unavailable — for the main worktree
// itself, when mainBranch is empty (a detached primary tree), or on any git
// error (e.g. an unborn branch with no commits to compare).
func AheadBehind(wt Worktree, mainBranch string) (ahead, behind int, ok bool) {
	if wt.IsMain || mainBranch == "" {
		return 0, 0, false
	}
	out, err := Git(wt.Path, "rev-list", "--left-right", "--count", mainBranch+"...HEAD")
	if err != nil {
		return 0, 0, false
	}
	behind, ahead, ok = parseAheadBehind(out)
	return ahead, behind, ok
}

// parseAheadBehind parses the two whitespace-separated integers of `git
// rev-list --left-right --count` output (left = behind, right = ahead). ok is
// false when the line isn't the expected two-number shape.
func parseAheadBehind(out string) (behind, ahead int, ok bool) {
	fields := strings.Fields(out)
	if len(fields) != 2 {
		return 0, 0, false
	}
	b, err1 := strconv.Atoi(fields[0])
	a, err2 := strconv.Atoi(fields[1])
	if err1 != nil || err2 != nil {
		return 0, 0, false
	}
	return b, a, true
}

// UncommittedDiffstat sums the line changes a worktree carries against HEAD —
// staged and unstaged together — via `git diff HEAD --shortstat`. Untracked
// files are intentionally excluded: their mere existence is already reported by
// the dirty flag, and shortstat can't count lines in a file git isn't tracking.
// Only meaningful for dirty worktrees, so callers gate on that. Returns 0/0 for
// a clean tree (empty shortstat output).
func UncommittedDiffstat(wt Worktree) (added, deleted int, err error) {
	out, err := Git(wt.Path, "diff", "HEAD", "--shortstat")
	if err != nil {
		return 0, 0, err
	}
	added, deleted = parseShortstat(out)
	return added, deleted, nil
}

// parseShortstat pulls the insertion and deletion counts out of a git
// --shortstat line ("N files changed, N insertions(+), N deletions(-)"),
// tolerating a missing insertions or deletions segment (git omits whichever is
// zero) and empty input (0/0).
func parseShortstat(out string) (added, deleted int) {
	for _, seg := range strings.Split(strings.TrimSpace(out), ",") {
		seg = strings.TrimSpace(seg)
		switch {
		case strings.Contains(seg, "insertion"):
			added = leadingInt(seg)
		case strings.Contains(seg, "deletion"):
			deleted = leadingInt(seg)
		}
	}
	return added, deleted
}

// leadingInt returns the integer at the start of a shortstat segment like
// "3 insertions(+)", or 0 when it doesn't start with a number.
func leadingInt(seg string) int {
	fields := strings.Fields(seg)
	if len(fields) == 0 {
		return 0
	}
	n, _ := strconv.Atoi(fields[0])
	return n
}

// FileStat is one file's change summary from `git diff --numstat`: its path and
// the added/deleted line counts. Binary files carry no line counts (git prints
// "-\t-" for them), so Add/Del stay 0 and Binary is set instead. A rename shows
// up in Path in git's own "old => new" (or "{a => b}") shorthand, passed through
// verbatim.
type FileStat struct {
	Path     string
	Add, Del int
	Binary   bool
}

// DiffAgainstBase returns the unified diff of everything the worktree's branch
// carries beyond base, via `git diff <base>...HEAD` (three-dot: the branch tip
// against its merge-base with base, so unrelated commits base landed since the
// fork don't show up as reverse changes). An empty base — no primary-worktree
// branch to compare against — yields an empty diff and no error, so the caller
// simply omits the section.
func DiffAgainstBase(wt Worktree, base string) (string, error) {
	if base == "" {
		return "", nil
	}
	return Git(wt.Path, "diff", base+"...HEAD")
}

// DiffUncommitted returns the unified diff of the worktree's uncommitted work —
// staged and unstaged together — via `git diff HEAD`. Untracked files are not
// included (git diff never shows them); UntrackedFiles lists those separately.
func DiffUncommitted(wt Worktree) (string, error) {
	return Git(wt.Path, "diff", "HEAD")
}

// NumstatAgainstBase returns the per-file change summary of everything the
// branch carries beyond base, parsed from `git diff --numstat <base>...HEAD`. An
// empty base yields no files and no error, mirroring DiffAgainstBase.
func NumstatAgainstBase(wt Worktree, base string) ([]FileStat, error) {
	if base == "" {
		return nil, nil
	}
	out, err := Git(wt.Path, "diff", "--numstat", base+"...HEAD")
	if err != nil {
		return nil, err
	}
	return parseNumstat(out), nil
}

// NumstatUncommitted returns the per-file change summary of the worktree's
// uncommitted work (staged+unstaged vs HEAD), parsed from `git diff --numstat
// HEAD`.
func NumstatUncommitted(wt Worktree) ([]FileStat, error) {
	out, err := Git(wt.Path, "diff", "--numstat", "HEAD")
	if err != nil {
		return nil, err
	}
	return parseNumstat(out), nil
}

// parseNumstat parses `git diff --numstat` output: one tab-separated record per
// line ("added\tdeleted\tpath"). A binary file has "-" for both counts, which we
// surface as Binary with zero counts. Malformed lines (fewer than three fields)
// are skipped; the path is taken verbatim, so a rename's "old => new" shorthand
// passes straight through.
func parseNumstat(out string) []FileStat {
	var stats []FileStat
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			continue
		}
		fields := strings.SplitN(line, "\t", 3)
		if len(fields) < 3 {
			continue
		}
		fs := FileStat{Path: fields[2]}
		if fields[0] == "-" && fields[1] == "-" {
			fs.Binary = true
		} else {
			fs.Add, _ = strconv.Atoi(fields[0])
			fs.Del, _ = strconv.Atoi(fields[1])
		}
		stats = append(stats, fs)
	}
	return stats
}

// UntrackedFiles returns the worktree's untracked file paths — the "?? " entries
// of `git status --porcelain`. They carry no diff body (git can't diff a file it
// isn't tracking); the diff viewer lists them in its index so the branch's new
// files aren't invisible.
func UntrackedFiles(wt Worktree) ([]string, error) {
	out, err := Git(wt.Path, "status", "--porcelain")
	if err != nil {
		return nil, err
	}
	return parseUntracked(out), nil
}

// parseUntracked pulls the untracked-file paths ("?? path" lines) out of `git
// status --porcelain` output, dropping the "?? " prefix. Every other status
// code (tracked modifications, staged changes) is ignored — those show up in the
// diff body instead.
func parseUntracked(out string) []string {
	var paths []string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.HasPrefix(line, "?? ") {
			paths = append(paths, strings.TrimPrefix(line, "?? "))
		}
	}
	return paths
}

// worktreeNameForbidden lists the punctuation git forbids inside a ref name
// (git-check-ref-format). A new-worktree name is substituted verbatim into both
// the branch name and the worktree path, so any of these would otherwise fail
// deep inside `git worktree add` with raw stderr. Space and control characters
// are checked separately (as a range), and '/' is deliberately absent — interior
// slashes are allowed for branch namespacing.
const worktreeNameForbidden = "~^:?*[\\"

// ValidateWorktreeName rejects new-worktree names before ct runs `git worktree
// add`, so bad input yields an inline hint instead of raw git stderr — and so a
// name containing ".." can never place the worktree outside the repo when it is
// filepath.Join'd into the worktree_path template.
//
// It lives in repo (next to CreateWorktree, the consumer of the name) rather
// than tui because it guards a git/filesystem operation, not UI state, and is
// reusable as a defense-in-depth check by any future caller of CreateWorktree.
//
// Interior "/" is intentionally allowed: branch namespacing like "feature/foo"
// is legitimate and the worktree_path template nests it as a subdirectory. Path
// traversal stays impossible because ".." (in any component) is rejected, so no
// join can climb out of the repo.
func ValidateWorktreeName(name string) error {
	switch {
	case strings.TrimSpace(name) == "":
		return fmt.Errorf("name cannot be empty")
	case strings.HasPrefix(name, "-"):
		return fmt.Errorf("name cannot start with '-'")
	case strings.HasPrefix(name, "/"):
		return fmt.Errorf("name cannot start with '/'")
	case strings.HasSuffix(name, "/"):
		return fmt.Errorf("name cannot end with '/'")
	case strings.HasSuffix(name, ".lock"):
		return fmt.Errorf("name cannot end with '.lock'")
	case strings.Contains(name, ".."):
		return fmt.Errorf("name cannot contain '..'")
	// No slash-separated component may begin with '.' (a git ref rule; it also
	// keeps the worktree directory out of hidden-file territory). This covers a
	// leading '.' and any "/." sequence.
	case strings.HasPrefix(name, "."), strings.Contains(name, "/."):
		return fmt.Errorf("name component cannot start with '.'")
	}
	for _, r := range name {
		switch {
		case r < 0x20 || r == 0x7f:
			return fmt.Errorf("name cannot contain control characters")
		case r == ' ':
			return fmt.Errorf("name cannot contain spaces")
		case strings.ContainsRune(worktreeNameForbidden, r):
			return fmt.Errorf("name cannot contain %q", r)
		}
	}
	return nil
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
	if _, err := Git(r.Path, args...); err != nil {
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
	if _, err := Git(r.Path, "worktree", "remove", "--force", wt.Path); err != nil {
		return err
	}
	if deleteBranch && wt.Branch != "" {
		if _, err := Git(r.Path, "branch", "-D", wt.Branch); err != nil {
			return err
		}
	}
	return nil
}

// gitTimeout bounds every git subprocess so a hung call — a credential helper
// waiting on a TTY it doesn't have, index lock contention, a dead network
// mount — fails visibly instead of stranding its goroutine (and the deck
// refresh it belongs to) forever.
const gitTimeout = 30 * time.Second

// Git runs a git command in dir and returns combined stdout, or an error that
// includes stderr. It is exported so sibling packages (e.g. internal/stack)
// reuse the same 30s-timeout, stderr-wrapping runner instead of shelling out
// their own way.
func Git(dir string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), gitTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...)
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
