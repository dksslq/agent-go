//go:build windows
// +build windows

package main

import "os/exec"

func setSysProcAttr(cmd *exec.Cmd) {
	// Windows 不支持 Setpgid，保持空实现即可
}
