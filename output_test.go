package main

import (
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"
)

func TestOutputTurnCancellationAndRecovery(t *testing.T) {
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	defer writer.Close()
	root, shutdown := context.WithCancel(context.Background())
	defer shutdown()
	out := newOutputWriter(root, writer)
	turn, cancel := context.WithCancel(root)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- out.write(turn, strings.Repeat("x", 1024*1024)) }()
	var first [1]byte
	if _, err := reader.Read(first[:]); err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("blocked write: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("turn cancellation did not release output wait")
	}
	// The existing OS write is still blocked; another turn must be cancelable
	// while waiting to hand off to the same writer, without spawning a writer.
	next, cancelNext := context.WithTimeout(root, 50*time.Millisecond)
	defer cancelNext()
	if err := out.write(next, "must not be queued"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("busy writer: %v", err)
	}
	drained := make(chan string, 1)
	go func() { data, _ := io.ReadAll(reader); drained <- string(data) }()
	if err := out.write(root, "recovered"); err != nil {
		t.Fatalf("next turn: %v", err)
	}
	writer.Close()
	got := <-drained
	if strings.Contains(got, "must not be queued") || !strings.HasSuffix(got, "recovered") {
		t.Fatal("canceled pending output was replayed or next output was lost")
	}
}
