//go:build darwin || linux || freebsd || netbsd || openbsd || dragonfly

package session

import "golang.org/x/sys/unix"

func (s *Session) hasForegroundProcess() bool {
	foreground, err := unix.IoctlGetInt(int(s.pty.Fd()), unix.TIOCGPGRP)
	if err != nil {
		return true
	}
	initial, err := unix.Getpgid(s.cmd.Process.Pid)
	if err != nil {
		return true
	}
	return foreground != initial
}
