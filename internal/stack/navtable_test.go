package stack

import (
	"strings"
	"testing"
)

func TestRenderNavRegion(t *testing.T) {
	got := renderNavRegion([]int{41, 42, 43}, 1)
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
	if r := renderNavRegion([]int{0, 42}, 0); !strings.Contains(r, "1. #? ← this PR") {
		t.Errorf("expected #? placeholder for unknown PR number, got:\n%s", r)
	}
}

func TestSpliceNav(t *testing.T) {
	region := renderNavRegion([]int{41, 42}, 0)

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
			body: "intro\n\n" + renderNavRegion([]int{99}, 0) + "\n\noutro",
			want: "intro\n\n" + region + "\n\noutro",
		},
		{
			name: "prose before and after are preserved byte-for-byte",
			body: "Before prose line 1\nBefore line 2\n" + renderNavRegion([]int{1, 2, 3}, 2) + "\nAfter prose\nmore after",
			want: "Before prose line 1\nBefore line 2\n" + region + "\nAfter prose\nmore after",
		},
		{
			name: "marker-only body: replaced in place",
			body: renderNavRegion([]int{7}, 0),
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
	region := renderNavRegion([]int{41, 42, 43}, 1)
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
