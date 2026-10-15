package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestLegacySessionAppendPreservesOriginalBytes(t *testing.T) {
	dir := t.TempDir()
	noImplicitTemp(t, dir)
	fixtures := []struct {
		name string
		body string
	}{
		{"array", ` [{"role":"system","content":"custom persona"},{"role":"user","content":"objective"},{"role":"assistant","content":"evidence"}]`},
		{"v3_transcript_first", ` {"transcript":[{"role":"system","content":"custom persona"},{"role":"user","content":"objective"},{"role":"assistant","content":"evidence"}],"version":3,"memory":"Retained objective","cursor":2}`},
		{"v3_memory_first", ` {"cursor":2,"memory":"Retained objective","version":3,"transcript":[{"role":"system","content":"custom persona"},{"role":"user","content":"objective"},{"role":"assistant","content":"evidence"}]}`},
	}
	for _, fixture := range fixtures {
		t.Run(fixture.name, func(t *testing.T) {
			path := filepath.Join(dir, fixture.name+".json")
			if err := os.WriteFile(path, []byte(fixture.body), 0o600); err != nil {
				t.Fatal(err)
			}
			s, err := loadSession(path)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(s.close)
			s.Transcript = append(s.Transcript, Message{Role: "user", Content: "continued objective"})
			if err := checkpointSession(path, s); err != nil {
				t.Fatal(err)
			}
			s.close()
			contents, err := os.ReadFile(path)
			if err != nil || !bytes.HasPrefix(contents, []byte(fixture.body)) {
				t.Fatalf("appending replaced the original legacy bytes: %v", err)
			}
			messages, state := regressionReadSavedState(t, nil, path)
			if state.Count != 4 || len(messages) != 4 || messages[0].Content != "custom persona" || messages[1].Content != "objective" || messages[2].Content != "evidence" || messages[3].Content != "continued objective" {
				t.Fatalf("legacy upgrade lost or duplicated history: %#v; %+v", messages, state)
			}
		})
	}
}

func TestJournalRestartIgnoresUncommittedTailAndRepairsOnSave(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.json")
	s := newSession()
	t.Cleanup(s.close)
	s.Transcript = append(s.Transcript, Message{Role: "user", Content: "initial objective"})
	if err := checkpointSession(path, s); err != nil {
		t.Fatal(err)
	}
	first, _ := regressionReadSavedState(t, s, path)
	firstOffset := s.diskOffset
	s.Transcript = append(s.Transcript, Message{Role: "assistant", Content: "uncommitted result"})
	if result := s.editMemory("Facts from the uncommitted second checkpoint.", 0); strings.HasPrefix(result, "Error") {
		t.Fatal(result)
	}
	s.close()
	complete, err := os.ReadFile(path)
	if err != nil || len(complete) == 0 || complete[len(complete)-1] != '\n' {
		t.Fatalf("missing checkpoint commit marker: %v", err)
	}
	// Tear the write in the middle of its first member: the message and the
	// state it carried stay uncommitted together.
	cut := bytes.Index(complete, []byte("\"m2\""))
	if cut < 0 || int64(cut) < firstOffset || cut+6 > len(complete) {
		t.Fatalf("cannot tear the document tail: %d; %d", cut, firstOffset)
	}
	interrupted := complete[:cut+6]
	if err := os.WriteFile(path, interrupted, 0o600); err != nil {
		t.Fatal(err)
	}
	restarted, err := loadSession(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(restarted.close)
	if restarted.messageCount() != 2 || restarted.Memory != "" || restarted.diskOffset != firstOffset || !reflect.DeepEqual(restarted.Transcript, first) {
		t.Fatalf("restart exposed an uncommitted checkpoint: %+v", restarted)
	}
	afterRead, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(afterRead, interrupted) {
		t.Fatalf("read-only recovery modified the interrupted source: %v", err)
	}
	replacement := Message{Role: "assistant", Content: "replacement after restart"}
	restarted.Transcript = append(restarted.Transcript, replacement)
	if err := checkpointSession(path, restarted); err != nil {
		t.Fatal(err)
	}
	restarted.close()
	messages, state := regressionReadSavedState(t, nil, path)
	want := append(append([]Message(nil), first...), replacement)
	if !reflect.DeepEqual(messages, want) || state.Count != 3 || state.Memory != "" {
		t.Fatalf("recovery did not continue exactly once: %#v; %+v", messages, state)
	}
	repaired, err := os.ReadFile(path)
	if err != nil || !bytes.HasPrefix(repaired, complete[:firstOffset]) || bytes.Contains(repaired, []byte("uncommitted result")) {
		t.Fatalf("repair changed committed bytes or retained the discarded checkpoint: %v", err)
	}
}

// Per-record checksums were removed in v5: a torn write is caught by the
// unterminated member, and same-length payload edits are accepted as a
// deliberate trade-off.

func TestSaveAsPreservesCompactedHistoryAndSource(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "original.json")
	copyPath := filepath.Join(dir, "copy.json")
	s := newSession()
	t.Cleanup(s.close)
	original := strings.Repeat("original raw output;", 1024)
	s.Transcript = append(s.Transcript,
		Message{Role: "user", Content: "original objective"},
		Message{Role: "assistant", ToolCalls: []ToolCall{regressionCall("exec", "exec")}},
		Message{Role: "tool", ToolCallID: "exec", Name: "exec", Content: original},
	)
	want := append([]Message(nil), s.Transcript...)
	if err := checkpointSession(path, s); err != nil {
		t.Fatal(err)
	}
	if result := s.editMemory("Objective and tool evidence retained.", 0); strings.HasPrefix(result, "Error") {
		t.Fatal(result)
	}
	next := Message{Role: "user", Content: "continue the objective"}
	s.Transcript = append(s.Transcript, next)
	want = append(want, next)
	if err := checkpointSession(path, s); err != nil {
		t.Fatal(err)
	}
	s.close()
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := loadSession(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(loaded.close)
	if err := checkpointSession(copyPath, loaded); err != nil {
		t.Fatal(err)
	}
	continued := Message{Role: "assistant", Content: "continued once in the copy"}
	loaded.Transcript = append(loaded.Transcript, continued)
	want = append(want, continued)
	if err := checkpointSession(copyPath, loaded); err != nil {
		t.Fatal(err)
	}
	loaded.close()
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("save-as altered the original session: %v", err)
	}
	copied, err := os.ReadFile(copyPath)
	if err != nil || !json.Valid(copied) {
		t.Fatalf("save-as did not produce one JSON document: %v", err)
	}
	messages, state := regressionReadSavedState(t, nil, copyPath)
	if !reflect.DeepEqual(messages, want) || state.Count != len(want) || state.Cursor != 4 || state.Base != 0 {
		t.Fatalf("save-as lost or duplicated compacted history: count=%d; %+v", len(messages), state)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 2 || entries[0].Name() != "copy.json" || entries[1].Name() != "original.json" {
		t.Fatalf("save-as created an implicit file: %v; %v", entries, err)
	}
}

func TestLateSavedJournalKeepsBaseAcrossRestartAndCompaction(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.json")
	s := newSession()
	t.Cleanup(s.close)
	s.Transcript = append(s.Transcript,
		Message{Role: "user", Content: "discarded original objective"},
		Message{Role: "assistant", Content: "discarded original evidence"},
	)
	if result := s.editMemory("Objective and evidence retained in summary.", 0); strings.HasPrefix(result, "Error") {
		t.Fatal(result)
	}
	next := Message{Role: "user", Content: "retained subsequent objective"}
	s.Transcript = append(s.Transcript, next)
	if err := checkpointSession(path, s); err != nil {
		t.Fatal(err)
	}
	s.close()
	restarted, err := loadSession(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(restarted.close)
	if err := checkpointSession(path, restarted); err != nil {
		t.Fatal(err)
	}
	if result := restarted.editMemory("Objective, evidence and subsequent work retained.", 0); strings.HasPrefix(result, "Error") {
		t.Fatal(result)
	}
	completion := Message{Role: "assistant", Content: "final completion"}
	restarted.Transcript = append(restarted.Transcript, completion)
	if err := checkpointSession(path, restarted); err != nil {
		t.Fatal(err)
	}
	restarted.close()
	messages, state := regressionReadSavedState(t, nil, path)
	if len(messages) != 3 || messages[1].Content != next.Content || messages[2].Content != completion.Content || state.Base != 2 || state.Count != 5 || state.Cursor != 4 {
		t.Fatalf("resumed late-saving journal lost its absolute base: %#v; %+v", messages, state)
	}
	loaded, err := loadSession(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(loaded.close)
	if len(loaded.Transcript) != 2 || loaded.messageCount() != 5 || loaded.Transcript[1].Content != completion.Content || loaded.Memory != restarted.Memory {
		t.Fatalf("second restart exposed discarded history or lost retained work: %+v", loaded)
	}
}

func TestCheckpointWriteFailureLeavesOldJournalReadable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.json")
	s := newSession()
	t.Cleanup(s.close)
	s.Transcript = append(s.Transcript, Message{Role: "user", Content: "retain the original objective"})
	if err := checkpointSession(path, s); err != nil {
		t.Fatal(err)
	}
	s.close()
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	readonly, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer readonly.Close()
	state, err := readSessionLog(readonly, nil)
	if err != nil {
		t.Fatal(err)
	}
	update := sessionUpdate{OpenDoc: state.DocOpen, Messages: []sessionMessage{
		{Index: state.Count, Message: Message{Role: "assistant", Content: "must never become visible"}},
	}}
	if offset, err := writeSessionUpdate(readonly, state.Offset, update); err == nil || offset != state.Offset {
		t.Fatalf("read-only write succeeded or changed commit offset: %d; %v", offset, err)
	}
	readonly.Close()
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("failed write altered committed bytes: %v", err)
	}
	restarted, err := loadSession(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(restarted.close)
	if restarted.messageCount() != 2 || restarted.Cursor != 1 || restarted.Memory != "" || restarted.Transcript[1].Content != "retain the original objective" {
		t.Fatalf("failed write changed restart state: %+v", restarted)
	}
}

func TestSessionDocumentHasNoVersionTag(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.json")
	s := newSession()
	t.Cleanup(s.close)
	s.Transcript = append(s.Transcript, Message{Role: "user", Content: "objective"})
	if err := checkpointSession(path, s); err != nil {
		t.Fatal(err)
	}
	s.close()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte("version")) {
		t.Fatalf("document carries a version tag: %s", raw)
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("document is not one JSON object: %v", err)
	}
	if _, ok := doc["m0"]; !ok {
		t.Fatalf("document lacks the system message: %s", raw)
	}
	if _, ok := doc["state"]; !ok {
		t.Fatalf("document lacks its state: %s", raw)
	}
	reloaded, err := loadSession(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(reloaded.close)
	reloaded.Transcript = append(reloaded.Transcript, Message{Role: "assistant", Content: "answer"})
	if err := checkpointSession(path, reloaded); err != nil {
		t.Fatal(err)
	}
	reloaded.close()
	raw, err = os.ReadFile(path)
	if err != nil || bytes.Contains(raw, []byte("version")) {
		t.Fatalf("appended document carries a version tag: %s; %v", raw, err)
	}
	messages, state := regressionReadSavedState(t, nil, path)
	if state.Count != 3 || len(messages) != 3 || messages[2].Content != "answer" {
		t.Fatalf("round trip lost messages: %#v; %+v", messages, state)
	}
}

func TestVersionedDocumentStillLoadsAndContinues(t *testing.T) {
	// A v2.5.0 document carries a leading "version": 5 member; it must keep
	// loading, and its continuation must append the current tagless form.
	path := filepath.Join(t.TempDir(), "v250.json")
	doc := "{\n  \"version\": 5,\n  \"m0\": {\"role\":\"system\",\"content\":\"old system\"}," +
		"\n  \"m1\": {\"role\":\"user\",\"content\":\"old task\"}," +
		"\n  \"state\": {\"base\":0,\"cursor\":1,\"memory\":\"\"}\n}\n"
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := loadSession(path)
	if err != nil {
		t.Fatalf("v2.5.0 load: %v", err)
	}
	t.Cleanup(s.close)
	if s.messageCount() != 2 || s.Transcript[1].Content != "old task" {
		t.Fatalf("v2.5.0 session wrong: %+v", s)
	}
	s.saveFile = path
	s.owner, err = reserveSession(path, true)
	if err != nil {
		t.Fatal(err)
	}
	s.ownsOwner = true
	s.Transcript = append(s.Transcript, Message{Role: "assistant", Content: "new answer"})
	if err := checkpointSession(path, s); err != nil {
		t.Fatalf("continue v2.5.0: %v", err)
	}
	s.close()
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// The append reopens the document by replacing its closing "\n}\n".
	committed := doc[:len(doc)-3]
	if !bytes.HasPrefix(after, []byte(committed)) {
		t.Fatalf("continuation rewrote the v2.5.0 bytes")
	}
	if appendix := after[len(committed):]; bytes.Contains(appendix, []byte("version")) {
		t.Fatalf("continuation reintroduced a version tag: %s", appendix)
	}
	restarted, err := loadSession(path)
	if err != nil {
		t.Fatalf("reload continued v2.5.0: %v", err)
	}
	t.Cleanup(restarted.close)
	if restarted.messageCount() != 3 || restarted.Transcript[2].Content != "new answer" {
		t.Fatalf("continued session wrong: %+v", restarted)
	}
}

func TestTornFirstWriteLoadsAsEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "torn.json")
	s := newSession()
	t.Cleanup(s.close)
	s.Transcript = append(s.Transcript, Message{Role: "user", Content: "objective"})
	if err := checkpointSession(path, s); err != nil {
		t.Fatal(err)
	}
	s.close()
	complete, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	cut := bytes.Index(complete, []byte("\"m0\""))
	if cut < 0 {
		t.Fatal("fixture lacks the system message")
	}
	if err := os.WriteFile(path, complete[:cut+4], 0o600); err != nil {
		t.Fatal(err)
	}
	if reloaded, err := loadSession(path); err != nil || reloaded != nil {
		t.Fatalf("torn first write did not load as empty: %v; %+v", err, reloaded)
	}
}
