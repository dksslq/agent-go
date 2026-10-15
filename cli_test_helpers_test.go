package main

import (
	"os"
	"os/exec"
	"testing"
)

// Optional integration coverage; production builds remain unchanged.
// The caller merges GOCOVERDIR output with the go test profile.
func buildCoveredCLI(t *testing.T, path string) {
	t.Helper()
	args := []string{"build", "-race", "-o", path}
	if dir := os.Getenv("AGENT_TEST_COVERDIR"); dir != "" {
		t.Setenv("GOCOVERDIR", dir)
		args = append(args, "-cover", "-covermode=atomic")
	}
	args = append(args, ".")
	if out, err := exec.Command("go", args...).CombinedOutput(); err != nil {
		t.Fatalf("build real CLI: %v\n%s", err, out)
	}
}
