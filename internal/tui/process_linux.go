//go:build linux

package tui

import (
	"os/exec"
	"syscall"
)

// configurePlayerProcess makes mpv exit when wa-cli exits on Linux.
func configurePlayerProcess(player *exec.Cmd) {
	player.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGTERM}
}
