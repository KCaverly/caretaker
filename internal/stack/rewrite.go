package stack

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/KCaverly/caretaker/internal/repo"
)

// idAssignment records a ct-stack-id newly minted for a previously-unsubmitted
// commit, bottom-first, for reporting what a submit assigned.
type idAssignment struct {
	Position int
	ShortSHA string
	Subject  string
	ID       string
}

// newStackID returns a fresh 8-lowercase-hex stack id that is not already in
// use. crypto/rand makes collisions astronomically unlikely, but the existing
// set is still checked so an id minted earlier in the same submit can never be
// reused. hex.EncodeToString emits lowercase, matching validStackID.
func newStackID(existing map[string]bool) (string, error) {
	for attempt := 0; attempt < 100; attempt++ {
		var buf [4]byte
		if _, err := rand.Read(buf[:]); err != nil {
			return "", fmt.Errorf("generating stack id: %w", err)
		}
		id := hex.EncodeToString(buf[:]) // exactly 8 lowercase hex chars
		if !existing[id] {
			return id, nil
		}
	}
	return "", fmt.Errorf("could not generate a unique stack id after 100 attempts")
}

// commitMeta is the original identity of a commit being rewritten: its author
// (preserved verbatim so a submit never rewrites authorship or author date) and
// its tree and raw message (so the rebuilt commit is byte-identical except for
// the appended trailer and the refreshed committer).
type commitMeta struct {
	authorName  string
	authorEmail string
	authorDate  string
	tree        string
	message     string // raw commit message, byte-exact
}

// injectTrailers ensures every commit in the stack carries a ct-stack-id
// trailer, rewriting history from the oldest commit that lacks one up to HEAD —
// without touching the working tree or the index. It rebuilds that suffix of the
// chain with plumbing (git commit-tree reusing each original tree), so the amend
// is invisible to `git status`: every rebuilt commit keeps its tree, and the
// branch ref is moved with a compare-and-swap update-ref that guards against a
// concurrent HEAD move.
//
// Commits that already carry a valid id keep their message byte-for-byte; the
// rest get a trailer appended via git interpret-trailers. Author identity and
// author date are preserved; the committer becomes the current user, exactly as
// an ordinary `git commit --amend` would do. It returns the ids assigned to the
// previously-unsubmitted commits (bottom-first), and is a no-op returning nil
// when every commit already has an id.
func injectTrailers(dir, branch string, commits []LocalCommit) ([]idAssignment, error) {
	existing := map[string]bool{}
	for _, c := range commits {
		if validStackID(c.StackID) {
			existing[c.StackID] = true
		}
	}

	start := -1
	for i, c := range commits {
		if !validStackID(c.StackID) {
			start = i
			break
		}
	}
	if start == -1 {
		return nil, nil
	}

	// The new parent of the rewrite point is the original parent of that commit;
	// commits below the rewrite point are untouched, so this ref is stable.
	parent, err := repo.Git(dir, "rev-parse", commits[start].SHA+"^")
	if err != nil {
		return nil, fmt.Errorf("resolving rewrite base: %w", err)
	}
	newParent := strings.TrimSpace(parent)
	oldHead := commits[len(commits)-1].SHA

	var assigns []idAssignment
	for i := start; i < len(commits); i++ {
		c := commits[i]
		meta, err := readCommitMeta(dir, c.SHA)
		if err != nil {
			return assigns, err
		}
		msg := meta.message
		if !validStackID(c.StackID) {
			id, err := newStackID(existing)
			if err != nil {
				return assigns, err
			}
			existing[id] = true
			msg, err = appendTrailer(dir, meta.message, id)
			if err != nil {
				return assigns, err
			}
			assigns = append(assigns, idAssignment{
				Position: i + 1, ShortSHA: c.ShortSHA, Subject: c.Subject, ID: id,
			})
		}
		newSHA, err := commitTree(dir, meta, newParent, msg)
		if err != nil {
			return assigns, err
		}
		newParent = newSHA
	}

	// Compare-and-swap the branch ref: the old-value check turns a concurrent
	// commit on this worktree into a clean failure rather than a lost update.
	if _, err := repo.Git(dir, "update-ref", "refs/heads/"+branch, newParent, oldHead); err != nil {
		return assigns, fmt.Errorf("moving branch %s: %w", branch, err)
	}
	return assigns, nil
}

// readCommitMeta reads a commit's author identity, tree, and exact raw message.
// The message comes from `git cat-file commit` (everything after the header's
// blank line) rather than a --format placeholder, because only the raw object
// bytes round-trip through commit-tree unchanged.
func readCommitMeta(dir, sha string) (commitMeta, error) {
	out, err := repo.Git(dir, "show", "-s", "--format=%an%x00%ae%x00%aI", sha)
	if err != nil {
		return commitMeta{}, err
	}
	fields := strings.SplitN(strings.TrimRight(out, "\n"), "\x00", 3)
	if len(fields) < 3 {
		return commitMeta{}, fmt.Errorf("unexpected author format for %s", sha)
	}

	tree, err := repo.Git(dir, "rev-parse", sha+"^{tree}")
	if err != nil {
		return commitMeta{}, err
	}

	raw, err := repo.Git(dir, "cat-file", "commit", sha)
	if err != nil {
		return commitMeta{}, err
	}
	idx := strings.Index(raw, "\n\n")
	if idx < 0 {
		return commitMeta{}, fmt.Errorf("could not find message body of %s", sha)
	}

	return commitMeta{
		authorName:  fields[0],
		authorEmail: fields[1],
		authorDate:  fields[2],
		tree:        strings.TrimSpace(tree),
		message:     raw[idx+2:],
	}, nil
}

// appendTrailer runs git interpret-trailers to add a ct-stack-id trailer to a
// raw commit message, returning the new message. The message is passed via a
// temp file so its exact bytes (blank lines, existing trailers) are preserved.
func appendTrailer(dir, message, id string) (string, error) {
	f, err := os.CreateTemp("", "ct-msg-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(f.Name())
	if _, err := f.WriteString(message); err != nil {
		f.Close()
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	out, err := repo.Git(dir, "interpret-trailers", "--trailer", "ct-stack-id: "+id, f.Name())
	if err != nil {
		return "", err
	}
	return out, nil
}

// commitTree rebuilds a single commit: it reuses the original tree and author
// identity/date (via GIT_AUTHOR_* in the environment) and takes the given parent
// and message, letting git default the committer to the current user. The
// message goes through a temp file (-F) so no shell quoting can corrupt it.
func commitTree(dir string, meta commitMeta, parent, message string) (string, error) {
	f, err := os.CreateTemp("", "ct-commit-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(f.Name())
	if _, err := f.WriteString(message); err != nil {
		f.Close()
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", err
	}

	env := []string{
		"GIT_AUTHOR_NAME=" + meta.authorName,
		"GIT_AUTHOR_EMAIL=" + meta.authorEmail,
		"GIT_AUTHOR_DATE=" + meta.authorDate,
	}
	out, err := gitEnv(dir, env, "commit-tree", meta.tree, "-p", parent, "-F", f.Name())
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// gitEnv is repo.Git's twin for the one place that needs extra environment (the
// GIT_AUTHOR_* overrides commit-tree reads): same dir, same 30s timeout, same
// stderr-wrapping contract, but with variables appended to the process env.
func gitEnv(dir string, extraEnv []string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), extraEnv...)
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
