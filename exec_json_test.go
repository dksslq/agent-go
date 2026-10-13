package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

const execJSONHelperEnv = "AGENT_GO_EXEC_JSON_HELPER"
const execJSONFloodWrites = 1024

var execJSONStdoutChunk = "你" + strings.Repeat("a", 1021)
var execJSONStderrChunk = "λ" + strings.Repeat("b", 1022)

// Use the test executable so every platform exercises real pipes without a
// shell, and os.Exit prevents the test runner from adding its own output.
func TestExecJSONHelperProcess(t *testing.T) {
	if os.Getenv(execJSONHelperEnv) != "1" {
		return
	}
	args := os.Args
	for len(args) > 0 && args[0] != "--" {
		args = args[1:]
	}
	if len(args) < 2 {
		os.Exit(2)
	}
	write := func(w io.Writer, value string) {
		if _, err := io.WriteString(w, value); err != nil {
			os.Exit(3)
		}
	}
	switch args[1] {
	case "separate":
		write(os.Stdout, "out: \"quoted\"\\path\n你\t\x00")
		write(os.Stderr, "err: \"quoted\"\\path\r\nλ\t\x00")
		os.Exit(7)
	case "flood":
		var done sync.WaitGroup
		for _, stream := range []struct {
			file  *os.File
			chunk string
		}{{os.Stdout, execJSONStdoutChunk}, {os.Stderr, execJSONStderrChunk}} {
			done.Add(1)
			go func(file *os.File, chunk string) {
				defer done.Done()
				for i := 0; i < execJSONFloodWrites; i++ {
					write(file, chunk)
				}
			}(stream.file, stream.chunk)
		}
		done.Wait()
		os.Exit(0)
	case "invalid_utf8":
		write(os.Stdout, "out\xff\xe4\xbd")
		write(os.Stderr, "err\xfe\xe4")
		os.Exit(0)
	case "wait":
		if len(args) != 3 {
			os.Exit(2)
		}
		write(os.Stdout, "stdout before cancellation\n")
		write(os.Stderr, "stderr before cancellation\n")
		if err := os.WriteFile(args[2], []byte("ready"), 0600); err != nil {
			os.Exit(4)
		}
		for {
			time.Sleep(time.Hour)
		}
	default:
		os.Exit(2)
	}
}

type execJSONResult struct {
	Stdout          string `json:"stdout"`
	Stderr          string `json:"stderr"`
	ExitCode        *int   `json:"exit_code"`
	Error           string `json:"error"`
	StdoutTruncated bool   `json:"stdout_truncated"`
	StderrTruncated bool   `json:"stderr_truncated"`
	OutputFile      string `json:"output_file"`
	OutputBytes     int64  `json:"output_bytes"`
}

func decodeExecJSON(t *testing.T, raw string) execJSONResult {
	t.Helper()
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &fields); err != nil {
		t.Fatalf("exec result is not a JSON object: %v; %q", err, raw)
	}
	for _, name := range []string{"stdout", "stderr", "exit_code"} {
		if _, exists := fields[name]; !exists {
			t.Fatalf("exec result omits %s: %q", name, raw)
		}
	}
	for _, name := range []string{"stdout", "stderr"} {
		if len(fields[name]) == 0 || fields[name][0] != '"' {
			t.Fatalf("%s must always be a string: %s", name, fields[name])
		}
	}
	var result execJSONResult
	if err := json.Unmarshal([]byte(raw), &result); err != nil {
		t.Fatalf("exec result has invalid field types: %v", err)
	}
	return result
}

func execJSONArgs(mode string, extra ...string) []string {
	return append([]string{"-test.run=^TestExecJSONHelperProcess$", "--", mode}, extra...)
}

func execJSONEnv() map[string]string {
	return map[string]string{execJSONHelperEnv: "1"}
}

func assertExecJSONSuccess(t *testing.T, result execJSONResult) {
	t.Helper()
	if result.ExitCode == nil || *result.ExitCode != 0 || result.Error != "" {
		t.Fatalf("successful command status: exit=%v, error=%q", result.ExitCode, result.Error)
	}
}

func TestExecJSONSeparatesStreamsAndPreservesExitCode(t *testing.T) {
	raw := execCmd(context.Background(), os.Args[0], execJSONArgs("separate"), "", 5*time.Second, 0, "", execJSONEnv(), false)
	result := decodeExecJSON(t, raw)
	if result.Stdout != "out: \"quoted\"\\path\n你\t\x00" || result.Stderr != "err: \"quoted\"\\path\r\nλ\t\x00" {
		t.Fatalf("escaped output changed or streams mixed: stdout=%q stderr=%q", result.Stdout, result.Stderr)
	}
	if result.ExitCode == nil || *result.ExitCode != 7 || result.Error != "" {
		t.Fatalf("normal nonzero exit lost: %s", raw)
	}
}

func TestExecJSONToolResultIsAnObject(t *testing.T) {
	args, err := json.Marshal(map[string]interface{}{
		"program":         os.Args[0],
		"args":            execJSONArgs("separate"),
		"environ":         execJSONEnv(),
		"timeout_seconds": 5,
	})
	if err != nil {
		t.Fatal(err)
	}
	raw, attachment := runTool(context.Background(), nil, "exec", string(args))
	if attachment != nil {
		t.Fatal("exec unexpectedly attached another message")
	}
	var wrapped struct {
		Result json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal([]byte(raw), &wrapped); err != nil {
		t.Fatalf("tool result is invalid JSON: %v", err)
	}
	if len(wrapped.Result) == 0 || wrapped.Result[0] != '{' {
		t.Fatalf("exec result is encoded as a string instead of an object: %s", raw)
	}
	result := decodeExecJSON(t, string(wrapped.Result))
	if result.Stdout != "out: \"quoted\"\\path\n你\t\x00" || result.Stderr != "err: \"quoted\"\\path\r\nλ\t\x00" || result.ExitCode == nil || *result.ExitCode != 7 {
		t.Fatalf("tool wrapper changed the exec fields: %s", raw)
	}
}

func TestExecJSONConcurrentStreamsAreComplete(t *testing.T) {
	raw := execCmd(context.Background(), os.Args[0], execJSONArgs("flood"), "", 10*time.Second, 0, "", execJSONEnv(), false)
	result := decodeExecJSON(t, raw)
	assertExecJSONSuccess(t, result)
	wantOut := strings.Repeat(execJSONStdoutChunk, execJSONFloodWrites)
	wantErr := strings.Repeat(execJSONStderrChunk, execJSONFloodWrites)
	if result.Stdout != wantOut || result.Stderr != wantErr {
		t.Fatalf("concurrent output lost or mixed: stdout=%d/%d bytes stderr=%d/%d bytes", len(result.Stdout), len(wantOut), len(result.Stderr), len(wantErr))
	}
}

func TestExecJSONInvalidUTF8UsesStandardReplacement(t *testing.T) {
	raw := execCmd(context.Background(), os.Args[0], execJSONArgs("invalid_utf8"), "", 5*time.Second, 0, "", execJSONEnv(), false)
	result := decodeExecJSON(t, raw)
	assertExecJSONSuccess(t, result)
	if result.Stdout != "out\ufffd\ufffd\ufffd" || result.Stderr != "err\ufffd\ufffd" {
		t.Fatalf("JSON changed invalid UTF-8 semantics: stdout=%q stderr=%q", result.Stdout, result.Stderr)
	}
}

func TestExecJSONCancellationRetainsBothStreams(t *testing.T) {
	for _, byParent := range []bool{false, true} {
		t.Run(fmt.Sprintf("parentCancel=%v", byParent), func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			marker := filepath.Join(t.TempDir(), "ready")
			timeout := 2 * time.Second
			if byParent {
				timeout = 10 * time.Second
			}
			done := make(chan string, 1)
			go func() {
				done <- execCmd(ctx, os.Args[0], execJSONArgs("wait", marker), "", timeout, 0, "", execJSONEnv(), false)
			}()
			if byParent {
				deadline := time.NewTimer(5 * time.Second)
				defer deadline.Stop()
				poll := time.NewTicker(10 * time.Millisecond)
				defer poll.Stop()
				ready := false
				for !ready {
					if _, err := os.Stat(marker); err == nil {
						ready = true
						break
					}
					select {
					case raw := <-done:
						t.Fatalf("command exited before cancellation: %s", raw)
					case <-deadline.C:
						t.Fatal("child did not become ready")
					case <-poll.C:
					}
				}
				cancel()
			}
			select {
			case raw := <-done:
				result := decodeExecJSON(t, raw)
				if result.Stdout != "stdout before cancellation\n" || result.Stderr != "stderr before cancellation\n" {
					t.Fatalf("cancellation discarded output: %s", raw)
				}
				if result.Error == "" || (runtime.GOOS != "windows" && result.ExitCode != nil && *result.ExitCode >= 0) {
					t.Fatalf("cancellation status looks like a normal exit: %s", raw)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("cancellation failed to release the caller")
			}
		})
	}
}

func TestExecJSONStartFailureIsStructured(t *testing.T) {
	program := filepath.Join(t.TempDir(), "nonexistent-executable")
	raw := execCmd(context.Background(), program, nil, "", time.Second, 0, "", nil, false)
	result := decodeExecJSON(t, raw)
	if result.Stdout != "" || result.Stderr != "" || result.ExitCode != nil || result.Error == "" {
		t.Fatalf("start failure lost its status: %s", raw)
	}
}

func TestExecJSONStartFailureRollsBackOutputFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "must-not-exist")
	raw := execCmdOutput(context.Background(), filepath.Join(dir, "nonexistent-executable"), nil, "", time.Second, 0, dir, nil, false, "must-not-exist")
	result := decodeExecJSON(t, raw)
	if result.ExitCode != nil || result.Error == "" {
		t.Fatalf("start failure lost its status: %s", raw)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("failed start left an output file behind: %v", err)
	}
	if result.OutputFile != "" || result.OutputBytes != 0 {
		t.Fatalf("failed start reports a committed output file: %s", raw)
	}
}

func TestExecJSONFileKeepsFullOutputWithIndependentPreviews(t *testing.T) {
	for _, limit := range []int{0, 3} {
		t.Run(fmt.Sprintf("preview=%d", limit), func(t *testing.T) {
			dir := t.TempDir()
			raw := execCmdOutput(context.Background(), os.Args[0], execJSONArgs("flood"), "", 10*time.Second, limit, dir, execJSONEnv(), false, "full-output")
			result := decodeExecJSON(t, raw)
			assertExecJSONSuccess(t, result)
			path := filepath.Join(dir, "full-output")
			if result.OutputFile != path || result.OutputBytes != 2*1024*execJSONFloodWrites {
				t.Fatalf("saved output metadata: %s", raw)
			}
			if limit == 0 {
				if result.Stdout != "" || result.Stderr != "" || result.StdoutTruncated || result.StderrTruncated {
					t.Fatalf("file-only result retains inline output: %s", raw)
				}
			} else if result.Stdout != "你aa" || result.Stderr != "λbb" || !result.StdoutTruncated || !result.StderrTruncated {
				t.Fatalf("preview is not limited independently per stream: %s", raw)
			}
			data, err := os.ReadFile(path)
			if err != nil || int64(len(data)) != result.OutputBytes {
				t.Fatalf("saved output length=%d, error=%v", len(data), err)
			}
			// The raw file may interleave reads from the two pipes. Validate
			// every emitted byte without requiring a cross-stream order.
			counts := map[byte]int{'a': 1021 * execJSONFloodWrites, 'b': 1022 * execJSONFloodWrites}
			for _, value := range []byte("你λ") {
				counts[value] = execJSONFloodWrites
			}
			for value, count := range counts {
				if got := bytes.Count(data, []byte{value}); got != count {
					t.Fatalf("saved byte %#x appears %d times; want %d", value, got, count)
				}
			}
		})
	}
}
