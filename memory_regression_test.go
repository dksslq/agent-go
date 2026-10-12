package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func regressionCall(id, name string) ToolCall {
	return ToolCall{ID: id, Type: "function", Function: FunctionCall{Name: name, Arguments: `{}`}}
}

func regressionSavedTranscript(t *testing.T, s *Session) []Message {
	t.Helper()
	path := filepath.Join(t.TempDir(), "session.json")
	if err := saveSession(path, s); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		Transcript []Message `json:"transcript"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		t.Fatal(err)
	}
	return wire.Transcript
}

func TestMemoryCompactsWithinAutonomousTurn(t *testing.T) {
	s := &Session{Cursor: 1, Transcript: []Message{
		{Role: "system", Content: "system"},
		{Role: "user", Content: "finish the autonomous task"},
		{Role: "assistant", ToolCalls: []ToolCall{regressionCall("exec1", "exec")}},
		{Role: "tool", ToolCallID: "exec1", Name: "exec", Content: strings.Repeat("large result", 10000)},
		{Role: "assistant", ToolCalls: []ToolCall{regressionCall("memory1", "memory_edit")}},
	}}
	t.Cleanup(s.close)
	original := append([]Message(nil), s.Transcript...)
	result := s.editMemory("Objective: finish autonomous task. exec1 succeeded. Next: continue.", 0)
	if strings.HasPrefix(result, "Error") || s.Cursor != 4 || s.messageCount() != 5 || len(s.Transcript) != 2 {
		t.Fatalf("current turn did not compact safely: %s; %+v", result, s)
	}
	if s.Transcript[1].ToolCalls[0].ID != "memory1" {
		t.Fatal("in-flight tool group was removed")
	}
	s.Transcript = append(s.Transcript, Message{Role: "tool", ToolCallID: "memory1", Name: "memory_edit", Content: result})
	original = append(original, s.Transcript[2])
	if got := regressionSavedTranscript(t, s); !reflect.DeepEqual(got, original) {
		t.Fatal("compaction changed historical messages in the portable session file")
	}
	if view := s.contextMessages(); len(view) != 4 || view[2].Role != "assistant" || view[3].ToolCallID != "memory1" {
		t.Fatalf("next request has a broken tool group: %#v", view)
	}

	// Repeated compactions keep absolute indices and append only new archive
	// entries even after a saved session is loaded again.
	path := filepath.Join(t.TempDir(), "resume.json")
	if err := saveSession(path, s); err != nil {
		t.Fatal(err)
	}
	loaded, err := loadSession(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(loaded.close)
	loaded.Transcript = append(loaded.Transcript, Message{Role: "assistant", ToolCalls: []ToolCall{regressionCall("memory2", "memory_edit")}})
	original = append(original, loaded.Transcript[len(loaded.Transcript)-1])
	result = loaded.editMemory("Objective retained; both earlier operations succeeded.", 0)
	if loaded.Cursor != 6 || len(loaded.Transcript) != 2 {
		t.Fatalf("second compaction lost absolute indices: %s; %+v", result, loaded)
	}
	last := Message{Role: "tool", ToolCallID: "memory2", Name: "memory_edit", Content: result}
	loaded.Transcript = append(loaded.Transcript, last)
	original = append(original, last)
	if got := regressionSavedTranscript(t, loaded); !reflect.DeepEqual(got, original) {
		t.Fatal("archive replay duplicated or lost messages after resume")
	}
}

func TestMemoryCursorSkipsWholeParallelToolGroup(t *testing.T) {
	s := &Session{Cursor: 1, Transcript: []Message{
		{Role: "system", Content: "s"},
		{Role: "user", Content: "u"},
		{Role: "assistant", ToolCalls: []ToolCall{regressionCall("a", "exec"), regressionCall("b", "exec")}},
		{Role: "tool", ToolCallID: "b", Content: "b"},
		{Role: "tool", ToolCallID: "a", Content: "a"},
		{Role: "assistant", ToolCalls: []ToolCall{regressionCall("m", "memory_edit")}},
	}}
	t.Cleanup(s.close)
	s.editMemory("task and results retained", 3)
	if s.Cursor != 5 || len(s.Transcript) != 2 || s.Transcript[1].Role != "assistant" {
		t.Fatalf("cursor split parallel tool group: %+v", s)
	}
	s.editMemory("new facts", 1)
	if s.Cursor != 5 || s.Memory != "task and results retained" {
		t.Fatal("a stale cursor replaced memory or moved backwards after releasing the prefix")
	}
}

func TestMemoryArchiveFailureLeavesStateIntact(t *testing.T) {
	s := newSession()
	s.Transcript = append(s.Transcript, Message{Role: "user", Content: "retain me"})
	path := filepath.Join(t.TempDir(), "read-only-history.jsonl")
	if err := os.WriteFile(path, []byte(`{"role":"system","content":"s"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	s.archive, s.archiveEntries = file, 1
	t.Cleanup(s.close)
	before := append([]Message(nil), s.Transcript...)
	if got := s.editMemory("new memory", 0); !strings.HasPrefix(got, "Error") {
		t.Fatalf("failed archive write reported success: %s", got)
	}
	if s.Memory != "" || s.Cursor != 1 || s.archived != 0 || !reflect.DeepEqual(before, s.Transcript) {
		t.Fatal("failed compaction discarded live history")
	}
}

func TestSessionSavePrivateUniqueAndConcurrent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.json")
	oldTmp := path + ".tmp"
	if err := os.WriteFile(oldTmp, []byte("user-owned file"), 0o600); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s := newSession()
			s.Transcript = append(s.Transcript, Message{Role: "user", Content: "concurrent save"})
			if err := saveSession(path, s); err != nil {
				t.Errorf("save: %v", err)
			}
		}()
	}
	wg.Wait()
	s, err := loadSession(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.close)
	if s.messageCount() != 2 {
		t.Fatal("concurrent saves tore the session")
	}
	data, err := os.ReadFile(oldTmp)
	if err != nil || string(data) != "user-owned file" {
		t.Fatal("save clobbered a pre-existing .tmp file")
	}
	stat, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && stat.Mode().Perm() != 0o600 {
		t.Fatalf("session with tool logs is not private: %o", stat.Mode().Perm())
	}
	leftovers, err := filepath.Glob(filepath.Join(dir, ".session.json.*.tmp"))
	if err != nil || len(leftovers) != 0 {
		t.Fatalf("temporary files leaked: %v, %v", leftovers, err)
	}
}

func TestInvalidSessionCannotReplaceSavedState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.json")
	if err := saveSession(path, newSession()); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	invalid := newSession()
	invalid.Transcript = append(invalid.Transcript, Message{Role: "tool", ToolCallID: "orphan", Content: "result"})
	if err := saveSession(path, invalid); err == nil {
		t.Fatal("save accepted orphaned tool result")
	}
	after, err := os.ReadFile(path)
	if err != nil || string(after) != string(before) {
		t.Fatal("failed save changed the existing session")
	}
}

func TestLoadRejectsInvalidSessionSemantics(t *testing.T) {
	cases := []string{
		`null`, `{}`, `[]`,
		`{"version":4,"transcript":[{"role":"system","content":"s"}]}`,
		`{"transcript":[{"role":"user","content":"u"}]}`,
		`{"transcript":[{"role":"system"},{"role":"unknown"}]}`,
		`{"transcript":[{"role":"system","content":{"invalid":"shape"}}]}`,
		`{"transcript":[{"role":"system","content":"s"},{"role":"user","content":[{"type":"text","text":123}]}]}`,
		`{"transcript":[{"role":"system","content":"s"},{"role":"user","content":[{"type":"image_url","image_url":{"url":123}}]}]}`,
		`{"transcript":[{"role":"system","content":"s"},{"role":"assistant","content":"a","tool_calls":[{"id":"a","type":"function","function":{"name":"exec","arguments":"not JSON"}}]},{"role":"tool","tool_call_id":"a","content":"result"}]}`,
		`{"transcript":[{"role":"system","content":"s"},{"role":"assistant","tool_calls":[{"id":"a","type":"function","function":{"name":"exec","arguments":"{}"}}]},{"role":"tool","tool_call_id":"a","content":{"invalid":"shape"}}]}`,
		`{"transcript":[{"role":"system"},{"role":"tool","tool_call_id":"missing"}]}`,
		`{"transcript":[{"role":"system"},{"role":"assistant","tool_calls":[{"id":"a","type":"function","function":{"name":"exec"}}]}]}`,
		`{"transcript":[{"role":"system"}],"cursor":1,"cursor":2}`,
		`{"transcript":[{"role":"system"}]} {"another":"object"}`,
		`{"transcript":[{"role":"system"},{"role":"assistant","tool_calls":[{"id":"a","function":{"name":"exec"}},{"id":"a","function":{"name":"exec"}}]},{"role":"tool","tool_call_id":"a"}]}`,
	}
	for _, body := range cases {
		path := filepath.Join(t.TempDir(), "bad.json")
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if s, err := loadSession(path); err == nil {
			s.close()
			t.Errorf("accepted invalid session: %s", body)
		}
	}
}

func TestLoadSessionRejectsSpecialFiles(t *testing.T) {
	if _, err := loadSession(t.TempDir()); err == nil {
		t.Fatal("session directory was accepted")
	}
	if runtime.GOOS != "windows" {
		if _, err := loadSession("/dev/null"); err == nil {
			t.Fatal("special session device was accepted")
		}
	}
}

func TestLoadStreamingReleasesAbsorbedHistory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.json")
	s := newSession()
	for i := 0; i < 128; i++ {
		s.Transcript = append(s.Transcript, Message{Role: "user", Content: strings.Repeat("x", 64*1024)})
	}
	s.Memory = "Task context retained."
	s.Cursor = len(s.Transcript)
	if err := saveSession(path, s); err != nil {
		t.Fatal(err)
	}
	loaded, err := loadSession(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(loaded.close)
	if len(loaded.Transcript) != 1 || loaded.messageCount() != s.messageCount() || loaded.Cursor != s.Cursor {
		t.Fatalf("load retained absorbed payloads or lost history: %+v", loaded)
	}
	if got := regressionSavedTranscript(t, loaded); !reflect.DeepEqual(got, s.Transcript) {
		t.Fatal("large archived transcript did not survive streaming roundtrip")
	}
	archivePath := loaded.archive.Name()
	file := loaded.archive
	loaded.close()
	if _, err := os.Stat(archivePath); !os.IsNotExist(err) {
		t.Fatalf("archive temporary file leaked: %v", err)
	}
	if _, err := file.WriteString("after close"); err == nil {
		t.Fatal("archive descriptor leaked")
	}
}

func TestLoadCursorNormalizesPastToolResults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.json")
	s := &Session{Cursor: 3, Memory: "facts", Transcript: []Message{
		{Role: "system", Content: "s"},
		{Role: "user", Content: "u"},
		{Role: "assistant", ToolCalls: []ToolCall{regressionCall("a", "exec"), regressionCall("b", "exec")}},
		{Role: "tool", ToolCallID: "a", Content: "a"},
		{Role: "tool", ToolCallID: "b", Content: "b"},
		{Role: "user", Content: "next"},
	}}
	if err := saveSession(path, s); err != nil {
		t.Fatal(err)
	}
	loaded, err := loadSession(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(loaded.close)
	if loaded.Cursor != 5 || loaded.residentCursor() != 1 || len(loaded.contextMessages()) != 3 || loaded.Transcript[1].Content != "next" {
		t.Fatalf("loaded context starts with orphaned tool results: %+v", loaded)
	}
}

func TestSessionCannotHideHistoryWithoutMemory(t *testing.T) {
	for _, memory := range []string{"", " \n\t "} {
		s := newSession()
		s.Transcript = append(s.Transcript, Message{Role: "user", Content: "important objective"}, Message{Role: "assistant", Content: "important evidence"})
		s.Cursor, s.Memory = 3, memory
		path := filepath.Join(t.TempDir(), "session.json")
		if err := saveSession(path, newSession()); err != nil {
			t.Fatal(err)
		}
		before, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := saveSession(path, s); err == nil {
			t.Fatal("save silently hid history without semantic memory")
		}
		after, err := os.ReadFile(path)
		if err != nil || string(after) != string(before) {
			t.Fatal("invalid compaction replaced the existing saved session")
		}
		body, err := json.Marshal(s)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, body, 0o600); err != nil {
			t.Fatal(err)
		}
		if loaded, err := loadSession(path); err == nil {
			loaded.close()
			t.Fatal("load silently hid history without semantic memory")
		}
	}
}

func TestReloadMemoryHeaderDiscoversCurrentArchive(t *testing.T) {
	s := newSession()
	t.Cleanup(s.close)
	s.Transcript = append(s.Transcript,
		Message{Role: "user", Content: "objective and original detail"},
		Message{Role: "assistant", Content: "earlier evidence"},
		Message{Role: "assistant", ToolCalls: []ToolCall{regressionCall("m", "memory_edit")}},
	)
	result := s.editMemory("Preserved objective and earlier evidence.", 0)
	oldPath := s.archive.Name()
	s.Transcript = append(s.Transcript, Message{Role: "tool", ToolCallID: "m", Name: "memory_edit", Content: result})
	path := filepath.Join(t.TempDir(), "session.json")
	if err := saveSession(path, s); err != nil {
		t.Fatal(err)
	}
	loaded, err := loadSession(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(loaded.close)
	currentPath := loaded.archive.Name()
	if oldPath == currentPath {
		t.Fatal("reload reused a stale process-local archive path")
	}
	view := loaded.contextMessages()
	header, ok := view[1].Content.(string)
	if !ok || !strings.Contains(header, currentPath) || strings.Contains(header, oldPath) {
		t.Fatalf("current archive cannot be discovered from memory header: %q", header)
	}
	if !strings.Contains(view[3].Content.(string), oldPath) {
		t.Fatal("test did not retain the historical stale tool-report path")
	}
	if _, err := os.Stat(currentPath); err != nil {
		t.Fatalf("advertised current archive is unavailable: %v", err)
	}
	// Appending work must preserve the memory header byte-for-byte so the path
	// is discoverable without making every new request lose its stable prefix.
	loaded.Transcript = append(loaded.Transcript, Message{Role: "assistant", Content: "continue"})
	if next := loaded.contextMessages(); next[1].Content != header {
		t.Fatal("an append changed the archive/memory prefix")
	}
}

func TestMemoryDefaultCompactionCheckpointAndResume(t *testing.T) {
	withStreamDefaults(t)
	checkpoint := filepath.Join(t.TempDir(), "session.json")
	var phase atomic.Int32
	withSSEServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch phase.Add(1) {
		case 1:
			io.WriteString(w, toolCallChunk(t, 0, "m1", "memory_edit", `{"memory":"Finish the current objective; started work.","cursor":1}`)+deltaChunk(t, map[string]interface{}{}, "tool_calls"))
		case 2:
			io.WriteString(w, toolCallChunk(t, 0, "m2", "memory_edit", `{"memory":"Finish the current objective; earlier notes confirmed."}`)+deltaChunk(t, map[string]interface{}{}, "tool_calls"))
		case 3:
			var request struct {
				Messages []Message `json:"messages"`
			}
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Error(err)
			} else if len(request.Messages) != 4 || len(request.Messages[2].ToolCalls) != 1 || request.Messages[2].ToolCalls[0].ID != "m2" || request.Messages[3].ToolCallID != "m2" {
				t.Errorf("default compaction broke the in-flight tool group: %+v", request.Messages)
			}
			data, err := os.ReadFile(checkpoint)
			if err != nil {
				t.Error(err)
			} else {
				var saved Session
				if err := json.Unmarshal(data, &saved); err != nil || len(saved.Transcript) != 6 || saved.Cursor != 4 {
					t.Errorf("completed tool rounds were not checkpointed before requesting more work: %s; %v", data, err)
				}
			}
			io.WriteString(w, deltaChunk(t, map[string]interface{}{contentField: "done"}, "")+sseData("[DONE]"))
		default:
			io.WriteString(w, deltaChunk(t, map[string]interface{}{contentField: "resumed without repeating tools"}, "")+sseData("[DONE]"))
		}
	})
	s := newSession()
	t.Cleanup(s.close)
	s.saveFile = checkpoint
	s.Transcript = append(s.Transcript, Message{Role: "user", Content: "finish the current objective"})
	if err := runInferenceLoop(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	if len(s.Transcript) != 4 || s.messageCount() != 7 || s.Cursor != 4 {
		t.Fatalf("tool loop lost or retained absorbed history: %+v", s)
	}
	loaded, err := loadSession(checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(loaded.close)
	loaded.Transcript = append(loaded.Transcript, Message{Role: "user", Content: "continue from the checkpoint"})
	if err := runInferenceLoop(context.Background(), loaded); err != nil {
		t.Fatal(err)
	}
	if phase.Load() != 4 || loaded.messageCount() != 8 {
		t.Fatalf("resume repeated tools or lost checkpoint history: calls=%d messages=%d", phase.Load(), loaded.messageCount())
	}
}
