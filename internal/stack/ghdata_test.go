package stack

import (
	"errors"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// ghFixture is realistic `gh api graphql` output for the batched head-ref query:
// one aliased field per branch. r0 is an open PR with a mixed check rollup, r1 a
// merged one whose rollup came back null (GitHub attaches none when nothing ran),
// and r2 a branch with no PR at all. A stray field carrying a PR outside the
// ct/wt/ namespace stands in for a widened scope and must be filtered out. The
// rollup mixes the CheckRun node shape (status/conclusion) with the legacy
// StatusContext shape (state) so summarizeChecks is exercised on both.
const ghFixture = `{"data":{"repository":{
  "r0": {"nodes": [{
    "number": 10,
    "url": "https://github.com/acme/repo/pull/10",
    "title": "bottom",
    "body": "the bottom PR",
    "state": "OPEN",
    "isDraft": false,
    "headRefName": "ct/wt/aaaaaaaa",
    "headRefOid": "aaaa111",
    "baseRefName": "main",
    "reviewDecision": "APPROVED",
    "mergeable": "CONFLICTING",
    "mergedAt": null,
    "commits": {"nodes": [{"commit": {"statusCheckRollup": {"contexts": {"nodes": [
      {"__typename": "CheckRun", "name": "build", "status": "COMPLETED", "conclusion": "SUCCESS"},
      {"__typename": "CheckRun", "name": "lint", "status": "IN_PROGRESS", "conclusion": ""},
      {"__typename": "StatusContext", "context": "ci/deploy", "state": "SUCCESS"}
    ]}}}}]}
  }]},
  "r1": {"nodes": [{
    "number": 9,
    "url": "https://github.com/acme/repo/pull/9",
    "title": "landed",
    "body": "the landed PR",
    "state": "MERGED",
    "isDraft": false,
    "headRefName": "ct/wt/bbbbbbbb",
    "headRefOid": "bbbb222",
    "baseRefName": "main",
    "reviewDecision": "APPROVED",
    "mergeable": "MERGEABLE",
    "mergedAt": "2026-07-10T12:00:00Z",
    "commits": {"nodes": [{"commit": {"statusCheckRollup": null}}]}
  }]},
  "r2": {"nodes": []},
  "r3": {"nodes": [{
    "number": 3,
    "url": "https://github.com/acme/repo/pull/3",
    "title": "unrelated feature branch",
    "body": "",
    "state": "OPEN",
    "isDraft": true,
    "headRefName": "feature/other",
    "baseRefName": "main",
    "reviewDecision": null,
    "mergedAt": null,
    "commits": {"nodes": []}
  }]}
}}}`

func TestDecodeAndFilterGHPRs(t *testing.T) {
	prs, err := decodeGHPRs([]byte(ghFixture))
	if err != nil {
		t.Fatalf("decodeGHPRs: %v", err)
	}
	if len(prs) != 3 {
		t.Fatalf("decoded %d PRs, want 3", len(prs))
	}
	// The aliases come back as a map, so the flattened list is ordered by number
	// descending, the way queryStackPRs leaves it.
	sortPRsForTest(prs)

	recs := filterStackPRs(prs, "wt")
	if len(recs) != 2 {
		t.Fatalf("filtered to %d records, want 2 (feature/other dropped)", len(recs))
	}
	if recs[1].Checks.Summary != "none" {
		t.Errorf("PR 9 has a null rollup, want summary none, got %q", recs[1].Checks.Summary)
	}
	if recs[0].Body != "the bottom PR" {
		t.Errorf("PR 10 body = %q", recs[0].Body)
	}

	// PR 10: open, bottom, mixed rollup -> pending (a check still in progress).
	if recs[0].Number != 10 || recs[0].State != "OPEN" || recs[0].Base != "main" {
		t.Errorf("record[0] = %+v", recs[0])
	}
	if recs[0].Checks.Summary != "pending" {
		t.Errorf("PR 10 checks summary = %q, want pending", recs[0].Checks.Summary)
	}
	if recs[0].Mergeable != "CONFLICTING" {
		t.Errorf("PR 10 mergeable = %q, want CONFLICTING", recs[0].Mergeable)
	}

	// PR 9: merged with a real timestamp.
	if recs[1].Number != 9 || recs[1].State != "MERGED" || recs[1].MergedAt != "2026-07-10T12:00:00Z" {
		t.Errorf("record[1] = %+v", recs[1])
	}
	if recs[1].Mergeable != "MERGEABLE" {
		t.Errorf("PR 9 mergeable = %q, want MERGEABLE", recs[1].Mergeable)
	}
}

func TestSummarizeChecks(t *testing.T) {
	cases := []struct {
		name        string
		checks      []ghCheck
		wantSummary string
		wantFailing []string
	}{
		{"empty rollup -> none", nil, "none", []string{}},
		{
			"all success -> passing",
			[]ghCheck{
				{Name: "build", Status: "COMPLETED", Conclusion: "SUCCESS"},
				{Context: "ci/deploy", State: "SUCCESS"},
			},
			"passing", []string{},
		},
		{
			"one in progress -> pending",
			[]ghCheck{
				{Name: "build", Status: "COMPLETED", Conclusion: "SUCCESS"},
				{Name: "test", Status: "QUEUED", Conclusion: ""},
			},
			"pending", []string{},
		},
		{
			"a failure wins over a pending -> failing",
			[]ghCheck{
				{Name: "build", Status: "IN_PROGRESS"},
				{Name: "test", Status: "COMPLETED", Conclusion: "FAILURE"},
				{Context: "ci/deploy", State: "ERROR"},
			},
			"failing", []string{"test", "ci/deploy"},
		},
		{
			"skipped and neutral count as passing",
			[]ghCheck{
				{Name: "opt", Status: "COMPLETED", Conclusion: "SKIPPED"},
				{Name: "info", Status: "COMPLETED", Conclusion: "NEUTRAL"},
			},
			"passing", []string{},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := summarizeChecks(tc.checks)
			if got.Summary != tc.wantSummary {
				t.Errorf("summary = %q, want %q", got.Summary, tc.wantSummary)
			}
			if !reflect.DeepEqual(got.Failing, tc.wantFailing) {
				t.Errorf("failing = %v, want %v", got.Failing, tc.wantFailing)
			}
		})
	}
}

// TestGatherGitHubUnavailable forces gh off the PATH and checks the soft-failure
// contract: no records, available=false, and a warning — never a hard error.
func TestGatherGitHubUnavailable(t *testing.T) {
	t.Setenv("PATH", "")
	recs, gh := gatherGitHubFor(t.TempDir(), "wt", []ghRef{{Branch: "ct/wt/aaaaaaaa", Tracked: true}})
	if len(recs) != 0 {
		t.Errorf("expected no records when gh is absent, got %d", len(recs))
	}
	if gh.Available {
		t.Error("github.available should be false when gh is missing")
	}
	if len(gh.Warnings) == 0 {
		t.Error("expected a warning explaining gh is unavailable")
	}
}

// TestGatherGitHubNoRefs is the zero-cost case: a stack with no branch it could
// own asks GitHub nothing at all, and the GitHub half of the status is vacuously
// complete rather than unavailable.
func TestGatherGitHubNoRefs(t *testing.T) {
	// PATH is emptied so any attempt to actually run gh would fail the assertion.
	t.Setenv("PATH", "")
	recs, gh := gatherGitHubFor(t.TempDir(), "wt", nil)
	if len(recs) != 0 {
		t.Errorf("expected no records, got %d", len(recs))
	}
	if gh.Available {
		t.Error("with gh missing the gather must still report unavailable")
	}
}

// TestHeadRefsFrom pins the query scope: one branch per validly-trailered local
// commit plus every fetched namespace branch, deduplicated, commits first.
func TestHeadRefsFrom(t *testing.T) {
	commits := []LocalCommit{
		{SHA: "1", StackID: "aaaaaaaa"},
		{SHA: "2", StackID: ""},          // unsubmitted: no branch yet
		{SHA: "3", StackID: "not-hex!!"}, // malformed: never becomes a branch
		{SHA: "4", StackID: "bbbbbbbb"},
	}
	remotes := map[string]string{
		"bbbbbbbb": "sha", // already covered by commit 4
		"cccccccc": "sha", // orphan: a branch whose commit is gone locally
	}
	got := headRefsFrom("wt", commits, remotes)
	want := []ghRef{
		{Branch: "ct/wt/aaaaaaaa", Tracked: true},
		{Branch: "ct/wt/bbbbbbbb", Tracked: true}, // a live commit wins over the remote ref
		{Branch: "ct/wt/cccccccc", Tracked: false},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("headRefsFrom = %v, want %v", got, want)
	}
}

// TestGHQueryDocument checks the shape the whole optimisation rests on: one
// aliased head-ref selection per branch in a single document, and no repository-
// wide pullRequests listing anywhere in it.
func TestGHQueryDocument(t *testing.T) {
	doc := ghQueryDocument([]ghRef{
		{Branch: "ct/wt/aaaaaaaa", Tracked: true},
		{Branch: "ct/wt/bbbbbbbb", Tracked: false},
	})
	for _, want := range []string{
		`r0:pullRequests(headRefName:"ct/wt/aaaaaaaa"`,
		`r1:pullRequests(headRefName:"ct/wt/bbbbbbbb"`,
		"fragment pr on PullRequest{",
		"statusCheckRollup",
	} {
		if !strings.Contains(doc, want) {
			t.Errorf("query document missing %q:\n%s", want, doc)
		}
	}
	// A selection without headRefName would be a repository-wide listing — the
	// exact thing that 504s on a large repo.
	if strings.Count(doc, "pullRequests(") != strings.Count(doc, "headRefName:") {
		t.Errorf("every pullRequests selection must be head-ref scoped:\n%s", doc)
	}
}

func TestGHQueryArgs(t *testing.T) {
	args := ghQueryArgs(ghRepo{Owner: "acme", Name: "repo"}, []ghRef{{Branch: "ct/wt/aaaaaaaa", Tracked: true}})
	if args[0] != "api" || args[1] != "graphql" {
		t.Errorf("args = %v, want an api graphql call", args[:2])
	}
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "owner=acme") || !strings.Contains(joined, "name=repo") {
		t.Errorf("args = %v, want owner/name variables", args)
	}
	// A GitHub Enterprise host has to be routed explicitly; github.com must not be.
	ghe := ghQueryArgs(ghRepo{Owner: "acme", Name: "repo", Host: "github.acme.dev"}, []ghRef{{Branch: "x"}})
	if !strings.Contains(strings.Join(ghe, " "), "--hostname github.acme.dev") {
		t.Errorf("GHE args = %v, want --hostname", ghe)
	}
	if strings.Contains(joined, "--hostname") {
		t.Errorf("github.com must not be routed with --hostname: %v", args)
	}
}

func TestParseRemoteURL(t *testing.T) {
	cases := []struct {
		raw   string
		want  ghRepo
		valid bool
	}{
		{"git@github.com:acme/repo.git", ghRepo{"acme", "repo", "github.com"}, true},
		{"git@github.com:acme/repo", ghRepo{"acme", "repo", "github.com"}, true},
		{"https://github.com/acme/repo.git", ghRepo{"acme", "repo", "github.com"}, true},
		{"https://user@github.com/acme/repo", ghRepo{"acme", "repo", "github.com"}, true},
		{"ssh://git@github.acme.dev:2222/acme/repo.git", ghRepo{"acme", "repo", "github.acme.dev"}, true},
		{"/srv/git/local.git", ghRepo{}, false},
		{"", ghRepo{}, false},
	}
	for _, tc := range cases {
		got, ok := parseRemoteURL(tc.raw)
		if ok != tc.valid || got != tc.want {
			t.Errorf("parseRemoteURL(%q) = %+v, %v; want %+v, %v", tc.raw, got, ok, tc.want, tc.valid)
		}
	}
}

// TestDescribeGHFailure covers issue #56's second complaint: the old wording
// blamed authentication for every failure, including the 504s that are really
// GitHub shedding load.
func TestDescribeGHFailure(t *testing.T) {
	cases := []struct {
		err  string
		want string
	}{
		{"gh api graphql: HTTP 504: We couldn't respond to your request in time.", "temporarily unavailable"},
		{"gh api graphql: HTTP 401: Bad credentials", "not authenticated"},
		{"gh api graphql: HTTP 404: Could not resolve to a Repository", "not found"},
		{"gh api graphql: something else entirely", "gh query failed"},
	}
	for _, tc := range cases {
		got := describeGHFailure(errors.New(tc.err))
		if !strings.Contains(got, tc.want) {
			t.Errorf("describeGHFailure(%q) = %q, want it to mention %q", tc.err, got, tc.want)
		}
	}
}

func TestGHCommandName(t *testing.T) {
	// A GraphQL document is kilobytes long; the error must name the call, not
	// echo the payload.
	got := ghCommandName([]string{"api", "graphql", "-f", "query=query($owner:String!){…}"})
	if got != "api graphql" {
		t.Errorf("ghCommandName = %q, want %q", got, "api graphql")
	}
}

// sortPRsForTest mirrors the newest-first ordering queryStackPRs applies.
func sortPRsForTest(prs []ghPR) {
	sort.Slice(prs, func(i, j int) bool { return prs[i].Number > prs[j].Number })
}
