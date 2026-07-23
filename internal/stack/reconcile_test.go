package stack

import (
	"reflect"
	"testing"
)

// commit is a terse LocalCommit builder for the table below. The SHA and short
// SHA are derived from the id/marker so remote-sync assertions are easy to line
// up.
func commit(sha, id, subject string) LocalCommit {
	return LocalCommit{SHA: sha, ShortSHA: sha[:7], Subject: subject, StackID: id}
}

// pr is a terse prRecord builder. checks is a summary word; failing names are
// left empty because reconcile never inspects them.
func pr(num int, state, head, base, review, checksSummary string) prRecord {
	return prRecord{
		Number:    num,
		URL:       "https://example.test/pr/" + itoa(num),
		State:     state,
		Head:      head,
		Base:      base,
		Review:    review,
		Mergeable: "MERGEABLE",
		Checks:    Checks{Summary: checksSummary, Failing: []string{}},
	}
}

// prm is pr() plus an explicit mergeable value (MERGEABLE / CONFLICTING /
// UNKNOWN), for the cascade cases that turn on it.
func prm(num int, state, head, base, review, checksSummary, mergeable string) prRecord {
	r := pr(num, state, head, base, review, checksSummary)
	r.Mergeable = mergeable
	return r
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

func TestReconcile(t *testing.T) {
	const wt, main = "wt", "main"
	br := func(id string) string { return "ct/" + wt + "/" + id }

	cases := []struct {
		name          string
		commits       []LocalCommit
		remotes       map[string]string
		prs           []prRecord
		wantStates    []State
		wantAction    string
		wantChainOK   bool
		wantOrphanIDs []string
	}{
		{
			name:        "unsubmitted: no trailer",
			commits:     []LocalCommit{commit("aaaaaaa1111", "", "wip")},
			wantStates:  []State{StateUnsubmitted},
			wantAction:  "submit",
			wantChainOK: true,
		},
		{
			name:        "unpushed: trailer, no remote branch",
			commits:     []LocalCommit{commit("aaaaaaa1111", "aaaaaaaa", "feat")},
			wantStates:  []State{StateUnpushed},
			wantAction:  "submit",
			wantChainOK: true,
		},
		{
			name:        "diverged: remote tip differs from local",
			commits:     []LocalCommit{commit("aaaaaaa1111", "aaaaaaaa", "feat")},
			remotes:     map[string]string{"aaaaaaaa": "bbbbbbb2222"},
			wantStates:  []State{StateDiverged},
			wantAction:  "submit",
			wantChainOK: true,
		},
		{
			name:        "missing-pr: in sync, no PR",
			commits:     []LocalCommit{commit("aaaaaaa1111", "aaaaaaaa", "feat")},
			remotes:     map[string]string{"aaaaaaaa": "aaaaaaa1111"},
			wantStates:  []State{StateMissingPR},
			wantAction:  "submit",
			wantChainOK: true,
		},
		{
			name:        "open: in sync PR, approved, passing -> merge",
			commits:     []LocalCommit{commit("aaaaaaa1111", "aaaaaaaa", "feat")},
			remotes:     map[string]string{"aaaaaaaa": "aaaaaaa1111"},
			prs:         []prRecord{pr(1, "OPEN", br("aaaaaaaa"), main, "APPROVED", "passing")},
			wantStates:  []State{StateOpen},
			wantAction:  "merge",
			wantChainOK: true,
		},
		{
			name:        "open: conflicting PR -> resolve conflicts",
			commits:     []LocalCommit{commit("aaaaaaa1111", "aaaaaaaa", "feat")},
			remotes:     map[string]string{"aaaaaaaa": "aaaaaaa1111"},
			prs:         []prRecord{prm(1, "OPEN", br("aaaaaaaa"), main, "APPROVED", "passing", "CONFLICTING")},
			wantStates:  []State{StateOpen},
			wantAction:  "resolve-conflicts",
			wantChainOK: true,
		},
		{
			name:        "open: unknown mergeability is not mergeable",
			commits:     []LocalCommit{commit("aaaaaaa1111", "aaaaaaaa", "feat")},
			remotes:     map[string]string{"aaaaaaaa": "aaaaaaa1111"},
			prs:         []prRecord{prm(1, "OPEN", br("aaaaaaaa"), main, "APPROVED", "passing", "UNKNOWN")},
			wantStates:  []State{StateOpen},
			wantAction:  "wait",
			wantChainOK: true,
		},
		{
			name:        "open: non-main base is not mergeable",
			commits:     []LocalCommit{commit("aaaaaaa1111", "aaaaaaaa", "feat")},
			remotes:     map[string]string{"aaaaaaaa": "aaaaaaa1111"},
			prs:         []prRecord{prm(1, "OPEN", br("aaaaaaaa"), "feature", "APPROVED", "passing", "MERGEABLE")},
			wantStates:  []State{StateOpen},
			wantAction:  "submit",
			wantChainOK: false,
		},
		{
			name:        "merged: fully landed single commit -> finish",
			commits:     []LocalCommit{commit("aaaaaaa1111", "aaaaaaaa", "feat")},
			remotes:     map[string]string{"aaaaaaaa": "aaaaaaa1111"},
			prs:         []prRecord{pr(1, "MERGED", br("aaaaaaaa"), main, "APPROVED", "passing")},
			wantStates:  []State{StateMerged},
			wantAction:  "finish",
			wantChainOK: true,
		},
		{
			name:        "closed: PR closed without merge -> escalate",
			commits:     []LocalCommit{commit("aaaaaaa1111", "aaaaaaaa", "feat")},
			remotes:     map[string]string{"aaaaaaaa": "aaaaaaa1111"},
			prs:         []prRecord{pr(1, "CLOSED", br("aaaaaaaa"), main, "", "none")},
			wantStates:  []State{StateClosed},
			wantAction:  "escalate",
			wantChainOK: true,
		},
		{
			name: "duplicate-id: same trailer on two commits -> escalate",
			commits: []LocalCommit{
				commit("aaaaaaa1111", "aaaaaaaa", "one"),
				commit("bbbbbbb2222", "aaaaaaaa", "two"),
			},
			wantStates:  []State{StateDuplicateID, StateDuplicateID},
			wantAction:  "escalate",
			wantChainOK: true,
		},
		{
			name:        "duplicate-id: malformed trailer (two concatenated ids)",
			commits:     []LocalCommit{commit("aaaaaaa1111", "aaaaaaaabbbbbbbb", "double")},
			wantStates:  []State{StateDuplicateID},
			wantAction:  "escalate",
			wantChainOK: true,
		},
		{
			name:        "empty stack, nothing tracked -> clean",
			commits:     nil,
			wantStates:  []State{},
			wantAction:  "clean",
			wantChainOK: true,
		},
		{
			name: "all-merged -> finish",
			commits: []LocalCommit{
				commit("aaaaaaa1111", "aaaaaaaa", "one"),
				commit("bbbbbbb2222", "bbbbbbbb", "two"),
			},
			remotes: map[string]string{"aaaaaaaa": "aaaaaaa1111", "bbbbbbbb": "bbbbbbb2222"},
			prs: []prRecord{
				pr(1, "MERGED", br("aaaaaaaa"), main, "APPROVED", "passing"),
				pr(2, "MERGED", br("bbbbbbbb"), br("aaaaaaaa"), "APPROVED", "passing"),
			},
			wantStates:  []State{StateMerged, StateMerged},
			wantAction:  "finish",
			wantChainOK: true,
		},
		{
			name:        "fully landed after restack: empty + merged PR -> archive",
			commits:     nil,
			prs:         []prRecord{pr(1, "MERGED", br("aaaaaaaa"), main, "APPROVED", "passing")},
			wantStates:  []State{},
			wantAction:  "archive",
			wantChainOK: true,
		},
		{
			name:        "gh unavailable (no PRs): local shape still resolves",
			commits:     []LocalCommit{commit("aaaaaaa1111", "aaaaaaaa", "feat")},
			remotes:     map[string]string{"aaaaaaaa": "aaaaaaa1111"},
			prs:         nil, // as if gh returned nothing
			wantStates:  []State{StateMissingPR},
			wantAction:  "submit",
			wantChainOK: true,
		},
		{
			name:    "orphan: open PR whose id matches no commit -> escalate",
			commits: []LocalCommit{commit("aaaaaaa1111", "aaaaaaaa", "feat")},
			remotes: map[string]string{"aaaaaaaa": "aaaaaaa1111"},
			prs: []prRecord{
				pr(1, "OPEN", br("aaaaaaaa"), main, "APPROVED", "passing"),
				pr(2, "OPEN", br("deadbeef"), main, "APPROVED", "passing"),
			},
			wantStates:    []State{StateOpen},
			wantAction:    "escalate",
			wantChainOK:   true,
			wantOrphanIDs: []string{"deadbeef"},
		},
		{
			name: "broken base chain: upper PR bases on main instead of lower branch",
			commits: []LocalCommit{
				commit("aaaaaaa1111", "aaaaaaaa", "one"),
				commit("bbbbbbb2222", "bbbbbbbb", "two"),
			},
			remotes: map[string]string{"aaaaaaaa": "aaaaaaa1111", "bbbbbbbb": "bbbbbbb2222"},
			prs: []prRecord{
				pr(1, "OPEN", br("aaaaaaaa"), main, "APPROVED", "passing"),
				pr(2, "OPEN", br("bbbbbbbb"), main, "APPROVED", "passing"), // should base on ct/wt/aaaaaaaa
			},
			wantStates:  []State{StateOpen, StateOpen},
			wantAction:  "submit", // broken chain routes to submit (restack the bases)
			wantChainOK: false,
		},
		{
			name:        "merge-eligibility: passing + REVIEW_REQUIRED -> wait",
			commits:     []LocalCommit{commit("aaaaaaa1111", "aaaaaaaa", "feat")},
			remotes:     map[string]string{"aaaaaaaa": "aaaaaaa1111"},
			prs:         []prRecord{pr(1, "OPEN", br("aaaaaaaa"), main, "REVIEW_REQUIRED", "passing")},
			wantStates:  []State{StateOpen},
			wantAction:  "wait",
			wantChainOK: true,
		},
		{
			name:        "merge-eligibility: passing + empty review -> merge",
			commits:     []LocalCommit{commit("aaaaaaa1111", "aaaaaaaa", "feat")},
			remotes:     map[string]string{"aaaaaaaa": "aaaaaaa1111"},
			prs:         []prRecord{pr(1, "OPEN", br("aaaaaaaa"), main, "", "passing")},
			wantStates:  []State{StateOpen},
			wantAction:  "merge",
			wantChainOK: true,
		},
		{
			name:        "fix-ci: bottom open PR failing checks",
			commits:     []LocalCommit{commit("aaaaaaa1111", "aaaaaaaa", "feat")},
			remotes:     map[string]string{"aaaaaaaa": "aaaaaaa1111"},
			prs:         []prRecord{pr(1, "OPEN", br("aaaaaaaa"), main, "APPROVED", "failing")},
			wantStates:  []State{StateOpen},
			wantAction:  "fix-ci",
			wantChainOK: true,
		},
		{
			name:        "wait: bottom open PR pending checks",
			commits:     []LocalCommit{commit("aaaaaaa1111", "aaaaaaaa", "feat")},
			remotes:     map[string]string{"aaaaaaaa": "aaaaaaa1111"},
			prs:         []prRecord{pr(1, "OPEN", br("aaaaaaaa"), main, "APPROVED", "pending")},
			wantStates:  []State{StateOpen},
			wantAction:  "wait",
			wantChainOK: true,
		},
		{
			// The cascade: A landed (still local as merged), B is green/approved and
			// auto-retargeted onto main. Keep merging — do NOT restack.
			name: "cascade: merged prefix + green approved main-based open -> merge",
			commits: []LocalCommit{
				commit("aaaaaaa1111", "aaaaaaaa", "one"),
				commit("bbbbbbb2222", "bbbbbbbb", "two"),
			},
			remotes: map[string]string{"aaaaaaaa": "aaaaaaa1111", "bbbbbbbb": "bbbbbbb2222"},
			prs: []prRecord{
				pr(1, "MERGED", br("aaaaaaaa"), main, "APPROVED", "passing"),
				pr(2, "OPEN", br("bbbbbbbb"), main, "APPROVED", "passing"),
			},
			wantStates:  []State{StateMerged, StateOpen},
			wantAction:  "merge",
			wantChainOK: true,
		},
		{
			name: "cascade blocked: merged prefix + failing bottom open -> restack",
			commits: []LocalCommit{
				commit("aaaaaaa1111", "aaaaaaaa", "one"),
				commit("bbbbbbb2222", "bbbbbbbb", "two"),
			},
			remotes: map[string]string{"aaaaaaaa": "aaaaaaa1111", "bbbbbbbb": "bbbbbbb2222"},
			prs: []prRecord{
				pr(1, "MERGED", br("aaaaaaaa"), main, "APPROVED", "passing"),
				pr(2, "OPEN", br("bbbbbbbb"), main, "APPROVED", "failing"),
			},
			wantStates:  []State{StateMerged, StateOpen},
			wantAction:  "restack",
			wantChainOK: true,
		},
		{
			name: "cascade blocked: merged prefix + CONFLICTING bottom open -> resolve conflicts",
			commits: []LocalCommit{
				commit("aaaaaaa1111", "aaaaaaaa", "one"),
				commit("bbbbbbb2222", "bbbbbbbb", "two"),
			},
			remotes: map[string]string{"aaaaaaaa": "aaaaaaa1111", "bbbbbbbb": "bbbbbbb2222"},
			prs: []prRecord{
				pr(1, "MERGED", br("aaaaaaaa"), main, "APPROVED", "passing"),
				prm(2, "OPEN", br("bbbbbbbb"), main, "APPROVED", "passing", "CONFLICTING"),
			},
			wantStates:  []State{StateMerged, StateOpen},
			wantAction:  "resolve-conflicts",
			wantChainOK: true,
		},
		{
			// Auto-retarget hasn't happened: B still bases on the landed branch, so
			// the chain is broken and the cascade is blocked -> restack (not merge).
			name: "cascade blocked: merged prefix + mis-based bottom open -> restack",
			commits: []LocalCommit{
				commit("aaaaaaa1111", "aaaaaaaa", "one"),
				commit("bbbbbbb2222", "bbbbbbbb", "two"),
			},
			remotes: map[string]string{"aaaaaaaa": "aaaaaaa1111", "bbbbbbbb": "bbbbbbb2222"},
			prs: []prRecord{
				pr(1, "MERGED", br("aaaaaaaa"), main, "APPROVED", "passing"),
				pr(2, "OPEN", br("bbbbbbbb"), br("aaaaaaaa"), "APPROVED", "passing"),
			},
			wantStates:  []State{StateMerged, StateOpen},
			wantAction:  "restack",
			wantChainOK: false,
		},
		{
			// Merged below, bottom open merely pending: CI may go green and the
			// cascade continue, so wait rather than restack.
			name: "cascade pending: merged prefix + pending bottom open -> wait",
			commits: []LocalCommit{
				commit("aaaaaaa1111", "aaaaaaaa", "one"),
				commit("bbbbbbb2222", "bbbbbbbb", "two"),
			},
			remotes: map[string]string{"aaaaaaaa": "aaaaaaa1111", "bbbbbbbb": "bbbbbbb2222"},
			prs: []prRecord{
				pr(1, "MERGED", br("aaaaaaaa"), main, "APPROVED", "passing"),
				pr(2, "OPEN", br("bbbbbbbb"), main, "APPROVED", "pending"),
			},
			wantStates:  []State{StateMerged, StateOpen},
			wantAction:  "wait",
			wantChainOK: true,
		},
		{
			// Passing but review still outstanding: wait for the reviewer, the
			// cascade may still complete.
			name: "cascade review: merged prefix + passing REVIEW_REQUIRED bottom open -> wait",
			commits: []LocalCommit{
				commit("aaaaaaa1111", "aaaaaaaa", "one"),
				commit("bbbbbbb2222", "bbbbbbbb", "two"),
			},
			remotes: map[string]string{"aaaaaaaa": "aaaaaaa1111", "bbbbbbbb": "bbbbbbb2222"},
			prs: []prRecord{
				pr(1, "MERGED", br("aaaaaaaa"), main, "APPROVED", "passing"),
				pr(2, "OPEN", br("bbbbbbbb"), main, "REVIEW_REQUIRED", "passing"),
			},
			wantStates:  []State{StateMerged, StateOpen},
			wantAction:  "wait",
			wantChainOK: true,
		},
		{
			// A landed commit sitting above an unlanded one is an out-of-order land.
			name: "non-contiguous merged prefix -> escalate",
			commits: []LocalCommit{
				commit("aaaaaaa1111", "aaaaaaaa", "one"),
				commit("bbbbbbb2222", "bbbbbbbb", "two"),
				commit("ccccccc3333", "cccccccc", "three"),
			},
			remotes: map[string]string{
				"aaaaaaaa": "aaaaaaa1111", "bbbbbbbb": "bbbbbbb2222", "cccccccc": "ccccccc3333",
			},
			prs: []prRecord{
				pr(1, "MERGED", br("aaaaaaaa"), main, "APPROVED", "passing"),
				pr(2, "OPEN", br("bbbbbbbb"), main, "APPROVED", "passing"),
				pr(3, "MERGED", br("cccccccc"), br("bbbbbbbb"), "APPROVED", "passing"),
			},
			wantStates:  []State{StateMerged, StateOpen, StateMerged},
			wantAction:  "escalate",
			wantChainOK: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stk, commits := reconcile(wt, main, tc.commits, tc.remotes, tc.prs)

			gotStates := make([]State, len(commits))
			for i, c := range commits {
				gotStates[i] = c.State
			}
			if !reflect.DeepEqual(gotStates, tc.wantStates) {
				t.Errorf("states = %v, want %v", gotStates, tc.wantStates)
			}
			if stk.NextAction != tc.wantAction {
				t.Errorf("next_action = %q, want %q", stk.NextAction, tc.wantAction)
			}
			if stk.BaseChainOK != tc.wantChainOK {
				t.Errorf("base_chain_ok = %v, want %v", stk.BaseChainOK, tc.wantChainOK)
			}
			gotOrphans := make([]string, len(stk.Orphans))
			for i, o := range stk.Orphans {
				gotOrphans[i] = o.StackID
			}
			want := tc.wantOrphanIDs
			if want == nil {
				want = []string{}
			}
			if !reflect.DeepEqual(gotOrphans, want) {
				t.Errorf("orphan ids = %v, want %v", gotOrphans, want)
			}
		})
	}
}

// TestReconcilePRProjection checks the per-commit PR view and the null rules the
// JSON contract requires (pr null for unsubmitted/unpushed/missing-pr; merged_at
// null unless merged).
func TestReconcilePRProjection(t *testing.T) {
	const wt, main = "wt", "main"
	commits := []LocalCommit{commit("aaaaaaa1111", "aaaaaaaa", "feat")}
	remotes := map[string]string{"aaaaaaaa": "aaaaaaa1111"}
	rec := pr(1, "OPEN", "ct/wt/aaaaaaaa", main, "APPROVED", "passing")
	rec.MergedAt = ""
	_, got := reconcile(wt, main, commits, remotes, []prRecord{rec})

	c := got[0]
	if c.PR == nil {
		t.Fatal("open commit should carry a PR")
	}
	if c.PR.MergedAt != nil {
		t.Errorf("open PR merged_at should be nil, got %v", *c.PR.MergedAt)
	}
	if c.RemoteBranch == nil || *c.RemoteBranch != "ct/wt/aaaaaaaa" {
		t.Errorf("remote_branch = %v, want ct/wt/aaaaaaaa", c.RemoteBranch)
	}
	if !c.RemoteInSync {
		t.Error("remote_in_sync should be true when tip matches")
	}

	// A missing-pr commit must have a nil PR pointer.
	_, got2 := reconcile(wt, main, commits, remotes, nil)
	if got2[0].PR != nil {
		t.Errorf("missing-pr commit should have nil PR, got %+v", got2[0].PR)
	}
	if got2[0].StackID == nil || *got2[0].StackID != "aaaaaaaa" {
		t.Errorf("stack_id = %v, want aaaaaaaa", got2[0].StackID)
	}
}

// TestMergeHintContents verifies the squash subject/body hint: it targets the
// bottom open PR, carries the commit subject, and keeps the message body verbatim
// (ct-stack-id trailer included, unlike a PR body which strips it).
func TestMergeHintContents(t *testing.T) {
	const wt, main = "wt", "main"
	br := func(id string) string { return "ct/" + wt + "/" + id }
	commits := []LocalCommit{
		commit("aaaaaaa1111", "aaaaaaaa", "one"),
		commit("bbbbbbb2222", "bbbbbbbb", "add the widget"),
	}
	remotes := map[string]string{"aaaaaaaa": "aaaaaaa1111", "bbbbbbbb": "bbbbbbb2222"}
	prs := []prRecord{
		pr(1, "MERGED", br("aaaaaaaa"), main, "APPROVED", "passing"),
		pr(2, "OPEN", br("bbbbbbbb"), main, "APPROVED", "passing"),
	}
	stk, out := reconcile(wt, main, commits, remotes, prs)
	if stk.NextAction != "merge" {
		t.Fatalf("precondition: next_action = %q, want merge", stk.NextAction)
	}

	c := bottomOpenCommit(out)
	if c == nil {
		t.Fatal("bottomOpenCommit returned nil for a merge-eligible stack")
	}
	if c.PR == nil || c.PR.Number != 2 {
		t.Fatalf("bottom open commit PR = %+v, want #2", c.PR)
	}

	body := "why the widget is needed\n\nct-stack-id: bbbbbbbb"
	hint := makeMergeHint(c, body+"\n")
	if hint == nil {
		t.Fatal("makeMergeHint returned nil")
	}
	if hint.Number != 2 {
		t.Errorf("hint number = %d, want 2", hint.Number)
	}
	if hint.Subject != "add the widget" {
		t.Errorf("hint subject = %q, want %q", hint.Subject, "add the widget")
	}
	if hint.Body != body {
		t.Errorf("hint body = %q, want %q (trailer retained, trailing newline trimmed)", hint.Body, body)
	}
}

func TestValidStackID(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"aaaaaaaa", true},
		{"0123abcd", true},
		{"deadbeef", true},
		{"", false},
		{"AAAAAAAA", false},         // uppercase not allowed
		{"aaaaaaa", false},          // 7 chars
		{"aaaaaaaaa", false},        // 9 chars
		{"aaaaaaaabbbbbbbb", false}, // two ids concatenated
		{"aaaa,aaa", false},         // literal comma
		{"gggggggg", false},         // non-hex
	}
	for _, tc := range cases {
		if got := validStackID(tc.in); got != tc.want {
			t.Errorf("validStackID(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}
