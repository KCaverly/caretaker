// Package repo handles discovery of repos under the configured root and the git
// worktree operations ct performs against them.
package repo

import (
	"context"
	"errors"
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

// DiscoverReposIn returns the git repositories that are immediate children of
// any of roots, merged and sorted by display name.
//
// A repository reachable from two roots (nested or symlinked trees) is listed
// once, keyed by its absolute path. A root that has gone missing since startup —
// config validation resolves them all, so this means it disappeared underneath a
// running ct — is skipped rather than allowed to hide the roots that are still
// there; its error surfaces only when no root yielded anything.
func DiscoverReposIn(roots []string) ([]Repo, error) {
	var repos []Repo
	var firstErr error
	seen := map[string]bool{}
	for _, root := range roots {
		found, err := DiscoverRepos(root)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		for _, r := range found {
			if seen[r.Path] {
				continue
			}
			seen[r.Path] = true
			repos = append(repos, r)
		}
	}
	if len(repos) == 0 && firstErr != nil {
		return nil, firstErr
	}
	disambiguateNames(repos)
	sort.Slice(repos, func(i, j int) bool { return repos[i].Name < repos[j].Name })
	return repos, nil
}

// maxNameDepth bounds how many trailing path segments a disambiguated name may
// use before it stops being a name and starts being a path.
const maxNameDepth = 4

// disambiguateNames makes every repo's display name unique. Two roots can hold
// repositories with the same directory name (~/work/api and ~/personal/api), and
// Repo.Name is not only what the deck shows: it is half of the workspace key that
// identifies sessions and persisted state. Two repos sharing a name would share
// those, so the name has to distinguish them.
//
// Colliding names grow leftward one path segment at a time — "api" becomes
// "work/api" and "personal/api" — and only colliding ones do, so the ordinary
// single-root case keeps bare names.
func disambiguateNames(repos []Repo) {
	byName := map[string][]int{}
	for i, r := range repos {
		byName[r.Name] = append(byName[r.Name], i)
	}
	for _, idxs := range byName {
		if len(idxs) < 2 {
			continue
		}
		depth := 2
		for ; depth < maxNameDepth; depth++ {
			if distinctTails(repos, idxs, depth) {
				break
			}
		}
		for _, i := range idxs {
			repos[i].Name = pathTail(repos[i].Path, depth)
		}
	}
}

// distinctTails reports whether the given repos' last-n-segment names are all
// different from each other.
func distinctTails(repos []Repo, idxs []int, depth int) bool {
	seen := map[string]bool{}
	for _, i := range idxs {
		tail := pathTail(repos[i].Path, depth)
		if seen[tail] {
			return false
		}
		seen[tail] = true
	}
	return true
}

// pathTail returns the last n segments of path joined with "/", or the whole path
// when it is shorter. The separator is always "/" so a disambiguated name reads
// the same on every platform.
func pathTail(path string, n int) string {
	segs := strings.Split(filepath.ToSlash(filepath.Clean(path)), "/")
	// Drop a leading "" from an absolute path so it never contributes a segment.
	if len(segs) > 0 && segs[0] == "" {
		segs = segs[1:]
	}
	if n >= len(segs) {
		return strings.Join(segs, "/")
	}
	return strings.Join(segs[len(segs)-n:], "/")
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
// pass: the committer time (unix seconds), the commit subject, and — when a
// base branch was supplied — how far the branch sits ahead of and behind that
// base.
type BranchTip struct {
	Time    int64  // committer time (unix seconds)
	Subject string // first line of the tip commit message
	// Ahead/Behind count the branch's divergence from the base branch passed to
	// BranchTips; HasBase gates whether that reading is present at all (false
	// when no base was supplied, or for the base branch itself measured against
	// itself is still reported but the caller ignores it for the main worktree).
	Ahead, Behind int
	HasBase       bool
}

// BranchTips returns the tip metadata of every local branch in r, keyed by
// short branch name — one subprocess for the whole repo, replacing a
// per-worktree `git log -1`. Worktree HEADs equal their branch tips in ct's
// branch-per-worktree model; detached worktrees simply miss the map (a zero
// BranchTip).
//
// When mainBranch is non-empty, the same for-each-ref folds in each branch's
// ahead/behind against it via the %(ahead-behind:<base>) atom (git 2.41+),
// computing every branch's divergence in one graph walk instead of a
// per-worktree `git rev-list`. aheadBehind reports whether that reading is
// present: it is false when no base was requested, or when the atom-bearing
// format failed (an older git, or a base ref that won't resolve) and BranchTips
// retried the plain three-field format so commit times and subjects still load.
// A false return with a non-empty mainBranch is the caller's cue to fall back
// to the per-worktree AheadBehind path, which works on any git.
func BranchTips(r Repo, mainBranch string) (tips map[string]BranchTip, aheadBehind bool, err error) {
	// %00 expands to a NUL in the output, an unambiguous separator no branch
	// name or subject can contain (a raw NUL can't be passed as an exec
	// argument). It beats spaces here because commit subjects — and the
	// ahead-behind atom's own "N M" pair — contain spaces freely.
	const baseFormat = "--format=%(refname:short)%00%(committerdate:unix)%00%(subject)"
	if mainBranch != "" {
		format := baseFormat + "%00%(ahead-behind:" + mainBranch + ")"
		out, ferr := Git(r.Path, "for-each-ref", format, "refs/heads")
		if ferr == nil {
			return parseBranchTips(out, true), true, nil
		}
		// Fall through to the base format on failure so tips still load; the
		// caller recomputes ahead/behind per worktree.
	}
	out, err := Git(r.Path, "for-each-ref", baseFormat, "refs/heads")
	if err != nil {
		return nil, false, err
	}
	return parseBranchTips(out, false), false, nil
}

// parseBranchTips parses the NUL-fenced for-each-ref output into the tip map.
// The first three fields are short name, committer unix time, and subject; a
// fourth field (present only when withAheadBehind is set) holds the
// %(ahead-behind:<base>) atom's "<ahead> <behind>" pair. Lines missing a
// required field or with an unparseable time are skipped; a branch whose
// ahead-behind pair is empty or malformed keeps its tip but reports no base.
func parseBranchTips(out string, withAheadBehind bool) map[string]BranchTip {
	tips := make(map[string]BranchTip)
	for _, line := range strings.Split(out, "\n") {
		fields := strings.SplitN(strings.TrimRight(line, "\r"), "\x00", 4)
		if len(fields) < 3 {
			continue
		}
		name, ts, subject := fields[0], fields[1], fields[2]
		t, err := strconv.ParseInt(strings.TrimSpace(ts), 10, 64)
		if err != nil {
			continue
		}
		tip := BranchTip{Time: t, Subject: subject}
		if withAheadBehind && len(fields) == 4 {
			if ahead, behind, ok := parseAheadBehindPair(fields[3]); ok {
				tip.Ahead, tip.Behind, tip.HasBase = ahead, behind, true
			}
		}
		tips[name] = tip
	}
	return tips
}

// parseAheadBehindPair parses the %(ahead-behind:<base>) atom's output — two
// space-separated integers, ahead then behind. ok is false when the field is
// empty (git omits it for a ref it cannot compare) or malformed.
func parseAheadBehindPair(s string) (ahead, behind int, ok bool) {
	fields := strings.Fields(s)
	if len(fields) != 2 {
		return 0, 0, false
	}
	a, err1 := strconv.Atoi(fields[0])
	b, err2 := strconv.Atoi(fields[1])
	if err1 != nil || err2 != nil {
		return 0, 0, false
	}
	return a, b, true
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

// diffBodyFlags pin the two git settings the unified-diff parser depends on.
// Both are user-configurable, both are reasonable things to have set, and both
// silently break the viewer rather than failing loudly:
//
//   - diff.external hands the patch off to an arbitrary program, whose output is
//     not a patch at all.
//   - color.ui or color.diff set to "always" prefixes every line with an escape
//     sequence even when writing to a pipe. The prefix tests that find `diff
//     --git` headers and +/- rows then stop matching, so the file-jump index
//     comes back empty — ] and [ quietly do nothing — and the styler cannot
//     colour a thing.
//
// The two are no longer equally non-negotiable. --no-ext-diff still is: nothing
// downstream can recover a patch from an external tool's output. --no-color can
// be deliberately overridden, because DiffOptions.Args land after these flags
// and a later git flag wins — `--color=always` is the supported way to feed a
// formatter that wants coloured input. That is safe only because the consumers
// of a coloured body strip ANSI before matching: diffpager's chunk splitter
// tests `diff --git` against ANSI-stripped text, so the file-jump index
// survives. ct's own built-in styler does not, which is why colouring the body
// is worth doing only alongside a configured pager.
//
// Only body-producing calls need them. --numstat is computed internally and is
// affected by neither, so it is left alone rather than carrying noise.
var diffBodyFlags = []string{"--no-ext-diff", "--no-color"}

// DiffOptions are the user's configurable git diff knobs, threaded in from
// config rather than read here so this package stays free of config. repo is
// the layer that talks to git and nothing else; making it import config would
// tie every git call — including the ones the deck refresh makes on a hot path
// — to a settings file it has no business knowing about, and would make these
// functions untestable without one.
//
// Args are extra flags placed after ct's pinned diffBodyFlags and before the
// revisions, so a user flag wins over a pinned one. They go on the --numstat
// calls too, not just the patch calls: the numstat feeds the file index and the
// +/− totals in the viewer's header, and a flag like -w or --histogram that
// changes the patch but not the counts would leave the header describing a diff
// the body no longer shows.
//
// Exclude are pathspec patterns, each appended as :(exclude)<pattern> after a
// `--`. They apply to the patch calls, the numstat calls, and the untracked-file
// listing alike, for the same reason: three views of one diff that disagree
// about which files are in it is worse than not supporting exclusions at all.
//
// The zero value is the "no configuration" case and must produce argv identical
// to what ct sent before these knobs existed — every existing call site passes
// through the same helpers, so a regression there would be silent.
type DiffOptions struct {
	Args    []string
	Exclude []string
}

// excludeArgs renders opts.Exclude as the pathspec tail git wants: a `--`
// separator followed by one :(exclude)<pattern> magic pathspec per entry. It
// returns nil for no exclusions so appending it is a no-op, which is what keeps
// a zero-value DiffOptions byte-identical to the pre-DiffOptions argv.
func (o DiffOptions) excludeArgs() []string {
	if len(o.Exclude) == 0 {
		return nil
	}
	out := make([]string, 0, len(o.Exclude)+1)
	out = append(out, "--")
	for _, p := range o.Exclude {
		out = append(out, ":(exclude)"+p)
	}
	return out
}

// gitDiffBody runs a patch-producing git subcommand — diff or show, both of
// which accept diff options — with diffBodyFlags inserted directly after the
// subcommand, where git expects them, then the user's opts.Args (so they can
// override a pinned flag), then the caller's revisions, then the exclusions as
// a trailing pathspec.
func gitDiffBody(dir string, okExit int, opts DiffOptions, sub string, args ...string) (string, error) {
	full := make([]string, 0, 1+len(diffBodyFlags)+len(opts.Args)+len(args)+len(opts.Exclude)+1)
	full = append(full, sub)
	full = append(full, diffBodyFlags...)
	full = append(full, opts.Args...)
	full = append(full, args...)
	full = append(full, opts.excludeArgs()...)
	return git(dir, okExit, full...)
}

// gitNumstat runs a --numstat-producing git subcommand with the same option
// placement gitDiffBody uses, minus diffBodyFlags (numstat output is neither
// coloured nor routed through diff.external). Sharing the shape with the body
// call is the point: the two must select the same files.
func gitNumstat(dir string, opts DiffOptions, sub string, args ...string) (string, error) {
	full := make([]string, 0, 2+len(opts.Args)+len(args)+len(opts.Exclude)+1)
	full = append(full, sub, "--numstat")
	full = append(full, opts.Args...)
	full = append(full, args...)
	full = append(full, opts.excludeArgs()...)
	return Git(dir, full...)
}

// DiffAgainstBase returns the unified diff of everything the worktree's branch
// carries beyond base, via `git diff <base>...HEAD` (three-dot: the branch tip
// against its merge-base with base, so unrelated commits base landed since the
// fork don't show up as reverse changes). An empty base — no primary-worktree
// branch to compare against — yields an empty diff and no error, so the caller
// simply omits the section.
func DiffAgainstBase(wt Worktree, base string, opts DiffOptions) (string, error) {
	if base == "" {
		return "", nil
	}
	return gitDiffBody(wt.Path, -1, opts, "diff", base+"...HEAD")
}

// DiffUncommitted returns the unified diff of the worktree's uncommitted work —
// staged and unstaged together — via `git diff HEAD`. Untracked files are not
// included (git diff never shows them); UntrackedFiles lists those separately.
func DiffUncommitted(wt Worktree, opts DiffOptions) (string, error) {
	return gitDiffBody(wt.Path, -1, opts, "diff", "HEAD")
}

// NumstatAgainstBase returns the per-file change summary of everything the
// branch carries beyond base, parsed from `git diff --numstat <base>...HEAD`. An
// empty base yields no files and no error, mirroring DiffAgainstBase.
func NumstatAgainstBase(wt Worktree, base string, opts DiffOptions) ([]FileStat, error) {
	if base == "" {
		return nil, nil
	}
	out, err := gitNumstat(wt.Path, opts, "diff", base+"...HEAD")
	if err != nil {
		return nil, err
	}
	return parseNumstat(out), nil
}

// NumstatUncommitted returns the per-file change summary of the worktree's
// uncommitted work (staged+unstaged vs HEAD), parsed from `git diff --numstat
// HEAD`.
func NumstatUncommitted(wt Worktree, opts DiffOptions) ([]FileStat, error) {
	out, err := gitNumstat(wt.Path, opts, "diff", "HEAD")
	if err != nil {
		return nil, err
	}
	return parseNumstat(out), nil
}

// DiffCommit returns the unified diff a single commit introduces, via `git diff
// sha^ sha` (the commit against its parent). A root commit has no parent, so on
// the sha^ lookup failing it falls back to `git show --format= sha`, which diffs
// the root against the empty tree. dir is any path inside the worktree the
// commit lives in.
//
// No --binary: this output is for reading, not applying. With it git inlines a
// base85 "GIT binary patch" literal for every changed binary file — hundreds of
// lines for a small blob, enough to blow past the viewer's line cap for a large
// one — and the base85 alphabet includes +/-, so those lines colour as bogus
// additions and deletions. Plain diff prints one "Binary files differ" line
// instead, matching DiffAgainstBase.
func DiffCommit(dir, sha string, opts DiffOptions) (string, error) {
	if _, err := Git(dir, "rev-parse", "--verify", "--quiet", sha+"^"); err != nil {
		return gitDiffBody(dir, -1, opts, "show", "--format=", sha)
	}
	return gitDiffBody(dir, -1, opts, "diff", sha+"^", sha)
}

// NumstatCommit returns a single commit's per-file change summary, parsed from
// `git diff --numstat sha^ sha`, mirroring DiffCommit's root-commit fallback to
// `git show --numstat --format= sha`.
func NumstatCommit(dir, sha string, opts DiffOptions) ([]FileStat, error) {
	if _, err := Git(dir, "rev-parse", "--verify", "--quiet", sha+"^"); err != nil {
		out, err := gitNumstat(dir, opts, "show", "--format=", sha)
		if err != nil {
			return nil, err
		}
		return parseNumstat(out), nil
	}
	out, err := gitNumstat(dir, opts, "diff", sha+"^", sha)
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
// of `git status --porcelain`. They carry no diff body of their own (git can't
// diff a file it isn't tracking), so the diff viewer lists them in its index and
// DiffUntracked renders their contents.
//
// -z is load-bearing, not a micro-optimisation. Without it git quotes any path
// that isn't plain ASCII — `?? "spaced name.txt"`, and non-ASCII bytes escaped
// octally as `"unicod\303\251.txt"` — which the viewer would print verbatim and
// DiffUntracked could not open. -z turns quoting off and NUL-fences the records
// instead, so paths come back exactly as they sit on disk.
//
// opts.Exclude is handed to git as a pathspec rather than filtered out of the
// returned paths in Go: git's own matcher then decides, so an exclusion means
// exactly the same thing here as it does on the patch and numstat calls —
// including all of pathspec's magic (globs, :(icase), leading-directory
// matching) — for free and without a second implementation to keep in step.
// opts.Args are not passed: `git status` takes no diff options and would reject
// them.
func UntrackedFiles(wt Worktree, opts DiffOptions) ([]string, error) {
	args := append([]string{"status", "--porcelain", "-z"}, opts.excludeArgs()...)
	out, err := Git(wt.Path, args...)
	if err != nil {
		return nil, err
	}
	return parseUntracked(out), nil
}

// DiffUntracked returns a unified diff of the contents of paths — untracked
// files, as returned by UntrackedFiles — concatenated into one body the diff
// styler can consume unchanged. Each file is diffed against os.DevNull with
// `git diff --no-index`, which renders it as a whole-file addition under a
// normal `diff --git` header, so file-jumping and +/− colouring work on new
// files exactly as they do on tracked ones. Binary files collapse to git's
// one-line "Binary files … differ" rather than dumping their contents.
//
// The caller decides how many paths are worth rendering; each one costs a git
// subprocess, so this walks whatever slice it is given and no more.
//
// A path that cannot be read is skipped rather than failing the whole body: an
// untracked file is by definition not under git's control and can vanish or be
// unreadable between the status call that listed it and this one, and losing
// every other file's diff to that race would be a poor trade.
//
// opts.Args apply (they shape the patch the same way they do everywhere else),
// but opts.Exclude deliberately does not: paths comes from UntrackedFiles, which
// already applied the exclusions, and a second `--` pathspec would collide with
// --no-index's two operands — git reads them positionally, so an extra pathspec
// tail is either a hard error or silently diffs the wrong pair.
func DiffUntracked(dir string, paths []string, opts DiffOptions) (string, error) {
	var b strings.Builder
	// Only the args travel; see the note above on why the exclusions must not.
	argsOnly := DiffOptions{Args: opts.Args}
	for _, p := range paths {
		// --no-index makes git compare two filesystem paths; exit 1 is its
		// "these differ" signal, which is true of every file here.
		out, err := gitDiffBody(dir, 1, argsOnly, "diff", "--no-index", "--", os.DevNull, p)
		if err != nil {
			continue
		}
		b.WriteString(out)
	}
	return b.String(), nil
}

// parseUntracked pulls the untracked-file paths ("?? path" records) out of `git
// status --porcelain -z` output. Records are NUL-fenced rather than newline
// separated, which is what lets a path contain spaces — or a newline — without
// git having to quote it. Every other status code (tracked modifications, staged
// changes) is ignored; those show up in the diff body instead.
func parseUntracked(out string) []string {
	var paths []string
	for _, rec := range strings.Split(out, "\x00") {
		if strings.HasPrefix(rec, "?? ") {
			paths = append(paths, strings.TrimPrefix(rec, "?? "))
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
// branch, based on baseRef. If baseRef is empty and the repository has an origin,
// the primary worktree's branch is fetched and origin/<branch> is used. Repos
// without an origin retain the local-only behavior of branching from HEAD.
// Returns the created Worktree.
func CreateWorktree(r Repo, relPath, branch, baseRef string) (Worktree, error) {
	if baseRef == "" {
		if _, err := Git(r.Path, "remote", "get-url", "origin"); err == nil {
			wts, err := ListWorktrees(r)
			if err != nil {
				return Worktree{}, fmt.Errorf("finding primary branch: %w", err)
			}
			if len(wts) == 0 || wts[0].Branch == "" {
				return Worktree{}, fmt.Errorf("cannot fetch base for a detached primary worktree")
			}
			mainBranch := wts[0].Branch
			if _, err := Git(r.Path, "fetch", "origin", mainBranch); err != nil {
				return Worktree{}, fmt.Errorf("fetching origin/%s: %w", mainBranch, err)
			}
			baseRef = "origin/" + mainBranch
		}
	}

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
	return git(dir, -1, args...)
}

// git runs a git command in dir, treating okExit as success in addition to 0.
// Pass -1 to accept only 0. `diff --no-index` needs the escape hatch: it follows
// diff(1) and reports "the inputs differ" as exit 1, which for a viewer is the
// ordinary case rather than a failure, and its output is on stdout as usual.
func git(dir string, okExit int, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), gitTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		// Only an ExitError carries a status worth forgiving; a timeout or a
		// missing binary still fails. ExitCode is -1 when the process was
		// signalled, which is why okExit uses -1 to mean "forgive nothing".
		var ee *exec.ExitError
		if okExit >= 0 && errors.As(err, &ee) && ee.ExitCode() == okExit {
			return stdout.String(), nil
		}
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("git %s: %s", strings.Join(args, " "), msg)
	}
	return stdout.String(), nil
}
