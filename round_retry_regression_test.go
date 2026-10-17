package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
)

// Verifies the user-reported failure: a model tool call with malformed JSON
// arguments no longer terminates the process; the round is retried with the
// abort marker as feedback and the model self-corrects.
func TestMalformedToolCallRetriesInsteadOfStopping(t *testing.T) {
	withStreamDefaults(t)
	s := newSession()
	s.Transcript = append(s.Transcript, Message{Role: "user", Content: "fix the server"})
	var requests int32
	var sawMarker int32
	withSSEServer(t, func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&requests, 1)
		var body strings.Builder
		buf := make([]byte, 4096)
		for {
			k, err := r.Body.Read(buf)
			if k > 0 {
				body.Write(buf[:k])
			}
			if err != nil {
				break
			}
		}
		if strings.Contains(body.String(), "[Turn aborted: tool call") {
			atomic.StoreInt32(&sawMarker, 1)
			// DeepSeek rejects a thinking-mode request whose tail is an
			// assistant message without reasoning_content; the abort
			// feedback must therefore ride on a user message.
			var req struct {
				Messages []Message `json:"messages"`
			}
			if err := json.Unmarshal([]byte(body.String()), &req); err != nil {
				t.Errorf("bad retry request: %v", err)
			} else if n := len(req.Messages); n > 0 && req.Messages[n-1].Role == "assistant" {
				t.Errorf("retry request tail is an assistant message: %+v", req.Messages[n-1])
			}
		}
		if n == 1 {
			// Exactly the reported shape: tool call whose streamed arguments
			// are truncated, with finish_reason=tool_calls.
			fmt.Fprint(w, toolCallChunk(t, 0, "call_00_5PJnTJOqBE7yLhNHIJdz3642", "exec", `{"program":"bash","args":["-c","echo hi`)+"\n\n")
			fmt.Fprint(w, `data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`+"\n\n")
			fmt.Fprint(w, "data: [DONE]\n\n")
			return
		}
		fmt.Fprint(w, deltaChunk(t, map[string]interface{}{reasoningField: "thought", contentField: "recovered"}, ""))
		fmt.Fprint(w, deltaChunk(t, map[string]interface{}{reasoningField: " then ", contentField: " and completed"}, "stop"))
		fmt.Fprint(w, "data: [DONE]\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	})
	if err := runInferenceLoop(context.Background(), s); err != nil {
		t.Fatalf("program stopped on malformed tool call: %v", err)
	}
	if atomic.LoadInt32(&requests) != 2 {
		t.Fatalf("expected one self-correction retry, got %d requests", requests)
	}
	if atomic.LoadInt32(&sawMarker) != 1 {
		t.Fatalf("retry request did not include the abort marker feedback")
	}
	var feedback, answers, reasoning strings.Builder
	for _, m := range s.Transcript {
		if c, ok := m.Content.(string); ok {
			switch m.Role {
			case "user":
				feedback.WriteString(c)
				feedback.WriteString("\n")
			case "assistant":
				answers.WriteString(c)
				answers.WriteString("\n")
				reasoning.WriteString(m.ReasoningContent)
			}
		}
	}
	if !strings.Contains(feedback.String(), "[Turn aborted: tool call") {
		t.Fatalf("transcript missing user-role abort marker feedback: %q", feedback.String())
	}
	if !strings.Contains(answers.String(), "recovered and completed") {
		t.Fatalf("transcript missing final answer: %q", answers.String())
	}
	if reasoning.String() != "thought then " {
		t.Fatalf("assistant messages must carry back the streamed reasoning, got %q", reasoning.String())
	}
}
