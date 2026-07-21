package stack

import (
	"strings"
	"time"

	"github.com/KCaverly/caretaker/internal/repo"
)

// LocalCommit is one commit in <main>..HEAD as read from git, before any
// reconciliation against remotes or PRs. StackID is the raw ct-stack-id trailer
// value: "" when the commit has no such trailer, and possibly malformed (see
// parseGitLog) when a commit carries more than one.
type LocalCommit struct {
	SHA      string
	ShortSHA string
	Subject  string
	StackID  string
}

// localCommits reads the stack's commits bottom-first in a single subprocess.
// --reverse makes git emit oldest-first, so index 0 is the bottom of the stack,
// matching the 1-based Position in the JSON output.
func localCommits(dir, mainBranch string) ([]LocalCommit, error) {
	// Four NUL-fenced fields per commit: full SHA, short SHA, subject, and the
	// ct-stack-id trailer value. A NUL beats any other separator because commit
	// subjects contain spaces (and almost anything else) freely — the same trick
	// repo.parseBranchTips uses.
	out, err := repo.Git(dir, "log", "--reverse",
		"--format=%H%x00%h%x00%s%x00%(trailers:key=ct-stack-id,valueonly,separator=,)",
		mainBranch+"..HEAD")
	if err != nil {
		return nil, err
	}
	return parseGitLog(out), nil
}

// parseGitLog parses the NUL-fenced `git log` output into bottom-first commits.
// Lines with fewer than four fields are skipped (defensive; the format always
// emits four). The trailer field is passed through verbatim — the reconciler
// decides whether it's a clean id, so this stays a pure structural parse.
//
// Note on the trailer separator: git parses `separator=,` by consuming the
// comma as the option delimiter, leaving an *empty* separator. A commit with two
// ct-stack-id trailers therefore emits their values concatenated with no comma
// (e.g. "aaaaaaaabbbbbbbb"), not "aaaaaaaa,bbbbbbbb". The reconciler treats any
// value that isn't a single clean 8-hex id as malformed, which catches both that
// concatenation and a literal comma, so the exact separator behaviour doesn't
// matter to correctness.
func parseGitLog(out string) []LocalCommit {
	var commits []LocalCommit
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			continue
		}
		fields := strings.SplitN(line, "\x00", 4)
		if len(fields) < 4 {
			continue
		}
		commits = append(commits, LocalCommit{
			SHA:      fields[0],
			ShortSHA: fields[1],
			Subject:  fields[2],
			StackID:  strings.TrimSpace(fields[3]),
		})
	}
	return commits
}

// remoteBranches reads the last-fetched stack branches for this worktree — no
// network — returning a map of stack id -> branch tip SHA. The path prefix
// restricts for-each-ref to refs/remotes/origin/ct/<worktree>/, so unrelated
// remote branches never enter the map.
func remoteBranches(dir, worktree string) (map[string]string, error) {
	prefix := "refs/remotes/origin/ct/" + worktree + "/"
	out, err := repo.Git(dir, "for-each-ref",
		"--format=%(refname:short)%00%(objectname)", prefix)
	if err != nil {
		return nil, err
	}
	return parseRemoteBranches(out), nil
}

// parseRemoteBranches parses the NUL-fenced for-each-ref output (short refname,
// object SHA) into a stack id -> SHA map. The id is the last path segment of the
// short refname (origin/ct/<worktree>/<id>), so it survives worktree names that
// themselves contain slashes. Malformed lines are skipped.
func parseRemoteBranches(out string) map[string]string {
	refs := make(map[string]string)
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimRight(line, "\r")
		fields := strings.SplitN(line, "\x00", 2)
		if len(fields) < 2 {
			continue
		}
		name, sha := fields[0], fields[1]
		if name == "" || sha == "" {
			continue
		}
		id := name[strings.LastIndex(name, "/")+1:]
		refs[id] = sha
	}
	return refs
}

// fetchOrigin refreshes remote refs so remoteBranches and the PR data reflect
// the true remote state. It is only run behind the --fetch flag because it is
// the one network call in the whole engine.
func fetchOrigin(dir string) error {
	_, err := repo.Git(dir, "fetch", "origin")
	return err
}

// Status computes the full read-only StackStatus for a worktree. It runs the
// gather steps (each a subprocess) and then hands their plain-data results to
// the pure reconciler. A GitHub failure is soft: the status is returned with
// github.available=false and warnings, so the local stack shape still renders.
// Only a failure to read local git state (the log or the refs) returns an error.
func Status(p Params) (StackStatus, error) {
	st, _, err := gatherStatus(p)
	return st, err
}

// gatherStatus is the shared engine behind Status and Submit: it runs the gather
// steps and reconciles them, returning both the public StackStatus and the raw
// prRecords (which carry PR titles and bodies the public view omits, and which
// Submit's planner needs to decide retitles and nav-body splices).
func gatherStatus(p Params) (StackStatus, []prRecord, error) {
	st := StackStatus{
		Schema:      1,
		GeneratedAt: time.Now().UTC().Format(time.RFC3339),
		Repo:        p.RepoName,
		Worktree:    p.WorktreeName,
		Branch:      p.Branch,
		MainBranch:  p.MainBranch,
	}

	if p.Fetch {
		// A fetch failure (offline, auth) is non-fatal: fetched stays false and
		// the last-fetched refs are used, exactly as if --fetch were absent.
		if err := fetchOrigin(p.WorktreeDir); err == nil {
			st.Fetched = true
		}
	}

	commits, err := localCommits(p.WorktreeDir, p.MainBranch)
	if err != nil {
		return st, nil, err
	}
	remotes, err := remoteBranches(p.WorktreeDir, p.WorktreeName)
	if err != nil {
		return st, nil, err
	}
	prs, gh := gatherGitHub(p.WorktreeDir, p.WorktreeName)
	st.GitHub = gh

	st.Stack, st.Commits = reconcile(p.WorktreeName, p.MainBranch, commits, remotes, prs)
	return st, prs, nil
}
