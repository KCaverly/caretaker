package stack

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// prRecord is a PR reduced to what the reconciler needs. It differs from the
// public PR type by carrying the head ref and raw state — fields used to match
// PRs to commits and to classify them — that the JSON output does not expose.
type prRecord struct {
	Number   int
	URL      string
	Title    string
	State    string // OPEN, CLOSED, MERGED
	Draft    bool
	Head     string // headRefName
	Base     string // baseRefName
	Review   string // reviewDecision
	MergedAt string // "" when never merged
	Checks   Checks
}

// ghPR mirrors one element of `gh pr list --json …`. statusCheckRollup is a
// heterogeneous array (CheckRun objects and StatusContext objects), so ghCheck
// captures the fields of both shapes and summarizeChecks reconciles them.
type ghPR struct {
	Number            int       `json:"number"`
	URL               string    `json:"url"`
	Title             string    `json:"title"`
	State             string    `json:"state"`
	IsDraft           bool      `json:"isDraft"`
	HeadRefName       string    `json:"headRefName"`
	BaseRefName       string    `json:"baseRefName"`
	ReviewDecision    string    `json:"reviewDecision"`
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
		"--json", "number,url,title,state,isDraft,headRefName,baseRefName,reviewDecision,statusCheckRollup,mergedAt")
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
			Number:   p.Number,
			URL:      p.URL,
			Title:    p.Title,
			State:    p.State,
			Draft:    p.IsDraft,
			Head:     p.HeadRefName,
			Base:     p.BaseRefName,
			Review:   p.ReviewDecision,
			MergedAt: p.MergedAt,
			Checks:   summarizeChecks(p.StatusCheckRollup),
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
