//go:build windows
// +build windows

package main

import "os/exec"

func setSysProcAttr(cmd *exec.Cmd) {
	// Windows 不支持 Setpgid，保持空实现即可
}

// CommandContext already cancels the direct child. Killing it again after
// Wait can produce ERROR_INVALID_PARAMETER instead of ErrProcessDone.
func cleanupCommand(*exec.Cmd) error { return nil }
