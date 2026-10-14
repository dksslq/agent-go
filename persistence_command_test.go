package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func commandSession(t *testing.T, command string, session **Session) {
	t.Helper()
	handled, err := handleCommand(command, session)
	if !handled || err != nil {
		t.Fatalf("command %q: handled=%v error=%v", command, handled, err)
	}
}

func readSessionBytes(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func assertSessionClosed(t *testing.T, path string) {
	t.Helper()
	// On Windows this fails if any prior exclusive session handle survived.
	owner, err := reserveSession(path, false)
	if err != nil {
		t.Fatalf("session handle was not released: %v", err)
	}
	if err := owner.close(); err != nil {
		t.Fatal(err)
	}
}

// The session's own paths must come from the caller's path, never from the
// handle name: on Windows an exclusively opened handle carries a \\?\ prefix
// that then misses the owner on every later comparison (history_read, source
// reads, checkpoint adoption).
func TestDecodeSessionUsesGivenPathNotHandleName(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.json")
	s := newSession()
	s.Transcript = append(s.Transcript, Message{Role: "user", Content: "recorded"})
	if err := checkpointSession(path, s); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	poisoned := os.NewFile(f.Fd(), `\\?\poisoned-handle-name`)
	t.Cleanup(func() { _ = poisoned.Close() })
	absolute, err := filepath.Abs(path)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := decodeSession(poisoned, absolute)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.sourcePath != absolute || loaded.historyPath != absolute {
		t.Fatalf("session paths derived from the handle name: source=%q history=%q", loaded.sourcePath, loaded.historyPath)
	}
}

// A failed checkpoint stays fail-closed for its own file, but must not poison
// unrelated recovery targets: a snapshot or a redirect only ever copies the
// failed file's last committed prefix.
func TestCheckpointFailureStaysScopedToItsFile(t *testing.T) {
	dir := t.TempDir()
	s, err := continueSession(filepath.Join(dir, "active.json"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.close)
	s.Transcript = append(s.Transcript, Message{Role: "user", Content: "objective"})
	if err := checkpointSession(s.saveFile, s); err != nil {
		t.Fatal(err)
	}
	injected := errors.New("injected checkpoint failure")
	s.checkpointErr, s.checkpointErrPath = injected, s.saveFile
	if err := checkpointSession(s.saveFile, s); err == nil {
		t.Fatal("a failed file lost its fail-closed guard")
	}
	if result := s.editMemory("Recovered memory.", 0); !strings.Contains(result, "injected") {
		t.Fatalf("editMemory bypassed the failed file's guard: %q", result)
	}
	recovery := filepath.Join(dir, "recovery.json")
	if err := saveSession(recovery, s); err != nil {
		t.Fatalf("stale failure poisoned an unrelated snapshot: %v", err)
	}
	messages, state := regressionReadSavedState(t, nil, recovery)
	if state.Count != 2 || messages[1].Content != "objective" {
		t.Fatalf("recovery snapshot lost the last committed state: %#v; %+v", messages, state)
	}
	if err := checkpointSession(filepath.Join(dir, "redirect.json"), s); err != nil {
		t.Fatalf("stale failure poisoned redirecting checkpoints: %v", err)
	}
}

func TestSnapshotClosesFileAndNeverAutosaves(t *testing.T) {
	dir := t.TempDir()
	noImplicitTemp(t, dir)
	path := filepath.Join(dir, "snapshot.json")
	s := newSession()
	t.Cleanup(s.close)
	s.Transcript = append(s.Transcript, Message{Role: "user", Content: "original objective"})
	if err := saveSession(path, s); err != nil {
		t.Fatal(err)
	}
	if s.owner != nil || s.saveFile != "" {
		t.Fatal("saving a snapshot enabled continuous persistence")
	}
	assertSessionClosed(t, path)
	before := readSessionBytes(t, path)
	s.Transcript = append(s.Transcript, Message{Role: "assistant", Content: "later evidence"})
	if result := s.editMemory("Objective and later evidence retained.", 0); strings.HasPrefix(result, "Error") {
		t.Fatal(result)
	}
	if after := readSessionBytes(t, path); !bytes.Equal(before, after) {
		t.Fatal("memory_edit silently updated a snapshot")
	}
	assertSessionClosed(t, path)
	onlyExplicitFile(t, dir, "snapshot.json")
}

func TestSnapshotPreservesContinuousBindingAndCompactedOriginals(t *testing.T) {
	dir := t.TempDir()
	source, snapshot := filepath.Join(dir, "continued.json"), filepath.Join(dir, "snapshot.json")
	s, err := continueSession(source)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.close)
	s.Transcript = append(s.Transcript,
		Message{Role: "user", Content: "original objective"},
		Message{Role: "assistant", Content: "raw evidence retained on disk"},
	)
	if err := checkpointSession(s.saveFile, s); err != nil {
		t.Fatal(err)
	}
	if result := s.editMemory("Objective and raw evidence summarized.", 0); strings.HasPrefix(result, "Error") {
		t.Fatal(result)
	}
	s.Transcript = append(s.Transcript, Message{Role: "user", Content: "next objective"})
	if err := checkpointSession(s.saveFile, s); err != nil {
		t.Fatal(err)
	}
	owner, saveFile, sourcePath := s.owner, s.saveFile, s.sourcePath
	diskBase, diskCount, diskOffset := s.diskBase, s.diskCount, s.diskOffset
	want, _ := regressionReadSavedState(t, s, source)
	if err := saveSession(snapshot, s); err != nil {
		t.Fatal(err)
	}
	if s.owner != owner || s.saveFile != saveFile || s.sourcePath != sourcePath || s.diskBase != diskBase || s.diskCount != diskCount || s.diskOffset != diskOffset {
		t.Fatal("snapshot changed the continuing session's binding or journal state")
	}
	assertSessionClosed(t, snapshot)
	snapshotBefore := readSessionBytes(t, snapshot)
	got, _ := regressionReadSavedState(t, nil, snapshot)
	if !reflect.DeepEqual(got, want) {
		t.Fatal("snapshot lost compacted original history")
	}
	s.Transcript = append(s.Transcript, Message{Role: "assistant", Content: "later continuation"})
	if err := checkpointSession(s.saveFile, s); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(snapshotBefore, readSessionBytes(t, snapshot)) {
		t.Fatal("continuation appended to the snapshot")
	}
	continued, _ := regressionReadSavedState(t, s, source)
	if len(continued) != len(want)+1 || continued[len(continued)-1].Content != "later continuation" {
		t.Fatal("saving a snapshot disabled or redirected continuous persistence")
	}
}

func TestSnapshotConflictLeavesSessionAndExistingFileIntact(t *testing.T) {
	dir := t.TempDir()
	s, err := continueSession(filepath.Join(dir, "active.json"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.close)
	target := filepath.Join(dir, "existing.json")
	const original = "user-owned existing contents"
	if err := os.WriteFile(target, []byte(original), 0600); err != nil {
		t.Fatal(err)
	}
	owner, saveFile, sourcePath := s.owner, s.saveFile, s.sourcePath
	transcript := append([]Message(nil), s.Transcript...)
	if err := saveSession(target, s); err == nil {
		t.Fatal("snapshot overwrote an existing file")
	}
	if string(readSessionBytes(t, target)) != original || s.owner != owner || s.saveFile != saveFile || s.sourcePath != sourcePath || !reflect.DeepEqual(s.Transcript, transcript) || s.checkpointErr != nil {
		t.Fatal("failed snapshot changed its target or poisoned the active session")
	}
	if err := saveSession(s.saveFile, s); err == nil {
		t.Fatal("snapshot accepted the active continuation file as a new target")
	}
	s.Transcript = append(s.Transcript, Message{Role: "user", Content: "still usable"})
	if err := checkpointSession(s.saveFile, s); err != nil {
		t.Fatalf("snapshot conflict broke further checkpoints: %v", err)
	}
}

func TestLoadCommandsReleaseBindingAndReadOnlySource(t *testing.T) {
	for _, command := range []string{"/load", "/reload"} {
		t.Run(command, func(t *testing.T) {
			dir := t.TempDir()
			activePath, loadPath := filepath.Join(dir, "active.json"), filepath.Join(dir, "loaded.json")
			fixture := newSession()
			fixture.Transcript = append(fixture.Transcript, Message{Role: "user", Content: "loaded objective"})
			if err := saveSession(loadPath, fixture); err != nil {
				t.Fatal(err)
			}
			fixture.close()
			before := readSessionBytes(t, loadPath)
			s, err := continueSession(activePath)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { s.close() })
			oldOwner := s.owner
			commandSession(t, command+" "+loadPath, &s)
			if oldOwner.file != nil || s.owner != nil || s.saveFile != "" || s.Transcript[len(s.Transcript)-1].Content != "loaded objective" {
				t.Fatal("loading retained ownership or failed to replace the session")
			}
			assertSessionClosed(t, activePath)
			assertSessionClosed(t, loadPath)
			s.Transcript = append(s.Transcript, Message{Role: "assistant", Content: "unsaved later evidence"})
			if result := s.editMemory("Loaded objective and later evidence retained.", 0); strings.HasPrefix(result, "Error") {
				t.Fatal(result)
			}
			if !bytes.Equal(before, readSessionBytes(t, loadPath)) {
				t.Fatal("read-only loading enabled writes to its source")
			}
		})
	}
}

func TestContinueCommandSwitchesBindingAndFailuresRollBack(t *testing.T) {
	dir := t.TempDir()
	first, second := filepath.Join(dir, "first.json"), filepath.Join(dir, "second.json")
	s, err := continueSession(first)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.close() })
	old, owner := s, s.owner
	invalid := filepath.Join(dir, "invalid.json")
	if err := os.WriteFile(invalid, []byte("invalid session"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, command := range []string{"/load", "/reload", "/continue"} {
		handled, err := handleCommand(command+" "+invalid, &s)
		if !handled || (command == "/continue" && !errors.Is(err, errCheckpoint)) || (command != "/continue" && err != nil) {
			t.Fatalf("failed %s returned the wrong command status: handled=%v error=%v", command, handled, err)
		}
		if s != old || s.owner != owner || owner.file == nil || !owner.matches(first) {
			t.Fatalf("failed %s changed the active session or released its owner", command)
		}
		assertSessionClosed(t, invalid)
	}
	commandSession(t, "/continue "+second, &s)
	if s == old || owner.file != nil || s.owner == nil || !s.owner.matches(second) || s.saveFile != s.owner.path {
		t.Fatal("continue did not transfer continuous ownership to its selected file")
	}
	assertSessionClosed(t, first)
	s.Transcript = append(s.Transcript, Message{Role: "user", Content: "continued objective"})
	if err := checkpointSession(s.saveFile, s); err != nil {
		t.Fatal(err)
	}
	commandSession(t, "/continue "+second, &s)
	if s.owner == nil || !s.owner.matches(second) || s.Transcript[len(s.Transcript)-1].Content != "continued objective" {
		t.Fatal("continuing the current file failed to reload it or retain ownership")
	}
}

func TestLoadCurrentContinuationClosesExclusiveHandle(t *testing.T) {
	path := filepath.Join(t.TempDir(), "active.json")
	s, err := continueSession(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.close() })
	s.Transcript = append(s.Transcript, Message{Role: "user", Content: "saved objective"})
	if err := checkpointSession(s.saveFile, s); err != nil {
		t.Fatal(err)
	}
	owner := s.owner
	commandSession(t, "/load "+path, &s)
	if owner.file != nil || s.owner != nil || s.saveFile != "" || s.Transcript[len(s.Transcript)-1].Content != "saved objective" {
		t.Fatal("loading the active continuation kept its exclusive handle")
	}
	assertSessionClosed(t, path)
}
