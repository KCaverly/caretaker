package stack

import (
	"strings"
	"testing"
)

// navNums builds nav entries from bare PR numbers, all non-merged — the common
// shape for the splice tests.
func navNums(numbers ...int) []navEntry {
	entries := make([]navEntry, len(numbers))
	for i, n := range numbers {
		entries[i].Number = n
	}
	return entries
}

func TestRenderNavRegion(t *testing.T) {
	got := renderNavRegion(navNums(41, 42, 43), 1)
	want := "<!-- ct-stack-begin -->\n" +
		"---\n" +
		"**Stack** (bottom → top):\n" +
		"1. #41\n" +
		"2. #42 ← this PR\n" +
		"3. #43\n" +
		"<!-- ct-stack-end -->"
	if got != want {
		t.Errorf("renderNavRegion mismatch:\n got %q\nwant %q", got, want)
	}
	// A not-yet-created PR (0) renders as "#?".
	if r := renderNavRegion(navNums(0, 42), 0); !strings.Contains(r, "1. #? ← this PR") {
		t.Errorf("expected #? placeholder for unknown PR number, got:\n%s", r)
	}
}

// TestRenderNavRegionMerged checks that a landed PR renders with a ✓ and a
// "(merged)" tag, and that the "← this PR" marker still applies when the current
// row is itself merged.
func TestRenderNavRegionMerged(t *testing.T) {
	got := renderNavRegion([]navEntry{
		{Number: 41, Merged: true},
		{Number: 42, Merged: false},
	}, 1)
	want := "<!-- ct-stack-begin -->\n" +
		"---\n" +
		"**Stack** (bottom → top):\n" +
		"1. ✓ #41 (merged)\n" +
		"2. #42 ← this PR\n" +
		"<!-- ct-stack-end -->"
	if got != want {
		t.Errorf("merged nav render mismatch:\n got %q\nwant %q", got, want)
	}
	// A merged current row keeps both the (merged) tag and the this-PR marker.
	if r := renderNavRegion([]navEntry{{Number: 41, Merged: true}}, 0); !strings.Contains(r, "1. ✓ #41 (merged) ← this PR") {
		t.Errorf("merged current row should keep the this-PR marker, got:\n%s", r)
	}
}

// TestNavEntriesFor projects commit views into nav entries, tagging the merged
// one.
func TestNavEntriesFor(t *testing.T) {
	commits := []Commit{
		{State: StateMerged, PR: &PR{Number: 41}},
		{State: StateOpen, PR: &PR{Number: 42}},
		{State: StateUnpushed},
	}
	got := navEntriesFor(commits)
	want := []navEntry{{Number: 41, Merged: true}, {Number: 42, Merged: false}, {Number: 0, Merged: false}}
	if len(got) != len(want) {
		t.Fatalf("navEntriesFor len = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("entry %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestSpliceNav(t *testing.T) {
	region := renderNavRegion(navNums(41, 42), 0)

	cases := []struct {
		name string
		body string
		want string
	}{
		{
			name: "no markers: append with a blank-line gap",
			body: "Fixes the thing.",
			want: "Fixes the thing.\n\n" + region,
		},
		{
			name: "empty body: region only",
			body: "",
			want: region,
		},
		{
			name: "whitespace-only body: region only",
			body: "\n\n",
			want: region,
		},
		{
			name: "markers mid-body: replace only the region",
			body: "intro\n\n" + renderNavRegion(navNums(99), 0) + "\n\noutro",
			want: "intro\n\n" + region + "\n\noutro",
		},
		{
			name: "prose before and after are preserved byte-for-byte",
			body: "Before prose line 1\nBefore line 2\n" + renderNavRegion(navNums(1, 2, 3), 2) + "\nAfter prose\nmore after",
			want: "Before prose line 1\nBefore line 2\n" + region + "\nAfter prose\nmore after",
		},
		{
			name: "marker-only body: replaced in place",
			body: renderNavRegion(navNums(7), 0),
			want: region,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := spliceNav(tc.body, region); got != tc.want {
				t.Errorf("spliceNav mismatch:\n got %q\nwant %q", got, tc.want)
			}
		})
	}
}

// TestSpliceNavIdempotent checks that splicing an identical region into an
// already-spliced body is a no-op — the property that lets submit skip an
// unnecessary `gh pr edit --body`.
func TestSpliceNavIdempotent(t *testing.T) {
	region := renderNavRegion(navNums(41, 42, 43), 1)
	body := "user wrote this\n\n" + region + "\n\nand this after"
	once := spliceNav(body, region)
	twice := spliceNav(once, region)
	if once != body {
		t.Errorf("first splice should be a no-op on an already-current body:\n got %q\nwant %q", once, body)
	}
	if twice != once {
		t.Errorf("re-splice not idempotent:\n got %q\nwant %q", twice, once)
	}
}
