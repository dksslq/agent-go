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

func TestFullyCompactedSnapshotRoundTrip(t *testing.T) {
	dir := t.TempDir()
	s := newSession()
	t.Cleanup(s.close)
	s.Transcript = append(s.Transcript, Message{Role: "user", Content: "task"}, Message{Role: "assistant", Content: "done"})
	if result := s.editMemory("Task completed.", 0); strings.HasPrefix(result, "Error") {
		t.Fatal(result)
	}
	path := filepath.Join(dir, "snapshot.json")
	if err := saveSession(path, s); err != nil {
		t.Fatal(err)
	}
	loaded, err := loadSession(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(loaded.close)
	if loaded.messageCount() != 3 || loaded.Cursor != 3 || loaded.archived != 2 || loaded.Memory != s.Memory || !reflect.DeepEqual(loaded.Transcript, s.Transcript) {
		t.Fatalf("compacted snapshot did not round trip: %+v", loaded)
	}
	copyPath := filepath.Join(dir, "copy.json")
	if err := saveSession(copyPath, loaded); err != nil {
		t.Fatal(err)
	}
	resumed, err := continueSession(copyPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(resumed.close)
	resumed.Transcript = append(resumed.Transcript, Message{Role: "user", Content: "next task"})
	if err := checkpointSession(copyPath, resumed); err != nil {
		t.Fatal(err)
	}
	resumed.close()
	messages, state := regressionReadSavedState(t, nil, copyPath)
	if len(messages) != 2 || messages[1].Content != "next task" || state.Base != 2 || state.Count != 4 || state.Cursor != 3 || state.Memory != s.Memory {
		t.Fatalf("resume lost the compaction gap: %+v; %+v", messages, state)
	}
}

func TestSnapshotNormalizesLegacyArchives(t *testing.T) {
	messages := []Message{{Role: "system", Content: "custom rules"}, {Role: "user", Content: "task"}, {Role: "assistant", Content: "evidence"}}
	array, err := json.Marshal(messages)
	if err != nil {
		t.Fatal(err)
	}
	cp, err := json.Marshal(sessionCheckpoint{Version: 4, Base: 2, Start: 0, Messages: messages, Memory: "Old facts.", Cursor: 4})
	if err != nil {
		t.Fatal(err)
	}
	envelope, err := json.Marshal(sessionLogEnvelope{Checkpoint: cp, SHA256: "legacy"})
	if err != nil {
		t.Fatal(err)
	}
	fixtures := []struct{ name, body string }{
		{"array", string(array)},
		{"v3", `{"version":3,"transcript":` + string(array) + `,"cursor":2,"memory":"Old facts."}`},
		{"v4", string(envelope) + "\n"},
	}
	for _, fixture := range fixtures {
		for _, continued := range []bool{false, true} {
			name := fixture.name
			if continued {
				name += "_continued"
			}
			t.Run(name, func(t *testing.T) {
				dir := t.TempDir()
				noImplicitTemp(t, dir)
				source, target := filepath.Join(dir, "old.json"), filepath.Join(dir, "new.json")
				if err := os.WriteFile(source, []byte(fixture.body), 0600); err != nil {
					t.Fatal(err)
				}
				s, err := loadSession(source)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(s.close)
				if continued {
					s.Transcript = append(s.Transcript, Message{Role: "user", Content: "continued work"})
					if err := checkpointSession(source, s); err != nil {
						t.Fatal(err)
					}
					s.close()
					s, err = loadSession(source)
					if err != nil {
						t.Fatal(err)
					}
					t.Cleanup(s.close)
				}
				before, err := os.ReadFile(source)
				if err != nil {
					t.Fatal(err)
				}
				want, oldState := regressionReadSavedState(t, nil, source)
				extra := Message{Role: "assistant", Content: "new answer"}
				s.Transcript = append(s.Transcript, extra)
				want = append(want, extra)
				if err := saveSession(target, s); err != nil {
					t.Fatal(err)
				}
				raw, err := os.ReadFile(target)
				if err != nil {
					t.Fatal(err)
				}
				if !json.Valid(raw) || bytes.Contains(raw, []byte(`"version"`)) || bytes.Contains(raw, []byte(`"sha256"`)) || bytes.Count(raw, []byte(`"state":`)) != 1 {
					t.Fatalf("snapshot is not one clean document: %s", raw)
				}
				after, err := os.ReadFile(source)
				if err != nil || !bytes.Equal(before, after) {
					t.Fatalf("source changed: %v", err)
				}
				got, state := regressionReadSavedState(t, nil, target)
				if !reflect.DeepEqual(got, want) || state.Base != oldState.Base || state.Count != oldState.Count+1 || state.Cursor != s.Cursor || state.Memory != s.Memory {
					t.Fatalf("snapshot lost history/state: %+v; %+v", got, state)
				}
				resumed, err := continueSession(target)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(resumed.close)
				resumed.Transcript = append(resumed.Transcript, Message{Role: "user", Content: "resume snapshot"})
				if err := checkpointSession(target, resumed); err != nil {
					t.Fatal(err)
				}
				resumed.close()
				final, err := os.ReadFile(target)
				if err != nil || !json.Valid(final) {
					t.Fatalf("continued snapshot is not whole JSON: %v", err)
				}
				_, finalState := regressionReadSavedState(t, nil, target)
				if finalState.Count != state.Count+1 {
					t.Fatalf("wrong continued count: %+v", finalState)
				}
			})
		}
	}
}
