//go:build !darwin && !linux && !freebsd && !netbsd && !openbsd && !dragonfly

package session

// Unsupported platforms cannot inspect the PTY foreground process group, so
// fail safe and require confirmation before closing a live terminal.
func (s *Session) hasForegroundProcess() bool { return true }
