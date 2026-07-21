package stack

import (
	"reflect"
	"testing"
)

// ghFixture is realistic `gh pr list --json …` output: two PRs in the wt stack
// (one open with a mixed check rollup, one merged), plus one unrelated PR whose
// head is outside the ct/wt/ namespace and must be filtered out. It mixes the
// CheckRun node shape (status/conclusion) with the legacy StatusContext shape
// (state) so summarizeChecks is exercised on both.
const ghFixture = `[
  {
    "number": 10,
    "url": "https://github.com/acme/repo/pull/10",
    "title": "bottom",
    "state": "OPEN",
    "isDraft": false,
    "headRefName": "ct/wt/aaaaaaaa",
    "baseRefName": "main",
    "reviewDecision": "APPROVED",
    "mergeable": "CONFLICTING",
    "mergedAt": "",
    "statusCheckRollup": [
      {"__typename": "CheckRun", "name": "build", "status": "COMPLETED", "conclusion": "SUCCESS"},
      {"__typename": "CheckRun", "name": "lint", "status": "IN_PROGRESS", "conclusion": ""},
      {"__typename": "StatusContext", "context": "ci/deploy", "state": "SUCCESS"}
    ]
  },
  {
    "number": 9,
    "url": "https://github.com/acme/repo/pull/9",
    "title": "landed",
    "state": "MERGED",
    "isDraft": false,
    "headRefName": "ct/wt/bbbbbbbb",
    "baseRefName": "main",
    "reviewDecision": "APPROVED",
    "mergeable": "MERGEABLE",
    "mergedAt": "2026-07-10T12:00:00Z",
    "statusCheckRollup": []
  },
  {
    "number": 3,
    "url": "https://github.com/acme/repo/pull/3",
    "title": "unrelated feature branch",
    "state": "OPEN",
    "isDraft": true,
    "headRefName": "feature/other",
    "baseRefName": "main",
    "reviewDecision": "",
    "mergedAt": "",
    "statusCheckRollup": []
  }
]`

func TestDecodeAndFilterGHPRs(t *testing.T) {
	prs, err := decodeGHPRs([]byte(ghFixture))
	if err != nil {
		t.Fatalf("decodeGHPRs: %v", err)
	}
	if len(prs) != 3 {
		t.Fatalf("decoded %d PRs, want 3", len(prs))
	}

	recs := filterStackPRs(prs, "wt")
	if len(recs) != 2 {
		t.Fatalf("filtered to %d records, want 2 (feature/other dropped)", len(recs))
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
	recs, gh := gatherGitHub(t.TempDir(), "wt")
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
