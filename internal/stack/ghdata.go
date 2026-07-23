package stack

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// prRecord is a PR reduced to what the reconciler needs. It differs from the
// public PR type by carrying the head ref and raw state — fields used to match
// PRs to commits and to classify them — that the JSON output does not expose.
type prRecord struct {
	Number    int
	URL       string
	Title     string
	Body      string // PR body, needed to splice the nav table on submit
	State     string // OPEN, CLOSED, MERGED
	Draft     bool
	Head      string // headRefName
	HeadSHA   string // headRefOid; survives branch deletion on GitHub
	Base      string // baseRefName
	Review    string // reviewDecision
	Mergeable string // MERGEABLE, CONFLICTING, UNKNOWN
	MergedAt  string // "" when never merged
	Checks    Checks
}

// ghPR mirrors one element of `gh pr list --json …`. statusCheckRollup is a
// heterogeneous array (CheckRun objects and StatusContext objects), so ghCheck
// captures the fields of both shapes and summarizeChecks reconciles them.
type ghPR struct {
	Number            int       `json:"number"`
	URL               string    `json:"url"`
	Title             string    `json:"title"`
	Body              string    `json:"body"`
	State             string    `json:"state"`
	IsDraft           bool      `json:"isDraft"`
	HeadRefName       string    `json:"headRefName"`
	HeadRefOid        string    `json:"headRefOid"`
	BaseRefName       string    `json:"baseRefName"`
	ReviewDecision    string    `json:"reviewDecision"`
	Mergeable         string    `json:"mergeable"`
	StatusCheckRollup []ghCheck `json:"statusCheckRollup"`
	MergedAt          string    `json:"mergedAt"`
}

// ghCheck is the union of the two node shapes GitHub returns in
// statusCheckRollup: a CheckRun (Name + Status + Conclusion) or a legacy
// StatusContext (Context + State).
type ghCheck struct {
	Name       string `json:"name"`       // CheckRun
	Status     string `json:"status"`     // CheckRun: QUEUED, IN_PROGRESS, COMPLETED
	Conclusion string `json:"conclusion"` // CheckRun: SUCCESS, FAILURE, …
	Context    string `json:"context"`    // StatusContext
	State      string `json:"state"`      // StatusContext: SUCCESS, PENDING, FAILURE, ERROR
}

// ghTimeout bounds the gh subprocess for the same reason repo.Git bounds git: a
// credential helper or a stalled API call must fail visibly rather than hang.
const ghTimeout = 30 * time.Second

// gatherGitHub lists this worktree's stack PRs via the gh CLI and returns them
// as prRecords alongside a GitHub availability report. Any problem — gh not
// installed, a non-zero exit (which includes the unauthenticated case), or
// undecodable output — is reported as available=false with a warning and an
// empty slice, never a hard error: the caller must still be able to render the
// local stack shape offline.
func gatherGitHub(dir, worktree string) ([]prRecord, GitHub) {
	gh := GitHub{Available: false, Warnings: []string{}}

	if _, err := exec.LookPath("gh"); err != nil {
		gh.Warnings = append(gh.Warnings, "gh CLI not found on PATH; GitHub PR status unavailable")
		return nil, gh
	}

	out, err := runGH(dir,
		"pr", "list", "--state", "all", "--limit", "200",
		"--json", "number,url,title,body,state,isDraft,headRefName,headRefOid,baseRefName,reviewDecision,mergeable,statusCheckRollup,mergedAt")
	if err != nil {
		gh.Warnings = append(gh.Warnings, "gh pr list failed (missing auth or repo?): "+err.Error())
		return nil, gh
	}

	prs, err := decodeGHPRs([]byte(out))
	if err != nil {
		gh.Warnings = append(gh.Warnings, "could not decode gh output: "+err.Error())
		return nil, gh
	}

	gh.Available = true
	return filterStackPRs(prs, worktree), gh
}

// filterStackPRs keeps only PRs whose head branch is under this worktree's
// ct/<worktree>/ namespace and collapses each into a prRecord, summarizing its
// check rollup. Filtering client-side keeps the gh query simple and avoids
// depending on server-side head filtering.
func filterStackPRs(prs []ghPR, worktree string) []prRecord {
	prefix := "ct/" + worktree + "/"
	var records []prRecord
	for _, p := range prs {
		if !strings.HasPrefix(p.HeadRefName, prefix) {
			continue
		}
		records = append(records, prRecord{
			Number:    p.Number,
			URL:       p.URL,
			Title:     p.Title,
			Body:      p.Body,
			State:     p.State,
			Draft:     p.IsDraft,
			Head:      p.HeadRefName,
			HeadSHA:   p.HeadRefOid,
			Base:      p.BaseRefName,
			Review:    p.ReviewDecision,
			Mergeable: p.Mergeable,
			MergedAt:  p.MergedAt,
			Checks:    summarizeChecks(p.StatusCheckRollup),
		})
	}
	return records
}

// decodeGHPRs unmarshals the `gh pr list --json` array. A separate function so
// the JSON contract with gh is unit-testable against a fixture.
func decodeGHPRs(data []byte) ([]ghPR, error) {
	var prs []ghPR
	if err := json.Unmarshal(data, &prs); err != nil {
		return nil, err
	}
	return prs, nil
}

// summarizeChecks collapses a PR's heterogeneous check rollup into a single
// summary word plus the names of failing checks. Precedence is failing > pending
// > passing: one red check makes the whole rollup "failing" (so the next-action
// engine says fix-ci before it says wait), and an empty rollup is "none".
func summarizeChecks(checks []ghCheck) Checks {
	c := Checks{Summary: "none", Failing: []string{}}
	if len(checks) == 0 {
		return c
	}

	anyPending, anyFailing := false, false
	for _, ck := range checks {
		switch classifyCheck(ck) {
		case "failing":
			anyFailing = true
			c.Failing = append(c.Failing, checkName(ck))
		case "pending":
			anyPending = true
		}
	}

	switch {
	case anyFailing:
		c.Summary = "failing"
	case anyPending:
		c.Summary = "pending"
	default:
		c.Summary = "passing"
	}
	return c
}

// classifyCheck maps one check node to "passing", "failing", or "pending",
// handling both the CheckRun shape (status/conclusion) and the StatusContext
// shape (state). Unknown values are treated as pending — the conservative choice
// that keeps the engine from prompting a merge on a check it doesn't understand.
func classifyCheck(ck ghCheck) string {
	// CheckRun: not COMPLETED means still running/queued.
	if ck.Status != "" && ck.Status != "COMPLETED" {
		return "pending"
	}
	if ck.Conclusion != "" {
		switch ck.Conclusion {
		case "SUCCESS", "NEUTRAL", "SKIPPED":
			return "passing"
		case "FAILURE", "TIMED_OUT", "CANCELLED", "ACTION_REQUIRED", "STARTUP_FAILURE", "STALE":
			return "failing"
		default:
			return "pending"
		}
	}
	// StatusContext shape.
	switch ck.State {
	case "SUCCESS":
		return "passing"
	case "FAILURE", "ERROR":
		return "failing"
	case "PENDING", "EXPECTED", "":
		return "pending"
	default:
		return "pending"
	}
}

// checkName returns the display name of a check, preferring the CheckRun Name and
// falling back to the StatusContext Context (or a placeholder when both empty).
func checkName(ck ghCheck) string {
	switch {
	case ck.Name != "":
		return ck.Name
	case ck.Context != "":
		return ck.Context
	default:
		return "(unnamed check)"
	}
}

// requireGH is submit's hard precondition: gh must be on PATH. Status soft-fails
// when gh is missing, but submit cannot open or edit PRs without it, so it fails
// early with a clear message. (Authentication is verified implicitly by the
// status gather, which reports github.available=false when gh is unauthed.)
func requireGH() error {
	if _, err := exec.LookPath("gh"); err != nil {
		return fmt.Errorf("gh CLI not found on PATH; stack submit needs GitHub access")
	}
	return nil
}

// ghCreateArgs builds the argv for creating a stack PR: it heads the given
// branch, bases on the previous commit's branch (or main for the bottom), and
// carries the title and full body. --draft is appended when requested. Kept pure
// and separate from the runner so the exact argv is unit-testable without ever
// invoking gh.
func ghCreateArgs(head, base, title, body string, draft bool) []string {
	args := []string{"pr", "create", "--head", head, "--base", base, "--title", title, "--body", body}
	if draft {
		args = append(args, "--draft")
	}
	return args
}

// ghEditBaseArgs builds the argv to retarget a PR onto a new base branch.
func ghEditBaseArgs(number int, base string) []string {
	return []string{"pr", "edit", strconv.Itoa(number), "--base", base}
}

// ghEditTitleArgs builds the argv to update a PR's title after a commit subject
// changed.
func ghEditTitleArgs(number int, title string) []string {
	return []string{"pr", "edit", strconv.Itoa(number), "--title", title}
}

// ghEditBodyArgs builds the argv to replace a PR's body (used for the nav-table
// splice).
func ghEditBodyArgs(number int, body string) []string {
	return []string{"pr", "edit", strconv.Itoa(number), "--body", body}
}

// ghCreatePR creates a PR via the gh CLI. Mutating: never called under
// --dry-run.
func ghCreatePR(dir, head, base, title, body string, draft bool) error {
	_, err := runGH(dir, ghCreateArgs(head, base, title, body, draft)...)
	return err
}

// ghEditBase retargets a PR's base branch. Mutating.
func ghEditBase(dir string, number int, base string) error {
	_, err := runGH(dir, ghEditBaseArgs(number, base)...)
	return err
}

// ghEditTitle updates a PR's title. Mutating.
func ghEditTitle(dir string, number int, title string) error {
	_, err := runGH(dir, ghEditTitleArgs(number, title)...)
	return err
}

// ghEditBody replaces a PR's body. Mutating.
func ghEditBody(dir string, number int, body string) error {
	_, err := runGH(dir, ghEditBodyArgs(number, body)...)
	return err
}

// ensureBranchHasNoOpenDependents is the final guard before deleting a remote
// stack branch. The check is intentionally fresh rather than derived from an
// earlier StackStatus: another actor may have retargeted or opened a PR while a
// multi-step submit/restack operation was running.
func ensureBranchHasNoOpenDependents(dir, worktree, branch string) error {
	prs, gh := gatherGitHub(dir, worktree)
	if !gh.Available {
		return fmt.Errorf("refusing to delete %s while GitHub state is unavailable: %s", branch, strings.Join(gh.Warnings, "; "))
	}
	if dependents := openPRsBasedOn(prs, branch); len(dependents) > 0 {
		return fmt.Errorf("refusing to delete %s: open PR #%d still targets it", branch, dependents[0].Number)
	}
	return nil
}

func openPRsBasedOn(prs []prRecord, branch string) []prRecord {
	var out []prRecord
	for _, p := range prs {
		if p.State == "OPEN" && p.Base == branch {
			out = append(out, p)
		}
	}
	return out
}

// runGH runs a gh command in dir with the same 30s-timeout, stderr-wrapping
// contract as repo.Git. It is a local twin rather than a reuse of repo.Git
// because that runner is hard-wired to the git binary.
func runGH(dir string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), ghTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "gh", args...)
	cmd.Dir = dir
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("gh %s: %s", strings.Join(args, " "), msg)
	}
	return stdout.String(), nil
}
