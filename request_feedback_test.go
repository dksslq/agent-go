package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"reflect"
	"sync/atomic"
	"testing"
)

func TestInferenceRequestFeedbackPreservesHistoryAndSystemPrefix(t *testing.T) {
	withStreamDefaults(t)
	extraRequestFields = []map[string]json.RawMessage{{"temperature": json.RawMessage(`0.3`)}}
	sess := newSession()
	defer sess.close()
	sess.Transcript = append(sess.Transcript,
		Message{Role: "user", Content: "earlier task"},
		Message{Role: "assistant", ToolCalls: []ToolCall{{ID: "old", Type: "function", Function: FunctionCall{Name: "old_tool", Arguments: `{}`}}}},
		Message{Role: "tool", ToolCallID: "old", Name: "old_tool", Content: wrapResult(1, 2, "earlier result")},
		Message{Role: "assistant", Content: "earlier task completed"},
		Message{Role: "user", Content: "continue with independent checks: 你好"},
	)
	original := append([]Message(nil), sess.Transcript...)
	expected := sess.contextMessages()
	type capturedRequest struct {
		bytes    int
		messages []Message
	}
	requests := make(chan capturedRequest, 2)
	var count atomic.Int32
	withSSEServer(t, func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
		}
		var request ChatRequest
		if err := json.Unmarshal(body, &request); err != nil {
			t.Error(err)
		}
		requests <- capturedRequest{len(body), request.Messages}
		if count.Add(1) == 1 {
			io.WriteString(w, toolCallChunk(t, 0, "one", "unknown_first", `{}`)+
				toolCallChunk(t, 1, "two", "unknown_second", `{}`)+
				deltaChunk(t, nil, "tool_calls"))
			return
		}
		io.WriteString(w, deltaChunk(t, map[string]interface{}{contentField: "done"}, "stop"))
	})
	if err := runInferenceLoop(context.Background(), sess); err != nil {
		t.Fatal(err)
	}
	if count.Load() != 2 {
		t.Fatalf("inference made %d requests, want 2", count.Load())
	}
	first, second := <-requests, <-requests
	if !reflect.DeepEqual(first.messages, expected) || len(second.messages) != len(expected)+3 ||
		!reflect.DeepEqual(second.messages[:len(expected)], expected) ||
		!reflect.DeepEqual(sess.Transcript[:len(original)], original) {
		t.Fatal("request feedback changed the system prefix or earlier transcript messages")
	}
	firstTool, lastTool := second.messages[len(expected)+1], second.messages[len(expected)+2]
	if firstTool.Role != "tool" || firstTool.ToolCallID != "one" || lastTool.Role != "tool" || lastTool.ToolCallID != "two" {
		t.Fatalf("tool result order changed: %+v %+v", firstTool, lastTool)
	}
	for _, msg := range []Message{second.messages[4], firstTool} {
		var result map[string]json.RawMessage
		if err := json.Unmarshal([]byte(msg.Content.(string)), &result); err != nil {
			t.Fatal(err)
		}
		if _, exists := result["last_request_bytes"]; exists {
			t.Fatal("request feedback was attached to an earlier tool result")
		}
	}
	var feedback map[string]json.RawMessage
	if err := json.Unmarshal([]byte(lastTool.Content.(string)), &feedback); err != nil {
		t.Fatal(err)
	}
	var measured int
	if err := json.Unmarshal(feedback["last_request_bytes"], &measured); err != nil || measured != first.bytes {
		t.Fatalf("last_request_bytes=%d, first HTTP payload=%d; error=%v", measured, first.bytes, err)
	}
	for key := range feedback {
		switch key {
		case "start_unix", "end_unix", "result", "last_request_bytes":
		default:
			t.Fatalf("unexpected inferred token/window feedback field: %s", key)
		}
	}
	if !reflect.DeepEqual(sess.Transcript[len(original)+1:len(original)+3], []Message{firstTool, lastTool}) {
		t.Fatal("later inference rewrote completed tool results")
	}
}

func TestSavedHistoryFeedbackPreservesRequestPrefix(t *testing.T) {
	for _, mode := range []string{"continue", "snapshot", "loaded"} {
		t.Run(mode, func(t *testing.T) {
			withStreamDefaults(t)
			path := filepath.Join(t.TempDir(), "session.json")
			sess := newSession()
			if mode == "continue" {
				var err error
				sess, err = continueSession(path)
				if err != nil {
					t.Fatal(err)
				}
			}
			sess.Transcript = append(sess.Transcript, Message{Role: "user", Content: "Complete independent checks"})
			if mode != "continue" {
				if err := saveSession(path, sess); err != nil {
					t.Fatal(err)
				}
				if mode == "loaded" {
					var err error
					sess, err = loadSession(path)
					if err != nil {
						t.Fatal(err)
					}
				}
			}
			t.Cleanup(sess.close)
			type capturedRequest struct {
				bytes      int
				messages   []Message
				historyEnd int
			}
			requests := make(chan capturedRequest, 4)
			var count atomic.Int32
			withSSEServer(t, func(w http.ResponseWriter, r *http.Request) {
				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Error(err)
					return
				}
				var request ChatRequest
				if err := json.Unmarshal(body, &request); err != nil {
					t.Error(err)
					return
				}
				// Verify the advertised bound against committed records, including
				// Windows where the active owner must supply the read handle.
				var saved sessionLogState
				func() {
					sess.mu.Lock()
					defer sess.mu.Unlock()
					_, saved = regressionReadSavedState(t, sess, path)
				}()
				requests <- capturedRequest{len(body), request.Messages, saved.Count}
				if count.Add(1) < 4 {
					io.WriteString(w, toolCallChunk(t, 0, "one", "unknown_first", `{}`)+
						toolCallChunk(t, 1, "two", "unknown_second", `{}`)+deltaChunk(t, nil, "tool_calls"))
					return
				}
				io.WriteString(w, deltaChunk(t, map[string]interface{}{contentField: "done"}, "stop"))
			})
			if err := runInferenceLoop(context.Background(), sess); err != nil {
				t.Fatal(err)
			}
			if count.Load() != 4 {
				t.Fatalf("inference made %d requests, want 4", count.Load())
			}
			previous := <-requests
			for round := 1; round < 4; round++ {
				current := <-requests
				before, _ := json.Marshal(previous.messages)
				after, _ := json.Marshal(current.messages)
				if len(current.messages) != len(previous.messages)+3 || !bytes.HasPrefix(after, before[:len(before)-1]) {
					t.Fatalf("round %d changed the cached system/history prefix", round)
				}
				var feedback ToolResult
				last := current.messages[len(current.messages)-1]
				if err := json.Unmarshal([]byte(last.Content.(string)), &feedback); err != nil {
					t.Fatal(err)
				}
				if feedback.HistoryNext != current.historyEnd || feedback.LastRequestBytes != previous.bytes {
					t.Fatalf("round %d: history_next=%d, saved end=%d; last_request_bytes=%d, prior request=%d", round,
						feedback.HistoryNext, current.historyEnd, feedback.LastRequestBytes, previous.bytes)
				}
				var first map[string]json.RawMessage
				if err := json.Unmarshal([]byte(current.messages[len(current.messages)-2].Content.(string)), &first); err != nil {
					t.Fatal(err)
				}
				if _, exists := first["history_next"]; exists {
					t.Fatal("history feedback was attached to multiple tool results")
				}
				previous = current
			}
		})
	}
}
