package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// An absent temporary directory makes accidental spooling fail even when the
// tests run as root, for which chmod alone would not prevent a write.
func noImplicitTemp(t *testing.T, dir string) {
	t.Helper()
	missing := filepath.Join(dir, "must-not-create-temp")
	for _, key := range []string{"TMPDIR", "TMP", "TEMP"} {
		t.Setenv(key, missing)
	}
	t.Cleanup(func() {
		if _, err := os.Lstat(missing); !os.IsNotExist(err) {
			t.Errorf("agent created or touched its temporary directory: %v", err)
		}
	})
}

func onlyExplicitFile(t *testing.T, dir, name string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != name || entries[0].IsDir() {
		t.Fatalf("operation created files other than %q: %v", name, entries)
	}
}

func TestUnsavedMemoryEditUsesOnlyRAM(t *testing.T) {
	dir := t.TempDir()
	noImplicitTemp(t, dir)
	s := newSession()
	t.Cleanup(s.close)
	raw := strings.Repeat("raw stdout that should leave RAM;", 4096)
	s.Transcript = append(s.Transcript,
		Message{Role: "user", Content: "retain the objective"},
		Message{Role: "assistant", ToolCalls: []ToolCall{regressionCall("exec", "exec")}},
		Message{Role: "tool", ToolCallID: "exec", Name: "exec", Content: raw},
		Message{Role: "assistant", ToolCalls: []ToolCall{regressionCall("edit", "memory_edit")}},
	)
	result := s.editMemory("Objective retained; exec succeeded; continue work.", 0)
	if strings.HasPrefix(result, "Error") {
		t.Fatal(result)
	}
	if s.Cursor != 4 || s.messageCount() != 5 || len(s.Transcript) != 2 || cap(s.Transcript) > 2 {
		t.Fatalf("compaction retained the consumed backing array or lost indices: cursor=%d count=%d len=%d cap=%d", s.Cursor, s.messageCount(), len(s.Transcript), cap(s.Transcript))
	}
	if s.Transcript[1].ToolCalls[0].ID != "edit" {
		t.Fatal("compaction discarded the pending tool call")
	}
	for _, message := range s.contextMessages() {
		if content, ok := message.Content.(string); ok && strings.Contains(content, raw) {
			t.Fatal("absorbed tool stdout remains in the API context")
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("unsaved compaction created files: %v; %v", entries, err)
	}
}

func TestLegacySessionLoadingDoesNotSpool(t *testing.T) {
	dir := t.TempDir()
	noImplicitTemp(t, dir)
	fixtures := []string{
		`[{"role":"system","content":"custom persona"},{"role":"user","content":"objective"},{"role":"assistant","content":"evidence"}]`,
		`{"version":3,"transcript":[{"role":"system","content":"custom persona"},{"role":"user","content":"objective"},{"role":"assistant","content":"evidence"}],"memory":"Retained objective","cursor":2}`,
		`{"cursor":2,"memory":"Retained objective","version":3,"transcript":[{"role":"system","content":"custom persona"},{"role":"user","content":"objective"},{"role":"assistant","content":"evidence"}]}`,
	}
	path := filepath.Join(dir, "session.json")
	for i, fixture := range fixtures {
		if err := os.WriteFile(path, []byte(fixture), 0o600); err != nil {
			t.Fatal(err)
		}
		s, err := loadSession(path)
		if err != nil {
			t.Fatalf("fixture %d needs temporary files: %v", i, err)
		}
		if s == nil || s.Transcript[0].Content != "custom persona" || s.messageCount() != 3 {
			t.Fatalf("fixture %d lost custom persona or history", i)
		}
		s.close()
		contents, err := os.ReadFile(path)
		if err != nil || string(contents) != fixture {
			t.Fatalf("loading fixture %d changed its source: %v", i, err)
		}
		onlyExplicitFile(t, dir, "session.json")
	}
}

func TestExplicitSessionPersistsHistoryWithoutOtherFiles(t *testing.T) {
	dir := t.TempDir()
	noImplicitTemp(t, dir)
	path := filepath.Join(dir, "session.json")
	s := newSession()
	s.saveFile = path
	t.Cleanup(s.close)
	raw := strings.Repeat("unique original output;", 4096)
	s.Transcript = append(s.Transcript,
		Message{Role: "user", Content: "preserve the objective"},
		Message{Role: "assistant", ToolCalls: []ToolCall{regressionCall("exec", "exec")}},
		Message{Role: "tool", ToolCallID: "exec", Name: "exec", Content: raw},
	)
	if err := checkpointSession(path, s); err != nil {
		t.Fatal(err)
	}
	onlyExplicitFile(t, dir, "session.json")
	s.Transcript = append(s.Transcript, Message{Role: "assistant", ToolCalls: []ToolCall{regressionCall("edit", "memory_edit")}})
	result := s.editMemory("Objective and exec evidence retained; continue work.", 0)
	if strings.HasPrefix(result, "Error") {
		t.Fatal(result)
	}
	s.Transcript = append(s.Transcript, Message{Role: "tool", ToolCallID: "edit", Name: "memory_edit", Content: result})
	if err := checkpointSession(path, s); err != nil {
		t.Fatal(err)
	}
	s.close()
	contents, err := os.ReadFile(path)
	if err != nil || !bytes.Contains(contents, []byte(raw)) {
		t.Fatalf("explicit journal lost original tool output: %v", err)
	}
	loaded, err := loadSession(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(loaded.close)
	if loaded.Cursor != 4 || loaded.messageCount() != 6 || len(loaded.Transcript) != 3 || loaded.Memory != s.Memory {
		t.Fatalf("resume lost memory, indices or the live tool group: %+v", loaded)
	}
	for _, message := range loaded.contextMessages() {
		if content, ok := message.Content.(string); ok && strings.Contains(content, raw) {
			t.Fatal("resume submitted persisted tool output again")
		}
	}
	onlyExplicitFile(t, dir, "session.json")
}

func TestSavingAfterUnsavedCompactionPreservesOnlyRetainedHistory(t *testing.T) {
	dir := t.TempDir()
	noImplicitTemp(t, dir)
	s := newSession()
	t.Cleanup(s.close)
	original := strings.Repeat("original unsaved evidence;", 1024)
	s.Transcript = append(s.Transcript,
		Message{Role: "user", Content: "original unsaved objective"},
		Message{Role: "assistant", Content: original},
	)
	if result := s.editMemory("Objective and evidence summarized.", 0); strings.HasPrefix(result, "Error") {
		t.Fatal(result)
	}
	s.Transcript = append(s.Transcript, Message{Role: "user", Content: "continue with the retained facts"})
	path := filepath.Join(dir, "session.json")
	if err := checkpointSession(path, s); err != nil {
		t.Fatal(err)
	}
	s.close()
	contents, err := os.ReadFile(path)
	if err != nil || bytes.Contains(contents, []byte(original)) {
		t.Fatalf("a late save resurrected discarded originals: %v", err)
	}
	loaded, err := loadSession(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(loaded.close)
	if loaded.Cursor != 3 || loaded.messageCount() != 4 || len(loaded.Transcript) != 2 || loaded.Memory != s.Memory || loaded.Transcript[1].Content != "continue with the retained facts" {
		t.Fatalf("late save lost absolute indices, memory or retained work: %+v", loaded)
	}
	onlyExplicitFile(t, dir, "session.json")
}
