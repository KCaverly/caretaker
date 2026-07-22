package main

import "fmt"

// These values are replaced by the release build. Keeping useful development
// defaults makes locally-built binaries identify themselves honestly.
var (
	version = "dev"
	commit  = "unknown"
	date    = "unknown"
)

func versionString() string {
	return fmt.Sprintf("ct %s (commit %s, built %s)", version, commit, date)
}
