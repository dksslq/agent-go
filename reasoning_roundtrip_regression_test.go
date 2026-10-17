package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// DeepSeek thinking mode with tools requires the assistant chain of thought
// to be passed back; without it the API returns 400 "reasoning_content must
// be passed back". This covers the full path: streamed reasoning is kept on
// the assistant message, persisted with the session document, restored on
// load, and marshalled back into subsequent requests.
func TestReasoningContentPersistsAndReturns(t *testing.T) {
	withStreamDefaults(t)
	s := newSession()
	s.Transcript = append(s.Transcript, Message{Role: "user", Content: "hi"})
	withSSEServer(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, deltaChunk(t, map[string]interface{}{reasoningField: "count "}, ""))
		fmt.Fprint(w, deltaChunk(t, map[string]interface{}{reasoningField: "letters"}, ""))
		fmt.Fprint(w, deltaChunk(t, map[string]interface{}{contentField: "hello"}, "stop"))
		fmt.Fprint(w, "data: [DONE]\n\n")
	})
	if err := runInferenceLoop(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	last := s.Transcript[len(s.Transcript)-1]
	if last.Role != "assistant" || last.ReasoningContent != "count letters" {
		t.Fatalf("assistant message lost streamed reasoning: %+v", last)
	}

	path := filepath.Join(t.TempDir(), "session.json")
	s.saveFile = path
	owner, err := reserveSession(path, true)
	if err != nil {
		t.Fatal(err)
	}
	s.owner, s.ownsOwner = owner, true
	if err := checkpointSession(path, s); err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	s.close()

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"reasoning_content":"count letters"`) {
		t.Fatalf("session document omitted reasoning_content: %s", raw)
	}
	loaded, err := loadSession(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(loaded.close)
	restored := loaded.Transcript[len(loaded.Transcript)-1]
	if restored.Role != "assistant" || restored.ReasoningContent != "count letters" {
		t.Fatalf("load dropped reasoning_content: %+v", restored)
	}
	data, err := json.Marshal(ChatRequest{Model: "m", Messages: loaded.contextMessages()})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"reasoning_content":"count letters"`) {
		t.Fatalf("subsequent request omits reasoning_content: %s", data)
	}
	// Non-thinking turns never emit the field at all.
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(data, &probe); err != nil {
		t.Fatal(err)
	}
	for _, m := range loaded.contextMessages() {
		if m.Role == "user" || m.Role == "system" {
			if m.ReasoningContent != "" {
				t.Fatalf("non-assistant message carries reasoning: %+v", m)
			}
		}
	}
}
