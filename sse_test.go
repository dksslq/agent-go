package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// ---------- helpers ----------

func withStreamDefaults(t *testing.T) {
	t.Helper()
	oldAPI, oldKey, oldModel := apiBase, apiKey, modelName
	oldR, oldC, oldTC, oldFR := reasoningField, contentField, toolCallsField, finishReasonField
	oldExtra, oldClient := extraRequestFields, httpClient
	oldIdle, oldRounds := toolCallIdleTimeout, maxToolRounds

	apiKey, modelName = "test-key", "test-model"
	reasoningField, contentField, toolCallsField, finishReasonField =
		"reasoning_content", "content", "tool_calls", "finish_reason"
	extraRequestFields = nil
	httpClient = &http.Client{}
	toolCallIdleTimeout = time.Minute

	t.Cleanup(func() {
		apiBase, apiKey, modelName = oldAPI, oldKey, oldModel
		reasoningField, contentField, toolCallsField, finishReasonField = oldR, oldC, oldTC, oldFR
		extraRequestFields, httpClient = oldExtra, oldClient
		toolCallIdleTimeout, maxToolRounds = oldIdle, oldRounds
	})
}

func withSSEServer(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	apiBase = srv.URL
	return srv
}

func sseData(payload string) string { return "data: " + payload + "\n\n" }

func deltaChunk(t *testing.T, delta map[string]interface{}, finish string) string {
	t.Helper()
	choice := map[string]interface{}{"delta": delta}
	if finish != "" {
		choice[finishReasonField] = finish
	}
	b, err := json.Marshal(map[string]interface{}{"choices": []interface{}{choice}})
	if err != nil {
		t.Fatal(err)
	}
	return sseData(string(b))
}

func toolCallChunk(t *testing.T, index int, id, name, args string) string {
	t.Helper()
	tc := map[string]interface{}{"index": float64(index)}
	if id != "" {
		tc["id"] = id
	}
	if name != "" || args != "" {
		fn := map[string]interface{}{}
		if name != "" {
			fn["name"] = name
		}
		if args != "" {
			fn["arguments"] = args
		}
		tc["function"] = fn
	}
	return deltaChunk(t, map[string]interface{}{toolCallsField: []interface{}{tc}}, "")
}

func collectStream(t *testing.T, msgs []Message) ([]StreamEvent, error) {
	t.Helper()
	ch, cancel, err := chatStream(context.Background(), msgs)
	if err != nil {
		return nil, err
	}
	defer cancel()
	var evs []StreamEvent
	for e := range ch {
		evs = append(evs, e)
	}
	return evs, nil
}

func assertEvents(t *testing.T, got, want []StreamEvent) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("event count = %d, want %d; got %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i].Type != want[i].Type {
			t.Fatalf("event[%d].Type = %v, want %v", i, got[i].Type, want[i].Type)
		}
		if want[i].Text != "" && got[i].Text != want[i].Text {
			t.Fatalf("event[%d].Text = %q, want %q", i, got[i].Text, want[i].Text)
		}
		if want[i].ToolCalls != nil {
			g, _ := json.Marshal(got[i].ToolCalls)
			w, _ := json.Marshal(want[i].ToolCalls)
			if string(g) != string(w) {
				t.Fatalf("event[%d].ToolCalls:\n got %s\nwant %s", i, g, w)
			}
		}
	}
}

// ---------- streaming: happy paths ----------

func TestChatStreamReasoningAndContent(t *testing.T) {
	withStreamDefaults(t)
	withSSEServer(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w,
			deltaChunk(t, map[string]interface{}{reasoningField: "think"}, "")+
				deltaChunk(t, map[string]interface{}{contentField: "Hello "}, "")+
				deltaChunk(t, map[string]interface{}{contentField: "world"}, "")+
				sseData("[DONE]"))
	})
	evs, err := collectStream(t, []Message{{Role: "user", Content: "hi"}})
	if err != nil {
		t.Fatal(err)
	}
	assertEvents(t, evs, []StreamEvent{
		{Type: EventReasoning, Text: "think"},
		{Type: EventToken, Text: "Hello "},
		{Type: EventToken, Text: "world"},
	})
}

func TestChatStreamToolCallAccumulation(t *testing.T) {
	withStreamDefaults(t)
	withSSEServer(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w,
			toolCallChunk(t, 0, "a1", "exec", "")+
				toolCallChunk(t, 0, "", "", `{"program":"ls"}`)+
				toolCallChunk(t, 1, "a2", "image_read", `{"path":"x.png"}`)+
				sseData("[DONE]"))
	})
	// out-of-order indices; stream ends without finish_reason -> flush at EOF.
	evs, err := collectStream(t, []Message{{Role: "user", Content: "hi"}})
	if err != nil {
		t.Fatal(err)
	}
	var calls []ToolCall
	var sawDelta bool
	for _, e := range evs {
		switch e.Type {
		case EventToolCallDelta:
			sawDelta = true
		case EventToolCalls:
			calls = e.ToolCalls
		}
	}
	if !sawDelta || calls == nil {
		t.Fatalf("missing tool events: %+v", evs)
	}
	if len(calls) != 2 || calls[0].ID != "a1" || calls[0].Function.Name != "exec" ||
		calls[0].Function.Arguments != `{"program":"ls"}` ||
		calls[1].ID != "a2" || calls[1].Function.Name != "image_read" {
		t.Fatalf("accumulation wrong: %+v", calls)
	}
}

func TestChatStreamToolCallsFinish(t *testing.T) {
	withStreamDefaults(t)
	withSSEServer(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w,
			toolCallChunk(t, 0, "a1", "exec", `{"program":"ls"}`)+
				deltaChunk(t, map[string]interface{}{}, "tool_calls")+
				sseData("[DONE]"))
	})
	evs, err := collectStream(t, []Message{{Role: "user", Content: "hi"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) == 0 || evs[len(evs)-1].Type != EventToolCalls || len(evs[len(evs)-1].ToolCalls) != 1 {
		t.Fatalf("finish_reason=tool_calls must emit final EventToolCalls: %+v", evs)
	}
}

func TestChatStreamCustomFieldMapping(t *testing.T) {
	withStreamDefaults(t)
	reasoningField, contentField, toolCallsField, finishReasonField = "thinking", "text", "tc", "fr"
	withSSEServer(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w,
			deltaChunk(t, map[string]interface{}{"thinking": "T"}, "")+
				deltaChunk(t, map[string]interface{}{"text": "X"}, "")+
				sseData("[DONE]"))
	})
	evs, err := collectStream(t, []Message{{Role: "user", Content: "hi"}})
	if err != nil {
		t.Fatal(err)
	}
	assertEvents(t, evs, []StreamEvent{
		{Type: EventReasoning, Text: "T"},
		{Type: EventToken, Text: "X"},
	})
}

func TestChatStreamDoneStopsReading(t *testing.T) {
	withStreamDefaults(t)
	withSSEServer(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w,
			deltaChunk(t, map[string]interface{}{contentField: "a"}, "")+
				sseData("[DONE]")+
				deltaChunk(t, map[string]interface{}{contentField: "IGNORED"}, ""))
	})
	evs, err := collectStream(t, []Message{{Role: "user", Content: "hi"}})
	if err != nil {
		t.Fatal(err)
	}
	assertEvents(t, evs, []StreamEvent{{Type: EventToken, Text: "a"}})
}

func TestChatStreamFlushPendingOnEOF(t *testing.T) {
	withStreamDefaults(t)
	withSSEServer(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, toolCallChunk(t, 0, "a1", "exec", `{"program":"ls"}`))
	})
	evs, err := collectStream(t, []Message{{Role: "user", Content: "hi"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) == 0 || evs[len(evs)-1].Type != EventToolCalls {
		t.Fatalf("pending tools must flush at EOF: %+v", evs)
	}
}

func TestChatStreamSkipsGarbageLines(t *testing.T) {
	withStreamDefaults(t)
	withSSEServer(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w,
			"event: ping\n\n"+
				": keepalive\n\n"+
				"data: {bad json\n\n"+
				sseData(`{"choices":[]}`)+
				deltaChunk(t, map[string]interface{}{contentField: "ok"}, "")+
				sseData("[DONE]"))
	})
	evs, err := collectStream(t, []Message{{Role: "user", Content: "hi"}})
	if err != nil {
		t.Fatal(err)
	}
	assertEvents(t, evs, []StreamEvent{{Type: EventToken, Text: "ok"}})
}

// ---------- streaming: error paths ----------

func TestChatStreamAPIError(t *testing.T) {
	withStreamDefaults(t)
	withSSEServer(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	})
	_, err := collectStream(t, []Message{{Role: "user", Content: "hi"}})
	if err == nil || !strings.Contains(err.Error(), "API error 500") || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("expected API error 500 with body, got %v", err)
	}
}

func TestChatStreamServerGone(t *testing.T) {
	withStreamDefaults(t)
	srv := withSSEServer(t, func(w http.ResponseWriter, r *http.Request) {})
	srv.Close()
	if _, err := collectStream(t, []Message{{Role: "user", Content: "hi"}}); err == nil {
		t.Fatal("expected connection error")
	}
}

func TestChatStreamContextCancel(t *testing.T) {
	withStreamDefaults(t)
	withSSEServer(t, func(w http.ResponseWriter, r *http.Request) {
		// hold the response; bail out on cancel or after a grace period so
		// srv.Close in cleanup can never block forever
		select {
		case <-r.Context().Done():
		case <-time.After(2 * time.Second):
		}
	})
	ctx, outer := context.WithCancel(context.Background())
	defer outer()
	go func() { time.Sleep(50 * time.Millisecond); outer() }()
	if _, _, err := chatStream(ctx, []Message{{Role: "user", Content: "hi"}}); err == nil {
		t.Fatal("expected error after context cancel")
	}
}

// ---------- request shape ----------

func TestChatStreamRequestShape(t *testing.T) {
	withStreamDefaults(t)
	var shape struct {
		Model       string      `json:"model"`
		Stream      bool        `json:"stream"`
		Temperature interface{} `json:"temperature"`
		Messages    []Message   `json:"messages"`
		Tools       []Tool      `json:"tools"`
	}
	phase := 0
	withSSEServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/chat/completions" {
			t.Errorf("bad request: %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer test-key" {
			t.Errorf("bad auth header: %q", r.Header.Get("Authorization"))
		}
		body, _ := io.ReadAll(r.Body)
		shape = struct {
			Model       string      `json:"model"`
			Stream      bool        `json:"stream"`
			Temperature interface{} `json:"temperature"`
			Messages    []Message   `json:"messages"`
			Tools       []Tool      `json:"tools"`
		}{}
		if err := json.Unmarshal(body, &shape); err != nil {
			t.Errorf("request not valid JSON: %v", err)
		}
		phase++
		io.WriteString(w, sseData("[DONE]"))
	})
	msgs := []Message{{Role: "system", Content: "s"}, {Role: "user", Content: "hi"}}
	if _, err := collectStream(t, msgs); err != nil {
		t.Fatal(err)
	}
	if shape.Model != "test-model" || !shape.Stream || len(shape.Messages) != 2 || len(shape.Tools) != 3 {
		t.Fatalf("bad request shape: model=%q stream=%v msgs=%d tools=%d",
			shape.Model, shape.Stream, len(shape.Messages), len(shape.Tools))
	}

	extraRequestFields = []map[string]interface{}{{"temperature": 0.5}}
	if _, err := collectStream(t, msgs); err != nil {
		t.Fatal(err)
	}
	if shape.Temperature != 0.5 {
		t.Fatalf("-extra merge failed: temperature=%v", shape.Temperature)
	}
}

// ---------- inference loop: tool round limit (end-to-end over httptest) ----------

// TestExecHelper is the payload executed by the round-limit test's exec tool:
// when spawned with AGENT_GO_HELPER=1 it exits immediately.
func TestExecHelper(t *testing.T) {
	if os.Getenv("AGENT_GO_HELPER") == "1" {
		os.Exit(0)
	}
}

func TestRunInferenceLoopToolRoundLimit(t *testing.T) {
	withStreamDefaults(t)
	maxToolRounds = 3

	toolArgs, _ := json.Marshal(map[string]interface{}{
		"program":         os.Args[0],
		"args":            []string{"-test.run=^TestExecHelper$"},
		"input":           "",
		"timeout_seconds": 30,
		"environ":         map[string]string{"AGENT_GO_HELPER": "1"},
	})
	turn := deltaChunk(t, map[string]interface{}{
		toolCallsField: []interface{}{map[string]interface{}{
			"index": float64(0), "id": "c1", "type": "function",
			"function": map[string]interface{}{"name": "exec", "arguments": string(toolArgs)},
		}},
	}, "tool_calls")

	var mu int64
	withSSEServer(t, func(w http.ResponseWriter, r *http.Request) {
		mu++
		io.WriteString(w, turn)
	})

	msgs := []Message{{Role: "system", Content: "s"}, {Role: "user", Content: "go"}}
	if err := runInferenceLoop(context.Background(), &msgs); err != nil {
		t.Fatalf("round limit must end the turn gracefully, got %v", err)
	}

	var assistants, tools int
	last := msgs[len(msgs)-1]
	if last.Role != "assistant" || last.Content != "[Tool round limit reached: 3]" {
		t.Fatalf("missing limit marker, last=%+v", last)
	}
	for _, m := range msgs {
		switch m.Role {
		case "assistant":
			if len(m.ToolCalls) > 0 {
				assistants++
			}
		case "tool":
			tools++
			var tr ToolResult
			if json.Unmarshal([]byte(fmt.Sprint(m.Content)), &tr) != nil {
				t.Fatalf("tool result not wrapped JSON: %v", m.Content)
			}
		}
	}
	if assistants != 3 || tools != 3 {
		t.Fatalf("assistant(tool_calls)=%d tool=%d, want 3/3 (strictly paired)", assistants, tools)
	}
	if atomic.LoadInt64(&mu) != 3 {
		t.Fatalf("server hits=%d, want 3", mu)
	}
}
