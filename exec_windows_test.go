//go:build windows

package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"testing"
)

func TestCancelCompletedWindowsCommand(t *testing.T) {
	cmd := exec.CommandContext(context.Background(), "cmd.exe", "/d", "/c", "exit", "0")
	setSysProcAttr(cmd)
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Cancel(); !errors.Is(err, os.ErrProcessDone) {
		t.Fatalf("completed command cancellation: %v", err)
	}
}
