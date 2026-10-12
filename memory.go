package main

// Session: the agent's long-term state, three concepts kept strictly apart.
//
//	Transcript — append-only ground truth. Never rewritten, persisted verbatim.
//	Memory     — the model's own semantic snapshot, maintained via memory_edit.
//	Context    — the derived request view: system + memory header + Transcript[Cursor:].
//
// Because memory_edit only rewrites the Memory snapshot and the Cursor, the
// transcript can never lose facts, tool_call pairing or ordering (P0), and
// between two edits the view only grows by appends — the serialized request is
// a byte-prefix extension of the previous one, so provider prefix caches stay
// hot (P0-2).

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
)

const memoryHeader = "Semantic memory (your own notes, maintained via memory_edit; the older transcript they absorb is preserved in the session file):\n\n"

const sessionVersion = 3

var memoryMu sync.Mutex // serializes concurrent memory_edit tool calls

type Session struct {
	Version    int       `json:"version"`
	Transcript []Message `json:"transcript"`
	Memory     string    `json:"memory"`
	Cursor     int       `json:"cursor"` // Transcript[:Cursor] is absorbed into Memory
}

func newSession() *Session {
	return &Session{
		Transcript: []Message{{Role: "system", Content: systemRules()}},
		Cursor:     1,
	}
}

// contextMessages renders the request view. Transcript[0] (the system anchor)
// is always present; the memory header sits at a fixed slot right after it.
func (s *Session) contextMessages() []Message {
	out := make([]Message, 0, 2+len(s.Transcript)-s.Cursor)
	out = append(out, s.Transcript[0])
	if s.Memory != "" {
		out = append(out, Message{Role: "system", Content: memoryHeader + s.Memory})
	}
	return append(out, s.Transcript[s.Cursor:]...)
}

// contextBytes is the exact wire size of the messages array. There is no
// tokenizer on purpose: OpenAI-compatible APIs do not report window sizes and
// the project carries zero dependencies — byte observability plus the model's
// own compaction decision is the context-management mechanism.
func (s *Session) contextBytes() int {
	b, err := json.Marshal(s.contextMessages())
	if err != nil {
		return 0
	}
	return len(b)
}

func lastUserIndex(msgs []Message) int {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "user" {
			return i
		}
	}
	return -1
}

// editMemory applies one memory_edit tool call. The transcript is never
// touched. Ratchet rules, enforced in this order:
//   - the cursor is one-way: it never decreases (un-compaction would only
//     thrash provider caches);
//   - it never passes the newest user message, so the in-flight turn is
//     always fully inside the view;
//   - it never lands on a tool result, or the view would open with an
//     orphaned tool message and every provider would reject the request.
//
// A cursor of 0 (parameter omitted) keeps the current cursor: the call then
// only rewrites the memory text. Returns a report for the model.
func (s *Session) editMemory(memory string, cursor int) string {
	if strings.TrimSpace(memory) == "" {
		return "Error: memory text is required (write your full semantic memory; it replaces the previous snapshot)"
	}
	memoryMu.Lock()
	defer memoryMu.Unlock()

	old := s.Cursor
	maxCur := lastUserIndex(s.Transcript) // everything before the newest user message is fair game
	if maxCur < s.Cursor {
		maxCur = s.Cursor
	}
	if cursor < s.Cursor {
		cursor = s.Cursor // one-way ratchet
	}
	if cursor > maxCur {
		cursor = maxCur // the current turn is protected
	}
	for cursor < len(s.Transcript) && s.Transcript[cursor].Role == "tool" {
		cursor++ // never open the view on an orphaned tool result
	}

	s.Memory = memory
	s.Cursor = cursor
	absorbed := cursor - old
	view := s.contextMessages()
	return fmt.Sprintf("Memory updated (%d bytes). Cursor %d -> %d: %d transcript message(s) absorbed into memory; the transcript itself is untouched and persists in the session file. Context view: %d messages, %d bytes.",
		len(memory), old, cursor, absorbed, len(view), s.contextBytes())
}

// saveSession writes the session as one atomically replaced file: tmp file +
// rename, so a crash can never leave a torn JSON behind. One file, not three:
// cross-file atomicity would tear on crash (new transcript, old memory), and
// the context view is derived — persisting it would only invite divergence.
func saveSession(path string, s *Session) error {
	s.Version = sessionVersion
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(f)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(s); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}

// loadSession reads the v3 object format; legacy flat-array sessions
// (everything before v2.0) load as the transcript with nothing absorbed.
func loadSession(path string) (*Session, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return nil, nil
	}
	var s Session
	if trimmed[0] == '[' { // legacy flat array
		if err := json.Unmarshal(trimmed, &s.Transcript); err != nil {
			return nil, fmt.Errorf("invalid session JSON: %w", err)
		}
		s.Cursor = 1
		return &s, nil
	}
	if err := json.Unmarshal(trimmed, &s); err != nil {
		return nil, fmt.Errorf("invalid session JSON: %w", err)
	}
	if s.Cursor < 1 {
		s.Cursor = 1
	}
	if s.Cursor > len(s.Transcript) {
		s.Cursor = len(s.Transcript)
	}
	return &s, nil
}
