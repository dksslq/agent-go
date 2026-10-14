//go:build windows

package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The child keeps attempting actual-file ownership across checkpoints, rather
// than testing a goroutine lock or merely checking a marker file.
func TestWindowsSessionOwnership(t *testing.T) {
	if os.Getenv("AGENTLET_TEST_OWNER_CHILD") == "1" {
		path := os.Getenv("AGENTLET_TEST_OWNER_PATH")
		done := make(chan struct{})
		go func() { io.Copy(io.Discard, os.Stdin); close(done) }()
		first := true
		for {
			select {
			case <-done:
				return
			default:
			}
			owner, err := reserveSession(path, false)
			if err == nil {
				owner.close()
				t.Fatal("child acquired another agent's session")
			}
			// ERROR_SHARING_VIOLATION is Win32 error 32.
			if !errors.Is(err, syscall.Errno(32)) {
				t.Fatalf("unexpected reservation failure: %v", err)
			}
			if first {
				fmt.Fprintln(os.Stdout, "conflict")
				first = false
			}
		}
	}
	path := filepath.Join(t.TempDir(), "session.json")
	initial := newSession()
	initial.Transcript = append(initial.Transcript, Message{Role: "user", Content: "original objective"})
	if err := checkpointSession(path, initial); err != nil {
		t.Fatal(err)
	}
	initial.close()
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	owner, err := reserveSession(path, false)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.close()
	s, err := loadReservedSession(path, owner)
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	s.owner = owner

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	child := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestWindowsSessionOwnership$")
	child.Env = append(os.Environ(), "AGENTLET_TEST_OWNER_CHILD=1", "AGENTLET_TEST_OWNER_PATH="+path)
	stdin, err := child.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := child.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr strings.Builder
	child.Stderr = &stderr
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { child.Process.Kill(); child.Wait() }()
	reader := bufio.NewReader(stdout)
	if line, err := reader.ReadString('\n'); err != nil || line != "conflict\n" {
		t.Fatalf("second process did not fail immediately: %q, %v", line, err)
	}
	if _, err := owner.file.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	after, err := io.ReadAll(owner.file)
	if err != nil || string(after) != string(before) {
		t.Fatal("conflicting process changed the original session")
	}
	for i := 0; i < 20; i++ {
		s.Transcript = append(s.Transcript, Message{Role: "assistant", Content: fmt.Sprintf("checkpoint %d", i)})
		if err := checkpointSession(path, s); err != nil {
			t.Fatalf("owned append checkpoint failed: %v", err)
		}
		loaded, err := loadReservedSession(path, owner)
		if err != nil {
			t.Fatal(err)
		}
		if loaded.messageCount() != s.messageCount() {
			t.Fatal("owned snapshot lost history")
		}
		loaded.close()
	}
	stdin.Close()
	io.Copy(io.Discard, reader)
	if err := child.Wait(); err != nil {
		t.Fatalf("ownership opened a checkpoint gap: %v; %s", err, stderr.String())
	}
	owner.close()
	if loaded, err := loadSession(path); err != nil {
		t.Fatal(err)
	} else {
		loaded.close()
	}
}

func TestWindowsReservationRollbackAndCommit(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "new.json")
	owner, err := reserveSession(path, true)
	if err != nil || !owner.createdNew() {
		t.Fatalf("missing file was not reserved as a new session: %v", err)
	}
	if err := owner.close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("failed startup left its placeholder behind: %v", err)
	}

	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	owner, err = reserveSession(path, true)
	if err != nil || owner.createdNew() {
		t.Fatalf("existing empty file was mistaken for a missing session: %v", err)
	}
	if err := owner.close(); err != nil {
		t.Fatal(err)
	}
	if stat, err := os.Stat(path); err != nil || stat.Size() != 0 {
		t.Fatalf("closing an existing empty file changed it: %v", err)
	}

	committed := filepath.Join(dir, "committed.json")
	owner, err = reserveSession(committed, true)
	if err != nil {
		t.Fatal(err)
	}
	s := newSession()
	s.owner = owner
	if err := checkpointSession(committed, s); err != nil {
		owner.close()
		t.Fatal(err)
	}
	if owner.createdNew() {
		t.Fatal("committed snapshot is still marked as a startup placeholder")
	}
	if err := owner.close(); err != nil {
		t.Fatal(err)
	}
	loaded, err := loadSession(committed)
	if err != nil {
		t.Fatal(err)
	}
	loaded.close()
}

func TestWindowsOwnershipDeniesPathReplacement(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.json")
	moved := filepath.Join(dir, "moved.json")
	owner, err := reserveSession(path, true)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.close()
	if err := os.Rename(path, moved); err == nil {
		t.Fatal("exclusive file allowed path replacement")
	}
	if err := os.Remove(path); err == nil {
		t.Fatal("exclusive file allowed deletion")
	}
	if err := owner.close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("unused selected placeholder remained: %v", err)
	}
}
