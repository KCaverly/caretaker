package stack

import (
	"reflect"
	"testing"
)

func TestParseGitLog(t *testing.T) {
	// Four NUL-fenced fields per line, bottom-first. Note the third commit carries
	// two ct-stack-id trailers, which git concatenates with no separator (see the
	// parseGitLog doc comment) — a malformed value the reconciler flags later. The
	// last line is blank (git's trailing newline) and must be skipped.
	out := "aaaaaaa1111\x00aaaaaaa\x00first subject\x00\n" +
		"bbbbbbb2222\x00bbbbbbb\x00second, has trailer\x00abc12345\n" +
		"ccccccc3333\x00ccccccc\x00two trailers\x00aaaaaaaabbbbbbbb\n" +
		"\n"
	got := parseGitLog(out)
	want := []LocalCommit{
		{SHA: "aaaaaaa1111", ShortSHA: "aaaaaaa", Subject: "first subject", StackID: ""},
		{SHA: "bbbbbbb2222", ShortSHA: "bbbbbbb", Subject: "second, has trailer", StackID: "abc12345"},
		{SHA: "ccccccc3333", ShortSHA: "ccccccc", Subject: "two trailers", StackID: "aaaaaaaabbbbbbbb"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parseGitLog mismatch:\n got %+v\nwant %+v", got, want)
	}
}

func TestParseGitLogEmpty(t *testing.T) {
	if got := parseGitLog(""); len(got) != 0 {
		t.Errorf("expected no commits, got %+v", got)
	}
}

func TestParseRemoteBranches(t *testing.T) {
	// short refname NUL object-name. The id is the last path segment, so it holds
	// up even when the worktree name itself contains a slash.
	out := "origin/ct/wt/abc12345\x00sha_abc\n" +
		"origin/ct/team/api/def67890\x00sha_def\n" +
		"malformed-no-nul\n" +
		"\x00only-sha\n" +
		"origin/ct/wt/empty\x00\n"
	got := parseRemoteBranches(out)
	want := map[string]string{
		"abc12345": "sha_abc",
		"def67890": "sha_def",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parseRemoteBranches = %v, want %v", got, want)
	}
}
