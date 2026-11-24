//go:build darwin

package tui

import "os/exec"

// macOS has no Pdeathsig equivalent. mpv is still started without looping.
func configurePlayerProcess(_ *exec.Cmd) {}
