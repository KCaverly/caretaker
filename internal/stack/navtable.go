package stack

import (
	"strconv"
	"strings"
)

// The nav region is a tool-owned block ct splices into every stack PR body so a
// reader on GitHub can see the whole stack from any one PR. Everything between
// (and including) these two HTML-comment markers is owned by ct; everything
// outside is user prose that must survive resubmits byte-for-byte.
const (
	navBeginMarker = "<!-- ct-stack-begin -->"
	navEndMarker   = "<!-- ct-stack-end -->"
)

// renderNavRegion builds the marker-delimited nav block for a stack of PRs,
// bottom-first. numbers[i] is the PR number at stack position i+1 (0 means the
// PR does not exist yet — rendered as "#?", which only happens transiently
// during a dry-run plan or a first create pass). currentIdx is the 0-based index
// of the PR whose body this region is being spliced into, marked "← this PR".
//
// The returned string has no trailing newline after the end marker so spliceNav
// can drop it into a body without accumulating blank lines on repeated splices.
func renderNavRegion(numbers []int, currentIdx int) string {
	var b strings.Builder
	b.WriteString(navBeginMarker + "\n")
	b.WriteString("---\n")
	b.WriteString("**Stack** (bottom → top):\n")
	for i, n := range numbers {
		ref := "#?"
		if n > 0 {
			ref = "#" + strconv.Itoa(n)
		}
		b.WriteString(strconv.Itoa(i+1) + ". " + ref)
		if i == currentIdx {
			b.WriteString(" ← this PR")
		}
		b.WriteString("\n")
	}
	b.WriteString(navEndMarker)
	return b.String()
}

// spliceNav merges a rendered nav region into a PR body. If the body already
// carries both markers, only the span from the begin marker through the end
// marker is replaced — the prefix before the begin marker and the suffix after
// the end marker are preserved exactly, so any prose a human wrote around the
// block on GitHub survives. If the markers are absent, the region is appended.
//
// It is a pure function and idempotent: splicing the same region into an
// already-spliced body returns the byte-identical body, which is what lets the
// executor skip a `gh pr edit --body` when nothing changed.
func spliceNav(body, region string) string {
	begin := strings.Index(body, navBeginMarker)
	end := strings.Index(body, navEndMarker)
	if begin >= 0 && end >= 0 && end >= begin {
		endClose := end + len(navEndMarker)
		return body[:begin] + region + body[endClose:]
	}
	if strings.TrimSpace(body) == "" {
		return region
	}
	return strings.TrimRight(body, "\n") + "\n\n" + region
}
