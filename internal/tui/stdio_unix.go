//go:build !windows

package tui

import (
	"os"
	"syscall"
)

// redirectStdout points the process-level stdout at logFile and returns a
// handle to the original terminal for Bubble Tea.
func redirectStdout(logFile *os.File) *os.File {
	ttyOut := os.Stdout
	if oldFd, err := syscall.Dup(int(os.Stdout.Fd())); err == nil {
		ttyOut = os.NewFile(uintptr(oldFd), "/dev/stdout")
	}
	if logFile != nil {
		_ = syscall.Dup2(int(logFile.Fd()), int(os.Stdout.Fd()))
		os.Stdout = logFile
	}
	return ttyOut
}

func pauseProcess(p *os.Process) error  { return p.Signal(syscall.SIGSTOP) }
func resumeProcess(p *os.Process) error { return p.Signal(syscall.SIGCONT) }
