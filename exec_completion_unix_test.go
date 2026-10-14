//go:build linux || darwin

package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// The grandchild has its own /dev/null streams, but inherits the parent's
// process group. Releasing it only after exec returns detects a late group kill.
func TestExecCompletedProcessHelper(t *testing.T) {
	if os.Getenv("AGENT_GO_COMPLETED_HELPER") != "1" {
		return
	}
	args := os.Args
	for len(args) > 0 && args[0] != "--" {
		args = args[1:]
	}
	if len(args) != 5 {
		os.Exit(2)
	}
	mode, ready, release, completed := args[1], args[2], args[3], args[4]
	if mode == "child" {
		if err := os.WriteFile(ready, nil, 0600); err != nil {
			os.Exit(3)
		}
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			if _, err := os.Stat(release); err == nil {
				if err := os.WriteFile(completed, nil, 0600); err != nil {
					os.Exit(4)
				}
				os.Exit(0)
			}
			time.Sleep(10 * time.Millisecond)
		}
		os.Exit(5)
	}
	child := exec.Command(os.Args[0], "-test.run=^TestExecCompletedProcessHelper$", "--", "child", ready, release, completed)
	if err := child.Start(); err != nil {
		os.Exit(6)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(ready); err == nil {
			code, _ := strconv.Atoi(mode)
			os.Exit(code)
		}
		time.Sleep(10 * time.Millisecond)
	}
	os.Exit(8)
}

func TestExecCompletedExitDoesNotKillIndependentChild(t *testing.T) {
	for _, code := range []int{0, 7} {
		t.Run(strconv.Itoa(code), func(t *testing.T) {
			dir := t.TempDir()
			ready, release, completed := filepath.Join(dir, "ready"), filepath.Join(dir, "release"), filepath.Join(dir, "completed")
			t.Cleanup(func() { _ = os.WriteFile(release, nil, 0600) })
			result := decodeExecResult(t, execCmd(context.Background(), os.Args[0],
				[]string{"-test.run=^TestExecCompletedProcessHelper$", "--", strconv.Itoa(code), ready, release, completed},
				"", 10*time.Second, 0, "", map[string]string{"AGENT_GO_COMPLETED_HELPER": "1"}, false))
			if result.ExitCode == nil || *result.ExitCode != code || result.Error != "" {
				t.Fatalf("completed exit changed: %+v", result)
			}
			if err := os.WriteFile(release, nil, 0600); err != nil {
				t.Fatal(err)
			}
			deadline := time.Now().Add(3 * time.Second)
			for {
				if _, err := os.Stat(completed); err == nil {
					return
				}
				if time.Now().After(deadline) {
					t.Fatal("completed command killed a child that had closed its inherited pipes")
				}
				time.Sleep(10 * time.Millisecond)
			}
		})
	}
}
