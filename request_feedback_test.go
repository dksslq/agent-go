package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"sync/atomic"
	"testing"
)

func TestInferenceRequestFeedbackPreservesHistoryAndSystemPrefix(t *testing.T) {
	withStreamDefaults(t)
	extraRequestFields = []map[string]interface{}{{"temperature": 0.3}}
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
	if !reflect.DeepEqual(first.messages, original) || len(second.messages) != len(original)+3 ||
		!reflect.DeepEqual(second.messages[:len(original)], original) ||
		!reflect.DeepEqual(sess.Transcript[:len(original)], original) {
		t.Fatal("request feedback changed the system prefix or earlier transcript messages")
	}
	firstTool, lastTool := second.messages[len(original)+1], second.messages[len(original)+2]
	if firstTool.Role != "tool" || firstTool.ToolCallID != "one" || lastTool.Role != "tool" || lastTool.ToolCallID != "two" {
		t.Fatalf("tool result order changed: %+v %+v", firstTool, lastTool)
	}
	for _, msg := range []Message{second.messages[3], firstTool} {
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
