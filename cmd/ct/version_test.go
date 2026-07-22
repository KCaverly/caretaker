package main

import "testing"

func TestVersionString(t *testing.T) {
	oldVersion, oldCommit, oldDate := version, commit, date
	t.Cleanup(func() { version, commit, date = oldVersion, oldCommit, oldDate })

	version = "v1.2.3"
	commit = "abc1234"
	date = "2026-07-22T12:00:00Z"

	want := "ct v1.2.3 (commit abc1234, built 2026-07-22T12:00:00Z)"
	if got := versionString(); got != want {
		t.Fatalf("versionString() = %q, want %q", got, want)
	}
}
