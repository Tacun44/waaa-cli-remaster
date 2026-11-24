//go:build windows

package tui

import "os/exec"

// configurePlayerProcess is a no-op on Windows.
func configurePlayerProcess(player *exec.Cmd) {}
