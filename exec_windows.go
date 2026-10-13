//go:build windows
// +build windows

package main

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

func setSysProcAttr(cmd *exec.Cmd) {
	if cmd.Cancel != nil {
		cmd.Cancel = func() error {
			err := cmd.Process.Kill()
			// Wait releases the handle before the context watcher joins.
			if errors.Is(err, syscall.EINVAL) {
				return os.ErrProcessDone
			}
			return err
		}
	}
}

// CommandContext already cancels the direct child. Killing it again after
// Wait can produce ERROR_INVALID_PARAMETER instead of ErrProcessDone.
func cleanupCommand(*exec.Cmd) error { return nil }
