package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestV4JournalContinuesAsV5(t *testing.T) {
	// Hand-built v4 journal: one initial checkpoint record (old envelope format).
	path := filepath.Join(t.TempDir(), "v4session.json")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	cp := sessionCheckpoint{Version: 4, Base: 0, Start: 0,
		Messages: []Message{
			{Role: "system", Content: "old system"},
			{Role: "user", Content: "old objective"},
		}, Memory: "", Cursor: 1}
	raw, _ := json.Marshal(cp)
	line, _ := json.Marshal(sessionLogEnvelope{Checkpoint: raw, SHA256: "0000"})
	if _, err := f.Write(append([]byte{'\n'}, append(line, '\n')...)); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(path)
	s, err := loadSession(path)
	if err != nil {
		t.Fatalf("v4 load: %v", err)
	}
	t.Cleanup(s.close)
	if s.messageCount() != 2 {
		t.Fatalf("v4 count: %d", s.messageCount())
	}
	// Continue: append a v5 document after the journal.
	s.saveFile = path
	s.owner, err = reserveSession(path, true)
	if err != nil {
		t.Fatal(err)
	}
	s.ownsOwner = true
	s.Transcript = append(s.Transcript, Message{Role: "assistant", Content: "new answer"})
	if err := checkpointSession(path, s); err != nil {
		t.Fatalf("v5 migrate append: %v", err)
	}
	s.close()
	after, _ := os.ReadFile(path)
	if string(after[:len(before)]) != string(before) {
		t.Fatalf("migration rewrote the v4 prefix")
	}
	fmt.Printf("migrated file:\n%s\n", after[len(before):])
	restarted, err := loadSession(path)
	if err != nil {
		t.Fatalf("reload migrated: %v", err)
	}
	t.Cleanup(restarted.close)
	if restarted.messageCount() != 3 || restarted.Transcript[1].Content != "old objective" || restarted.Transcript[2].Content != "new answer" {
		t.Fatalf("migrated session wrong: %+v", restarted)
	}
	restarted.Transcript = append(restarted.Transcript, Message{Role: "user", Content: "more"})
	if err := checkpointSession(path, restarted); err != nil {
		t.Fatal(err)
	}
	restarted.close()
	final, err := loadSession(path)
	if err != nil {
		t.Fatal(err)
	}
	final.close()
	if final.messageCount() != 4 {
		t.Fatalf("second append on migrated file: %d", final.messageCount())
	}
}
