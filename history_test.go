package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
)

func writeHistoryFixture(t *testing.T, path string, base int, messages []Message) {
	t.Helper()
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	update := sessionUpdate{}
	for i, message := range messages {
		index := i
		if i > 0 {
			index = base + i
		}
		update.Messages = append(update.Messages, sessionMessage{Index: index, Message: message})
	}
	update.State = &sessionState{Base: base, Cursor: base + len(messages), Memory: "Saved facts."}
	_, writeErr := writeSessionUpdate(file, 0, update)
	closeErr := file.Close()
	if writeErr != nil || closeErr != nil {
		t.Fatalf("write fixture: %v; close: %v", writeErr, closeErr)
	}
}

func TestHistoryReadReturnsDiscardedOriginalsWithoutWrites(t *testing.T) {
	dir := t.TempDir()
	noImplicitTemp(t, dir)
	path := filepath.Join(dir, "history.json")
	raw := strings.Repeat("original raw stdout;", 100000)
	messages := []Message{
		{Role: "system", Content: "system"},
		{Role: "user", Content: "retain the original task"},
		{Role: "assistant", ToolCalls: []ToolCall{regressionCall("exec", "exec")}},
		{Role: "tool", ToolCallID: "exec", Name: "exec", Content: raw},
	}
	writeHistoryFixture(t, path, 0, messages)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	s := newSession()
	s.historyPath, s.historyCount = path, len(messages)
	s.Transcript = append(s.Transcript, Message{Role: "user", Content: "new unsaved work"})
	if got := s.editMemory("Original work summarized; continue new work.", 0); strings.HasPrefix(got, "Error") {
		t.Fatal(got)
	}
	resident := append([]Message(nil), s.Transcript...)
	result, err := s.readHistory(context.Background(), 1, 4)
	if err != nil || result.Start != 1 || result.End != 4 || result.Next != 4 || len(result.Messages) != 3 {
		t.Fatalf("read discarded originals: %+v; %v", result, err)
	}
	for i, entry := range result.Messages {
		if entry.Index != i+1 || !reflect.DeepEqual(entry.Message, messages[i+1]) {
			t.Fatalf("original message %d changed", i+1)
		}
	}
	if !reflect.DeepEqual(s.Transcript, resident) || s.sourcePath != "" || s.owner != nil || s.saveFile != "" {
		t.Fatal("history read cached originals or enabled persistence")
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("history read modified its source: %v", err)
	}
	onlyExplicitFile(t, dir, "history.json")
}

func TestHistoryReadRejectsMissingOriginalsAndInvalidRanges(t *testing.T) {
	s := newSession()
	if _, err := s.readHistory(context.Background(), 0, 1); err == nil || !strings.Contains(err.Error(), "no saved history") {
		t.Fatalf("unsaved history reported as readable: %v", err)
	}
	path := filepath.Join(t.TempDir(), "late-save.json")
	messages := []Message{{Role: "system", Content: "system"}, {Role: "user", Content: "retained later task"}}
	writeHistoryFixture(t, path, 5, messages)
	s.historyPath, s.historyCount = path, 7
	for _, interval := range [][2]int{{-1, 1}, {4, 3}, {6, 8}, {8, 8}} {
		if _, err := s.readHistory(context.Background(), interval[0], interval[1]); err == nil || !strings.Contains(err.Error(), "history range") {
			t.Fatalf("invalid range %v accepted: %v", interval, err)
		}
	}
	for _, interval := range [][2]int{{0, 7}, {1, 2}, {5, 7}} {
		if _, err := s.readHistory(context.Background(), interval[0], interval[1]); err == nil || !strings.Contains(err.Error(), "[1,6)") || !strings.Contains(err.Error(), "cannot be recovered") {
			t.Fatalf("missing originals %v silently skipped: %v", interval, err)
		}
	}
	for _, interval := range [][2]int{{0, 1}, {6, 7}} {
		got, err := s.readHistory(context.Background(), interval[0], interval[1])
		if err != nil || len(got.Messages) != 1 || got.Messages[0].Index != interval[0] {
			t.Fatalf("available range %v rejected: %+v; %v", interval, got, err)
		}
	}
}

func TestHistoryReadEmptyRangeReturnsSelectedArchiveMetadata(t *testing.T) {
	path := filepath.Join(t.TempDir(), "late-save.json")
	messages := []Message{{Role: "system", Content: "system"}, {Role: "user", Content: "retained later task"}}
	writeHistoryFixture(t, path, 5, messages)
	s := newSession()
	s.historyPath, s.historyCount = path, 7
	for _, interval := range [][2]int{{0, 0}, {3, 3}, {7, 7}} {
		result, err := s.readHistory(context.Background(), interval[0], interval[1])
		if err != nil || result.Start != interval[0] || result.End != interval[1] || result.Next != 7 || result.Base != 5 || len(result.Messages) != 0 || result.Messages == nil {
			t.Fatalf("metadata for %v is incorrect: %+v; %v", interval, result, err)
		}
	}
	// A selected read-only snapshot does not implicitly include later records.
	s.historyCount = 6
	result, err := s.readHistory(context.Background(), 0, 0)
	if err != nil || result.Next != 6 || result.Base != 5 {
		t.Fatalf("metadata moved beyond its selected boundary: %+v; %v", result, err)
	}
}

func TestHistoryReadBorrowsOwnerForConcurrentReads(t *testing.T) {
	path := filepath.Join(t.TempDir(), "continued.json")
	messages := []Message{{Role: "system", Content: "system"}, {Role: "user", Content: "original task"}}
	writeHistoryFixture(t, path, 0, messages)
	owner, err := reserveSession(path, false)
	if err != nil {
		t.Fatal(err)
	}
	s := newSession()
	s.owner, s.ownsOwner = owner, true
	s.historyPath, s.historyCount = path, len(messages)
	t.Cleanup(s.close)
	var wait sync.WaitGroup
	for i := 0; i < 8; i++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			result, err := s.readHistory(context.Background(), 0, 2)
			if err != nil || len(result.Messages) != 2 || result.Messages[1].Message.Content != "original task" {
				t.Errorf("borrowed read failed: %+v; %v", result, err)
			}
		}()
	}
	wait.Wait()
	if _, err := owner.file.Stat(); err != nil || !owner.matches(path) {
		t.Fatalf("history read closed the active owner: %v", err)
	}
}

func TestHistoryReadRejectsCorruptedArchiveWithoutReturningPartialData(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.json")
	writeHistoryFixture(t, path, 0, []Message{{Role: "system", Content: "original system"}})
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// Structural corruption: an unknown member breaks the document grammar.
	corrupt := bytes.Replace(original, []byte("\"m0\""), []byte("\"x0\""), 1)
	if bytes.Equal(corrupt, original) {
		t.Fatal("test did not modify a committed payload")
	}
	if err := os.WriteFile(path, corrupt, 0o600); err != nil {
		t.Fatal(err)
	}
	s := newSession()
	s.historyPath, s.historyCount = path, 1
	for _, end := range []int{0, 1} {
		result, err := s.readHistory(context.Background(), 0, end)
		if err == nil || !strings.Contains(err.Error(), "unknown session document field") || len(result.Messages) != 0 {
			t.Fatalf("corrupt history exposed data or metadata: %+v; %v", result, err)
		}
	}
	after, readErr := os.ReadFile(path)
	if readErr != nil || !bytes.Equal(corrupt, after) {
		t.Fatalf("rejected history was modified: %v", readErr)
	}
}

func TestHistoryReadDispatchRequiresExplicitIntegerRange(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.json")
	writeHistoryFixture(t, path, 0, []Message{{Role: "system", Content: "system"}})
	s := newSession()
	s.historyPath, s.historyCount = path, 1
	for _, args := range []string{`{}`, `{"start":0}`, `{"end":1}`, `{"start":null,"end":1}`, `{"start":0.5,"end":1}`, `{"start":0,"end":"1"}`} {
		text, extra := runTool(context.Background(), s, "history_read", args)
		if extra != nil || !strings.Contains(text, "requires integer start and end") {
			t.Fatalf("invalid arguments %s accepted: %s", args, text)
		}
	}
	text, extra := runTool(context.Background(), s, "history_read", `{"start":0,"end":1}`)
	var result struct {
		Result historyReadResult `json:"result"`
	}
	if err := json.Unmarshal([]byte(text), &result); err != nil || extra != nil || len(result.Result.Messages) != 1 || result.Result.Messages[0].Index != 0 {
		t.Fatalf("tool result is not structured history: %s; %v", text, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.readHistory(ctx, 0, 1); err != context.Canceled {
		t.Fatalf("canceled history read continued: %v", err)
	}
}
