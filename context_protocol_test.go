package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
)

// A strict compatible backend needs a conversational user anchor as well as
// complete tool groups. The persisted transcript need not retain that anchor.
func strictContextError(messages []Message) error {
	firstConversation := true
	v := transcriptValidator{}
	for _, message := range messages {
		if message.Role != "system" && firstConversation {
			if message.Role != "user" {
				return fmt.Errorf("conversation begins with %s", message.Role)
			}
			firstConversation = false
		}
		if err := v.add(message); err != nil {
			return err
		}
	}
	return v.finish()
}

func TestCompactionKeepsRequestsValidAcrossAutonomousToolRounds(t *testing.T) {
	withStreamDefaults(t)
	s := newSession()
	t.Cleanup(s.close)
	raw := strings.Repeat("consumed tool output;", 4096)
	s.Transcript = append(s.Transcript,
		Message{Role: "user", Content: "Complete the authorized objective"},
		Message{Role: "assistant", ToolCalls: []ToolCall{regressionCall("old", "exec")}},
		Message{Role: "tool", ToolCallID: "old", Name: "exec", Content: raw},
	)
	var round atomic.Int32
	withSSEServer(t, func(w http.ResponseWriter, r *http.Request) {
		var request ChatRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			http.Error(w, "invalid JSON", http.StatusBadRequest)
			return
		}
		if err := strictContextError(request.Messages); err != nil {
			t.Error(err)
			http.Error(w, "messages invalid", http.StatusBadRequest)
			return
		}
		n := round.Add(1)
		if n > 1 {
			if len(request.Messages) != 5 || request.Messages[2].Content != compactedContinuation || len(request.Messages[3].ToolCalls) != 1 ||
				request.Messages[3].ToolCalls[0].ID != fmt.Sprintf("memory%d", n-1) ||
				request.Messages[4].ToolCallID != fmt.Sprintf("memory%d", n-1) {
				t.Errorf("compaction retained earlier rounds or lost the pending group: %+v", request.Messages)
			}
			for _, message := range request.Messages {
				if content, ok := message.Content.(string); ok && strings.Contains(content, raw) {
					t.Error("consumed raw tool output survived compaction")
				}
			}
		}
		if n < 6 {
			args := fmt.Sprintf(`{"memory":"Authorized objective retained; round %d complete."}`, n)
			io.WriteString(w, toolCallChunk(t, 0, fmt.Sprintf("memory%d", n), "memory_edit", args)+deltaChunk(t, nil, "tool_calls"))
			return
		}
		io.WriteString(w, deltaChunk(t, map[string]interface{}{contentField: "done"}, "stop"))
	})
	if err := runInferenceLoop(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	if round.Load() != 6 || len(s.Transcript) != 4 || s.Transcript[1].Role != "assistant" || s.Transcript[2].Role != "tool" {
		t.Fatalf("compaction failed to release completed rounds: rounds=%d transcript=%+v", round.Load(), s.Transcript)
	}
	for _, message := range s.Transcript {
		if message.Content == compactedContinuation {
			t.Fatal("runtime continuation was written into original history")
		}
	}
}

func TestLoadCompactedCheckpointRestoresRequestProtocol(t *testing.T) {
	withStreamDefaults(t)
	// This is the shape produced by clearing memory in the reported v4 file:
	// the removed user message precedes an otherwise valid memory_edit pair.
	checkpoint := sessionCheckpoint{
		Version: 4, Base: 22, Cursor: 23, Memory: "Memory cleared as requested.",
		Messages: []Message{
			{Role: "system", Content: "Original system prompt"},
			{Role: "assistant", ToolCalls: []ToolCall{regressionCall("clear", "memory_edit")}},
			{Role: "tool", ToolCallID: "clear", Name: "memory_edit", Content: `{"result":"Cursor 1 -> 23; next=24."}`},
		},
	}
	raw, err := json.Marshal(checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	line, err := json.Marshal(sessionLogEnvelope{Checkpoint: raw, SHA256: fmt.Sprintf("%x", sha256.Sum256(raw))})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "reported-session.json")
	if err := os.WriteFile(path, append(line, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := loadSession(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.close)
	before := append([]Message(nil), s.Transcript...)
	view := s.contextMessages()
	if len(view) != 5 || view[2].Content != compactedContinuation || !reflect.DeepEqual(s.Transcript, before) {
		t.Fatalf("loading the old checkpoint did not restore the request without rewriting history: %+v", view)
	}
	withSSEServer(t, func(w http.ResponseWriter, r *http.Request) {
		var request ChatRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			return
		}
		if err := strictContextError(request.Messages); err != nil {
			t.Error(err)
			http.Error(w, "messages invalid", http.StatusBadRequest)
			return
		}
		io.WriteString(w, deltaChunk(t, map[string]interface{}{contentField: "Memory cleared."}, "stop"))
	})
	if err := runInferenceLoop(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(path)
	if err != nil || !reflect.DeepEqual(after, append(line, '\n')) {
		t.Fatalf("resuming inference unexpectedly rewrote the loaded file: %v", err)
	}
}

func TestContextContinuationOnlyPrecedesRetainedAssistant(t *testing.T) {
	for _, role := range []string{"", "user", "assistant"} {
		t.Run(role, func(t *testing.T) {
			s := newSession()
			if role != "" {
				s.Transcript = append(s.Transcript, Message{Role: role, Content: "retained"})
			}
			before := append([]Message(nil), s.Transcript...)
			view := s.contextMessages()
			want := 2 + len(s.Transcript) - 1
			if role == "assistant" {
				want++
				if view[2].Role != "user" || view[2].Content != compactedContinuation {
					t.Fatal("retained assistant lacks a runtime conversation anchor")
				}
			}
			if len(view) != want || !reflect.DeepEqual(s.Transcript, before) {
				t.Fatalf("request construction changed history: view=%+v transcript=%+v", view, s.Transcript)
			}
		})
	}
}
