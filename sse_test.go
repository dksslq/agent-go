package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
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
	oldIdle := streamIdleTimeout

	apiKey, modelName = "test-key", "test-model"
	reasoningField, contentField, toolCallsField, finishReasonField =
		"reasoning_content", "content", "tool_calls", "finish_reason"
	extraRequestFields = nil
	httpClient = &http.Client{}
	streamIdleTimeout = time.Minute

	t.Cleanup(func() {
		apiBase, apiKey, modelName = oldAPI, oldKey, oldModel
		reasoningField, contentField, toolCallsField, finishReasonField = oldR, oldC, oldTC, oldFR
		extraRequestFields, httpClient = oldExtra, oldClient
		streamIdleTimeout = oldIdle
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

func TestChatStreamRejectsPendingOnEOF(t *testing.T) {
	withStreamDefaults(t)
	withSSEServer(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, toolCallChunk(t, 0, "a1", "exec", `{"program":"ls"}`))
	})
	evs, err := collectStream(t, []Message{{Role: "user", Content: "hi"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) == 0 || evs[len(evs)-1].Type != EventError {
		t.Fatalf("an incomplete response must fail instead of executing pending tools: %+v", evs)
	}
	for _, event := range evs {
		if event.Type == EventToolCalls {
			t.Fatalf("truncated response emitted executable calls: %+v", evs)
		}
	}
}

func TestChatStreamSkipsGarbageLines(t *testing.T) {
	withStreamDefaults(t)
	withSSEServer(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w,
			"event: ping\n\n"+
				": keepalive\n\n"+
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

// ---------- inference loop: multi-round tool execution (end-to-end over httptest) ----------

func TestRunInferenceLoopMultiRound(t *testing.T) {
	withStreamDefaults(t)
	toolArgs, _ := json.Marshal(map[string]interface{}{
		"program": "echo", "args": []string{"pong"}, "input": "", "timeout_seconds": 5,
	})
	phase := 0
	var hits int64
	withSSEServer(t, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&hits, 1)
		phase++
		if phase == 1 {
			io.WriteString(w, toolCallChunk(t, 0, "c1", "exec", string(toolArgs))+
				deltaChunk(t, map[string]interface{}{}, "tool_calls"))
			return
		}
		io.WriteString(w, deltaChunk(t, map[string]interface{}{contentField: "done"}, "")+sseData("[DONE]"))
	})

	sess := &Session{
		Transcript: []Message{{Role: "system", Content: "s"}, {Role: "user", Content: "go"}},
		Cursor:     1,
	}
	if err := runInferenceLoop(context.Background(), sess); err != nil {
		t.Fatalf("loop: %v", err)
	}
	if atomic.LoadInt64(&hits) != 2 {
		t.Fatalf("server hits=%d, want 2", atomic.LoadInt64(&hits))
	}
	roles := make([]string, 0, len(sess.Transcript))
	for _, m := range sess.Transcript {
		roles = append(roles, m.Role)
	}
	if strings.Join(roles, ",") != "system,user,assistant,tool,assistant" {
		t.Fatalf("transcript roles: %v", roles)
	}
	if len(sess.Transcript[2].ToolCalls) != 1 || sess.Transcript[3].ToolCallID != "c1" {
		t.Fatalf("tool pair broken: %+v %+v", sess.Transcript[2], sess.Transcript[3])
	}
	var tr ToolResult
	if err := json.Unmarshal([]byte(fmt.Sprint(sess.Transcript[3].Content)), &tr); err != nil {
		t.Fatalf("exec result not wrapped: %v", err)
	}
	if !strings.Contains(tr.Result.(map[string]interface{})["stdout"].(string), "pong") {
		t.Fatalf("exec output missing: %q", tr.Result)
	}
	if !strings.Contains(fmt.Sprint(sess.Transcript[4].Content), "done") {
		t.Fatalf("final answer missing: %+v", sess.Transcript[4])
	}
}

// ---------- memory_edit end-to-end (immutable transcript + semantic memory) ----------

func TestMemoryEditEndToEnd(t *testing.T) {
	withStreamDefaults(t)
	var bodies []map[string]interface{}
	phase := 0
	withSSEServer(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var m map[string]interface{}
		if json.Unmarshal(body, &m) == nil {
			bodies = append(bodies, m)
		}
		phase++
		if phase == 1 {
			io.WriteString(w,
				toolCallChunk(t, 0, "m1", "memory_edit", `{"memory":"user greeted; agent said hello; user asked to organize","cursor":3}`)+
					deltaChunk(t, map[string]interface{}{}, "tool_calls"))
			return
		}
		io.WriteString(w, deltaChunk(t, map[string]interface{}{contentField: "done"}, "")+sseData("[DONE]"))
	})

	sess := &Session{
		Transcript: []Message{
			{Role: "system", Content: "s"},
			{Role: "user", Content: "hi"},
			{Role: "assistant", Content: "hello"},
			{Role: "user", Content: "organize"},
		},
		Cursor: 1,
	}
	if err := saveSession(filepath.Join(t.TempDir(), "session.json"), sess); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(sess.close)
	if err := runInferenceLoop(context.Background(), sess); err != nil {
		t.Fatal(err)
	}
	if len(bodies) != 2 {
		t.Fatalf("expected 2 inference rounds, got %d", len(bodies))
	}

	r1 := bodies[0]["messages"].([]interface{})
	if len(r1) != 5 {
		t.Fatalf("round1 saw %d messages, want 5 including persistence status", len(r1))
	}

	// round 2 must see: [system, memory header, user(organize), assistant(tool_calls), tool(result)]
	r2 := bodies[1]["messages"].([]interface{})
	want := []string{"system", "system", "user", "assistant", "tool"}
	if len(r2) != len(want) {
		t.Fatalf("round2 has %d messages, want %d: %+v", len(r2), len(want), r2)
	}
	for i, role := range want {
		got := r2[i].(map[string]interface{})["role"].(string)
		if got != role {
			t.Fatalf("round2[%d].role=%q, want %q", i, got, role)
		}
	}
	mem := r2[1].(map[string]interface{})["content"].(string)
	if !strings.Contains(mem, "Memory (data") || !strings.Contains(mem, "asked to organize") {
		t.Fatalf("memory header wrong: %q", mem)
	}
	if fmt.Sprint(r2[2].(map[string]interface{})["content"]) != "organize" {
		t.Fatalf("view must resume at the cursor: %+v", r2[2])
	}
	var tr ToolResult
	if err := json.Unmarshal([]byte(fmt.Sprint(r2[4].(map[string]interface{})["content"])), &tr); err != nil {
		t.Fatalf("memory_edit result not wrapped: %v", err)
	}
	if !strings.Contains(tr.Result.(string), "Cursor 1 -> 3") {
		t.Fatalf("unexpected report: %q", tr.Result)
	}

	// Compaction releases RAM, while portable history preserves every message.
	t.Cleanup(sess.close)
	if len(sess.Transcript) != 5 || sess.messageCount() != 7 {
		t.Fatalf("compacted prefix retained or duplicated: resident=%d logical=%d", len(sess.Transcript), sess.messageCount())
	}
	full := fullTranscript(t, sess)
	if len(full) != 7 {
		t.Fatalf("transcript = %d messages, want 7 (4 original + tool pair + final answer)", len(sess.Transcript))
	}
	if fmt.Sprint(full[1].Content) != "hi" ||
		fmt.Sprint(full[2].Content) != "hello" ||
		fmt.Sprint(full[3].Content) != "organize" {
		t.Fatalf("transcript must never be rewritten: %+v", sess.Transcript)
	}
	if !strings.Contains(fmt.Sprint(full[6].Content), "done") {
		t.Fatalf("final answer missing from transcript: %+v", full[6])
	}
	if sess.Memory != "user greeted; agent said hello; user asked to organize" || sess.Cursor != 3 {
		t.Fatalf("session state: memory=%q cursor=%d", sess.Memory, sess.Cursor)
	}
}
