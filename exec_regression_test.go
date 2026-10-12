package main

import (
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestParseTimeoutFractionalAndOverflow(t *testing.T) {
	d, err := parseTimeout(map[string]interface{}{"timeout_seconds": 0.125})
	if err != nil || d != 125*time.Millisecond {
		t.Fatalf("fractional timeout = %v, %v", d, err)
	}
	for _, v := range []float64{0, -1, math.NaN(), math.Inf(1), math.MaxFloat64, float64(1<<63) / float64(time.Second)} {
		if _, err := parseTimeout(map[string]interface{}{"timeout_seconds": v}); err == nil {
			t.Fatalf("accepted invalid timeout %v", v)
		}
	}
}

func TestCommandOutputSplitUTF8AndDiscard(t *testing.T) {
	out, err := newCommandOutput(2, "", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, chunk := range [][]byte{{0xe4}, {0xbd, 0xa0, 0xe5}, {0xa5}, {0xbd}, []byte(strings.Repeat("x", 100000))} {
		if n, err := out.Write(chunk); err != nil || n != len(chunk) {
			t.Fatalf("Write = %d, %v", n, err)
		}
	}
	result, err := out.finish()
	if err != nil || result != "你好\n...[truncated]" {
		t.Fatalf("split UTF-8 preview = %q, %v", result, err)
	}
	if out.buffer.Len() != len("你好") || len(out.pending) != 0 {
		t.Fatalf("discarded output retained: %d buffered, %d pending", out.buffer.Len(), len(out.pending))
	}
}

func TestCommandOutputIncompleteAndInvalidUTF8(t *testing.T) {
	for _, tc := range []struct {
		chunks [][]byte
		limit  int
		want   string
	}{
		{[][]byte{{0xe4}, {'a', 'b'}}, 2, "\xe4a\n...[truncated]"},
		{[][]byte{{0xe4, 0xbd}}, 1, "\xe4\n...[truncated]"},
		{[][]byte{[]byte("你好")}, 2, "你好"},
	} {
		out, _ := newCommandOutput(tc.limit, "", "")
		for _, p := range tc.chunks {
			out.Write(p)
		}
		got, err := out.finish()
		if err != nil || got != tc.want {
			t.Fatalf("preview = %q, %v; want %q", got, err, tc.want)
		}
	}
}

// TestExecCaptureHelperProcess supplies portable child behavior for exec tests.
func TestExecCaptureHelperProcess(t *testing.T) {
	if os.Getenv("AGENT_GO_EXEC_HELPER") != "1" {
		return
	}
	args := os.Args
	for len(args) > 0 && args[0] != "--" {
		args = args[1:]
	}
	if len(args) != 2 {
		os.Exit(2)
	}
	if _, err := os.Stdout.WriteString(strings.Repeat("x", 100000)); err != nil {
		os.Exit(3)
	}
	if err := os.WriteFile(args[1], []byte("complete"), 0600); err != nil {
		os.Exit(4)
	}
	os.Exit(7)
}

func TestExecEnvironmentHelperProcess(t *testing.T) {
	if os.Getenv("AGENT_GO_ENV_HELPER") != "1" {
		return
	}
	fmt.Printf("PWD=%s\nINJECTED=%s\n", os.Getenv("PWD"), os.Getenv("AGENT_GO_INJECTED"))
	os.Exit(0)
}

func TestExecEnvironmentUsesWorkingDirectory(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("PWD is a Unix environment convention")
	}
	dir := t.TempDir()
	result := execCmd(context.Background(), os.Args[0], []string{"-test.run=^TestExecEnvironmentHelperProcess$"}, "", 5*time.Second, 0, dir, map[string]string{"AGENT_GO_ENV_HELPER": "1", "AGENT_GO_INJECTED": "yes"}, false)
	if !strings.Contains(result, "PWD="+dir+"\n") || !strings.Contains(result, "INJECTED=yes\n") {
		t.Fatalf("injected environment lost working directory: %q", result)
	}
	result = execCmd(context.Background(), os.Args[0], []string{"-test.run=^TestExecEnvironmentHelperProcess$"}, "", 5*time.Second, 0, dir, map[string]string{"AGENT_GO_ENV_HELPER": "1", "PWD": "explicit"}, false)
	if !strings.Contains(result, "PWD=explicit\n") {
		t.Fatalf("explicit PWD override was lost: %q", result)
	}
}

func captureHelperArgs(marker string) []string {
	return []string{"-test.run=^TestExecCaptureHelperProcess$", "--", marker}
}

func TestExecOutputLimitDrainsAndPreservesStatus(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "finished")
	result := execCmd(context.Background(), os.Args[0], captureHelperArgs(marker), "", 5*time.Second, 20, "", map[string]string{"AGENT_GO_EXEC_HELPER": "1"}, false)
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("output limit terminated the command: %v; result %q", err, result)
	}
	if !strings.HasPrefix(result, strings.Repeat("x", 20)+"\n...[truncated]") || !strings.Contains(result, "Error:") || !strings.Contains(result, "exit status 7") {
		t.Fatalf("preview lost output or exit status: %q", result)
	}
}

func TestExecOutputFilePreservesRawBytesAndExistingFiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "output")
	marker := filepath.Join(dir, "finished")
	result := execCmdOutput(context.Background(), os.Args[0], captureHelperArgs(marker), "", 5*time.Second, 10, dir, map[string]string{"AGENT_GO_EXEC_HELPER": "1"}, false, "output")
	data, err := os.ReadFile(path)
	if err != nil || string(data) != strings.Repeat("x", 100000) {
		t.Fatalf("raw output not retained: %d bytes, %v", len(data), err)
	}
	if !strings.Contains(result, path) || !strings.Contains(result, "100000 bytes") || !strings.Contains(result, strings.Repeat("x", 10)+"\n...[truncated]") || !strings.Contains(result, "exit status 7") {
		t.Fatalf("file result missing preview/status: %q", result)
	}
	if runtime.GOOS != "windows" {
		st, err := os.Stat(path)
		if err != nil || st.Mode().Perm() != 0600 {
			t.Fatalf("output permissions: %v, %v", st, err)
		}
	}
	result = execCmdOutput(context.Background(), os.Args[0], captureHelperArgs(marker), "", 5*time.Second, 0, dir, nil, false, "output")
	if !strings.Contains(result, "Output file error:") {
		t.Fatalf("existing output file accepted: %q", result)
	}
	after, err := os.ReadFile(path)
	if err != nil || string(after) != string(data) {
		t.Fatal("existing output file changed")
	}
}

func TestCommandOutputFileWithoutPreview(t *testing.T) {
	path := filepath.Join(t.TempDir(), "output")
	out, err := newCommandOutput(0, "", path)
	if err != nil {
		t.Fatal(err)
	}
	out.Write([]byte("raw\x00bytes\xff"))
	result, err := out.finish()
	if err != nil || out.buffer.Len() != 0 || strings.Contains(result, "raw") {
		t.Fatalf("file-only output retained inline: %q, %v", result, err)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "raw\x00bytes\xff" {
		t.Fatalf("file-only output = %q, %v", data, err)
	}
}

func TestExecCancelKillsGrandchildren(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("Unix process-group regression")
	}
	for _, cancelParent := range []bool{false, true} {
		t.Run(fmt.Sprintf("parentCancel=%v", cancelParent), func(t *testing.T) {
			dir := t.TempDir()
			ready := filepath.Join(dir, "ready")
			leaked := filepath.Join(dir, "leaked")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			timeout := 150 * time.Millisecond
			if cancelParent {
				timeout = 5 * time.Second
			}
			done := make(chan string, 1)
			go func() {
				done <- execCmd(ctx, "sh", []string{"-c", `sh -c 'printf ready > "$1"; sleep 1; printf leaked > "$2"' sh "$1" "$2" & wait`, "sh", ready, leaked}, "", timeout, 0, "", nil, false)
			}()
			if cancelParent {
				deadline := time.Now().Add(2 * time.Second)
				for {
					if _, err := os.Stat(ready); err == nil {
						break
					}
					if time.Now().After(deadline) {
						t.Fatal("child did not become ready")
					}
					time.Sleep(10 * time.Millisecond)
				}
				cancel()
			}
			select {
			case result := <-done:
				want := "timed out"
				if cancelParent {
					want = "interrupted"
				}
				if !strings.Contains(result, want) {
					t.Fatalf("cancel status = %q; want %q", result, want)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("cancel did not return")
			}
			time.Sleep(1100 * time.Millisecond)
			if _, err := os.Stat(leaked); !os.IsNotExist(err) {
				t.Fatalf("grandchild survived cancellation: %v", err)
			}
		})
	}
}

func TestExecExitedParentDoesNotLeavePipeHoldingChild(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("Unix process-group regression")
	}
	for _, tc := range []struct {
		timeout time.Duration
		exit    string
	}{
		{100 * time.Millisecond, ""},
		{5 * time.Second, ""},
		{5 * time.Second, " exit 9"},
	} {
		t.Run(tc.timeout.String()+tc.exit, func(t *testing.T) {
			marker := filepath.Join(t.TempDir(), "leaked")
			result := execCmd(context.Background(), "sh", []string{"-c", `sh -c 'sleep 1; printf leaked > "$1"' sh "$1" &` + tc.exit, "sh", marker}, "", tc.timeout, 0, "", nil, false)
			if !strings.Contains(result, "Error:") {
				t.Fatalf("pipe drain failure was hidden: %q", result)
			}
			time.Sleep(1100 * time.Millisecond)
			if _, err := os.Stat(marker); !os.IsNotExist(err) {
				t.Fatalf("pipe-holding child survived: %v", err)
			}
		})
	}
}

func TestExecDetachedProcessIsReaped(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux /proc verifies that exited children are reaped")
	}
	result := execCmd(context.Background(), "sh", []string{"-c", "exit 0"}, "", time.Second, 0, "", nil, true)
	var pid int
	if _, err := fmt.Sscanf(result, "Process %d detached", &pid); err != nil || pid <= 0 {
		t.Fatalf("detach result: %q", result)
	}
	deadline := time.Now().Add(2 * time.Second)
	path := filepath.Join("/proc", strconv.Itoa(pid))
	for {
		if _, err := os.Stat(path); os.IsNotExist(err) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("detached child %d remains after exit", pid)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
