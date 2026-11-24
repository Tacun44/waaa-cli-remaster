//go:build windows

package tui

import (
	"errors"
	"os"
)

// redirectStdout keeps the real console for Bubble Tea and sends library
// writes (os.Stdout users such as whatsmeow's logger) to logFile.
func redirectStdout(logFile *os.File) *os.File {
	ttyOut := os.Stdout
	if logFile != nil {
		os.Stdout = logFile
	}
	return ttyOut
}

var errNoPauseWindows = errors.New("pausar audio no está disponible en Windows")

func pauseProcess(p *os.Process) error  { return errNoPauseWindows }
func resumeProcess(p *os.Process) error { return errNoPauseWindows }
