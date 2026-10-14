package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// API strings are data. Do not silently insert characters into user payloads.
func TestUserInputPreservesLiteralPayload(t *testing.T) {
	text := "  <|im_start|> [gMASK] \r\n"
	if got := buildUserMessage(text, &pendingState{}); got.Content != text {
		t.Fatalf("payload changed: %#v", got.Content)
	}
	if got, err := pipePrompt("instruction", strings.NewReader(text)); err != nil || got != "instruction\n\n"+text {
		t.Fatalf("pipe payload changed: %q %v", got, err)
	}
}

// ---------- tool helpers ----------

func TestWrapResult(t *testing.T) {
	raw := "ok\x80\xff<end>"
	j := wrapResult(100, 200, raw)
	var tr ToolResult
	if err := json.Unmarshal([]byte(j), &tr); err != nil {
		t.Fatalf("wrapResult not valid JSON: %v", err)
	}
	if tr.StartUnix != 100 || tr.EndUnix != 200 {
		t.Fatalf("timestamps lost: %+v", tr)
	}
	if !strings.Contains(tr.Result.(string), "\ufffd") || strings.Contains(tr.Result.(string), "\x80") || strings.Contains(tr.Result.(string), "\xff") {
		t.Fatalf("invalid UTF-8 not replaced: %q", tr.Result)
	}
	if !strings.HasSuffix(tr.Result.(string), "<end>") {
		t.Fatalf("tail lost: %q", tr.Result)
	}
}

func TestParseBoolFromParams(t *testing.T) {
	p := map[string]interface{}{"b": true, "s": "true", "n": float64(1)}
	if !parseBoolFromParams(p, "b") {
		t.Fatal("true lost")
	}
	for _, k := range []string{"s", "n", "missing"} {
		if parseBoolFromParams(p, k) {
			t.Fatalf("%s must be false", k)
		}
	}
}

// ---------- readImage (tool file IO) ----------

func TestReadImage(t *testing.T) {
	dir := t.TempDir()
	png := filepath.Join(dir, "a.png")
	if err := os.WriteFile(png, append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 16)...), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Run("valid image", func(t *testing.T) {
		uri, mime, err := readImage(png)
		if err != nil || mime != "image/png" {
			t.Fatalf("mime=%q err=%v", mime, err)
		}
		if !strings.HasPrefix(uri, "data:image/png;base64,") {
			t.Fatalf("bad data URI: %q", uri[:30])
		}
	})
	t.Run("not an image", func(t *testing.T) {
		txt := filepath.Join(dir, "a.txt")
		if err := os.WriteFile(txt, []byte("plain text"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, _, err := readImage(txt); err == nil || !strings.Contains(err.Error(), "not a valid image") {
			t.Fatalf("expected type error, got %v", err)
		}
	})
	t.Run("directory", func(t *testing.T) {
		if _, _, err := readImage(dir); err == nil || !strings.Contains(err.Error(), "directory") {
			t.Fatalf("expected dir error, got %v", err)
		}
	})
	t.Run("missing", func(t *testing.T) {
		if _, _, err := readImage(filepath.Join(dir, "nope")); err == nil || !strings.Contains(err.Error(), "open error") {
			t.Fatalf("expected open error, got %v", err)
		}
	})
}

// ---------- readLine (pipe mode; raw mode is covered by the PTY suite) ----------

func readPipe(t *testing.T, s string) (string, error) {
	t.Helper()
	old := termRaw
	termRaw = false
	t.Cleanup(func() { termRaw = old })
	return readLine(bufio.NewReader(strings.NewReader(s)))
}

func TestReadLinePipe(t *testing.T) {
	cases := []struct {
		name, in, want string
		wantEOF        bool
	}{
		{"plain", "hello\n", "hello", false},
		{"crlf", "ok\r\n", "ok", false},
		{"lone cr", "ok\r", "ok", false},
		{"tab kept", "a\tb\n", "a\tb", false},
		{"esc preserved", "\x1b[A\x1b[H\x1b[3~ok\n", "\x1b[A\x1b[H\x1b[3~ok", false},
		{"lone esc preserves next key", "\x1bXok\n", "\x1bXok", false},
		{"controls preserved", "\x01\x03\x1aok\n", "\x01\x03\x1aok", false},
		{"backspace byte ignored in pipe mode", "ab\x7fc\n", "ab\x7fc", false},
		{"eof with buffer", "abc", "abc", true},
		{"empty", "\n", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := readPipe(t, c.in)
			if got != c.want {
				t.Fatalf("got %q, want %q", got, c.want)
			}
			if (err == io.EOF) != c.wantEOF {
				t.Fatalf("EOF=%v, want %v", err, c.wantEOF)
			}
		})
	}
}

// ---------- system prompt ----------

// 系统提示词必须完全静态：任何环境信息（TZ/OS/Exe/变量）跨运行即失真，
// 静态写入只会打穿模型端前缀缓存；模型需要时可用 exec 实时探测。
func TestSystemRules(t *testing.T) {
	s := systemRules()
	for _, want := range []string{
		"autonomous assistant", "output_file", "milestones", "last_request_bytes", strconv.Quote(interruptedMarker),
	} {
		if !strings.Contains(s, want) {
			t.Fatalf("missing %q in system prompt", want)
		}
	}
	for _, ban := range []string{"ENV:", "TZ:", "OS:", "Exe:", "Timestamp:", "PID:", "Vars:", "PATH="} {
		if strings.Contains(s, ban) {
			t.Fatalf("environment detail %q must not be in system prompt (goes stale, breaks prefix cache)", ban)
		}
	}
}

// ---------- session save / load ----------

func sampleTranscript() []Message {
	return []Message{
		{Role: "system", Content: "custom persona"},
		{Role: "user", Content: "hi"},
		{Role: "assistant", Content: "let me look", ToolCalls: []ToolCall{{
			ID: "call1", Type: "function",
			Function: FunctionCall{Name: "exec", Arguments: `{"program":"ls","input":"","timeout_seconds":10}`},
		}}},
		{Role: "tool", ToolCallID: "call1", Name: "exec", Content: "agent.go"},
		{Role: "user", Content: []interface{}{
			map[string]interface{}{"type": "text", "text": "what is this"},
			map[string]interface{}{"type": "image_url", "image_url": map[string]interface{}{"url": "data:image/png;base64,AAA"}},
		}},
	}
}

func TestSessionRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.json")
	want := &Session{Transcript: sampleTranscript(), Memory: "durable facts", Cursor: 2}
	if err := checkpointSession(path, want); err != nil {
		t.Fatalf("save: %v", err)
	}
	want.close()
	got, err := loadSession(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	t.Cleanup(got.close)
	if got.messageCount() != want.messageCount() || got.Cursor != want.Cursor || got.Memory != want.Memory || !reflect.DeepEqual(fullTranscript(t, got), fullTranscript(t, want)) {
		t.Fatalf("round trip mismatch: got count=%d cursor=%d memory=%q", got.messageCount(), got.Cursor, got.Memory)
	}
}

func TestLoadSessionEmptyFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.json")
	if err := os.WriteFile(path, []byte("  \n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := loadSession(path)
	if got != nil || err != nil {
		t.Fatalf("empty file: got %v, err %v", got, err)
	}
}

func TestLoadSessionInvalidJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadSession(path); err == nil || !strings.Contains(err.Error(), "invalid session JSON") {
		t.Fatalf("expected invalid JSON error, got %v", err)
	}
}

func TestLoadSessionMissing(t *testing.T) {
	if _, err := loadSession(filepath.Join(t.TempDir(), "nope.json")); err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestLoadSessionV3(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.json")
	body := `{"version":3,"transcript":[{"role":"system","content":"s"},{"role":"user","content":"u1"},{"role":"assistant","content":"a1"}],"memory":"notes","cursor":2}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := loadSession(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(got.close)
	if len(got.Transcript) != 2 || got.messageCount() != 3 || got.Memory != "notes" || got.Cursor != 2 {
		t.Fatalf("v3 fields lost: %+v", got)
	}
}

func TestLoadSessionV3ClampsCursor(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.json")
	if err := os.WriteFile(path, []byte(`{"version":3,"transcript":[{"role":"system","content":"s"}],"cursor":9}`), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := loadSession(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Cursor != 1 {
		t.Fatalf("cursor must clamp into [1, len], got %d", got.Cursor)
	}
}

func TestLoadSessionLegacyArray(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.json")
	body := `[{"role":"system","content":"you are a pirate"},{"role":"user","content":[{"type":"text","text":"hi"},{"type":"image_url","image_url":{"url":"data:image/png;base64,AAA"}}]}]`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := loadSession(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Memory != "" || got.Cursor != 1 || len(got.Transcript) != 2 {
		t.Fatalf("legacy array must map to transcript, nothing absorbed: %+v", got)
	}
	if got.Transcript[0].Content != "you are a pirate" || got.Transcript[1].Content == nil {
		t.Fatalf("legacy content must pass through untouched: %#v", got.Transcript)
	}
	view := got.contextMessages()
	if len(view) != 3 || view[1].Role != "system" || view[2].Role != "user" {
		t.Fatalf("legacy view must show the full transcript and persistence state: %+v", view)
	}
}

func TestSaveSessionAppendsCheckpoints(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "s.json")
	s := &Session{Transcript: sampleTranscript(), Memory: "one"}
	t.Cleanup(s.close)
	if err := checkpointSession(path, s); err != nil {
		t.Fatal(err)
	}
	// Memory replacements go through memory_edit so the document records them.
	if result := s.editMemory("two", 0); strings.HasPrefix(result, "Error") {
		t.Fatal(result)
	}
	s.Transcript = append(s.Transcript, Message{Role: "assistant", Content: "new evidence"})
	if err := checkpointSession(path, s); err != nil {
		t.Fatal(err)
	}
	got, err := loadReservedSession(path, s.owner)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(got.close)
	if got.Memory != "two" || got.messageCount() != s.messageCount() {
		t.Fatalf("checkpoint lost new memory or messages: %+v", got)
	}
	leftovers, _ := filepath.Glob(filepath.Join(dir, "*.tmp"))
	if len(leftovers) != 0 {
		t.Fatalf("tmp file left behind: %v", leftovers)
	}
}

func TestSaveSessionBadPath(t *testing.T) {
	if err := checkpointSession(filepath.Join(t.TempDir(), "no-such-dir", "s.json"), &Session{}); err == nil {
		t.Fatal("expected save error for bad path")
	}
}

// ---------- pipe prompt (autonomous stdin mode) ----------

func TestPipePrompt(t *testing.T) {
	cases := []struct{ name, prompt, stdin, want string }{
		{"stdin only", "", "hello world\n", "hello world\n"},
		{"prompt only", "do it", "", "do it"},
		{"combined", "do it", "data payload\n", "do it\n\ndata payload\n"},
		{"whitespace data preserved", "", "  \n\t", "  \n\t"},
		{"both empty", "", "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := pipePrompt(c.prompt, strings.NewReader(c.stdin))
			if err != nil {
				t.Fatal(err)
			}
			if got != c.want {
				t.Fatalf("got %q, want %q", got, c.want)
			}
		})
	}
}

// ---------- exec: real process end-to-end (unix; windows has no /bin/sh) ----------

func TestExecCmdReal(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("sh-based exec tests are unix-only")
	}
	ctx := context.Background()

	t.Run("stdout and stderr captured", func(t *testing.T) {
		out := execCmd(ctx, "sh", []string{"-c", "echo out; echo err 1>&2"}, "", time.Second, 0, "", nil, false)
		if !strings.Contains(out, "out") || !strings.Contains(out, "err") {
			t.Fatalf("combined output lost: %q", out)
		}
	})
	t.Run("stdin injection", func(t *testing.T) {
		out := execCmd(ctx, "sh", []string{"-c", "read line; echo got:$line"}, "abc\n", time.Second, 0, "", nil, false)
		if !strings.Contains(out, "got:abc") {
			t.Fatalf("stdin not delivered: %q", out)
		}
	})
	t.Run("timeout kills", func(t *testing.T) {
		out := execCmd(ctx, "sh", []string{"-c", "sleep 5"}, "", 300*time.Millisecond, 0, "", nil, false)
		if !strings.Contains(out, "timed out") {
			t.Fatalf("timeout not reported: %q", out)
		}
	})
	t.Run("grandchild holds the pipe", func(t *testing.T) {
		start := time.Now()
		out := execCmd(ctx, "sh", []string{"-c", "echo hi; sh -c 'sleep 5' &"}, "", 10*time.Second, 0, "", nil, false)
		if elapsed := time.Since(start); elapsed > 4*time.Second {
			t.Fatalf("wedged on grandchild-held pipe: %v", elapsed)
		}
		if !strings.Contains(out, "hi") {
			t.Fatalf("output lost: %q", out)
		}
	})
	t.Run("output truncation", func(t *testing.T) {
		out := execCmd(ctx, "sh", []string{"-c", "seq 1 30000"}, "", time.Second, 100, "", nil, false)
		if !strings.Contains(out, `"stdout_truncated":true`) || len(out) > 400 {
			t.Fatalf("truncation not applied: %d bytes", len(out))
		}
	})
	t.Run("cwd", func(t *testing.T) {
		dir := t.TempDir()
		out := execCmd(ctx, "sh", []string{"-c", "pwd"}, "", time.Second, 0, dir, nil, false)
		if !strings.Contains(out, filepath.Base(dir)) {
			t.Fatalf("cwd not applied: %q", out)
		}
	})
	t.Run("environ injection", func(t *testing.T) {
		out := execCmd(ctx, "sh", []string{"-c", "echo $AGENTLET_TEST_VAR"}, "", time.Second, 0, "", map[string]string{"AGENTLET_TEST_VAR": "xyz"}, false)
		if !strings.Contains(out, "xyz") {
			t.Fatalf("environ not applied: %q", out)
		}
	})
	t.Run("nonzero exit reported", func(t *testing.T) {
		out := execCmd(ctx, "sh", []string{"-c", "echo boom; exit 3"}, "", time.Second, 0, "", nil, false)
		if !strings.Contains(out, "boom") || !strings.Contains(out, `"exit_code":3`) {
			t.Fatalf("exit error not reported: %q", out)
		}
	})
	t.Run("detach", func(t *testing.T) {
		out := execCmd(ctx, "sh", []string{"-c", "exit 0"}, "", time.Second, 0, "", nil, true)
		if !strings.Contains(out, `"pid":`) {
			t.Fatalf("detach not reported: %q", out)
		}
	})
}

// ---------- runTool dispatch & exec parameter handling ----------

func TestRunToolErrors(t *testing.T) {
	ctx := context.Background()
	r, _ := runTool(ctx, nil, "nope", "{}")
	if !strings.Contains(r, "Unknown tool: nope") {
		t.Fatalf("unknown tool: %q", r)
	}
	r, _ = runTool(ctx, nil, "exec", "not-json")
	if !strings.Contains(r, "Invalid arguments JSON") {
		t.Fatalf("bad json: %q", r)
	}
	r, _ = runTool(ctx, nil, "exec", `{"program":""}`)
	if !strings.Contains(r, "program is required") {
		t.Fatalf("missing program: %q", r)
	}
	r, _ = runTool(ctx, nil, "exec", `{"program":"echo","args":[1]}`)
	if !strings.Contains(r, "invalid exec parameter types") {
		t.Fatalf("bad args: %q", r)
	}
	r, _ = runTool(ctx, nil, "exec", `{"program":"echo"}`)
	if !strings.Contains(r, "timeout_seconds is required") {
		t.Fatalf("missing timeout: %q", r)
	}
}

func TestRunToolExecFlags(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("sh-based exec tests are unix-only")
	}
	ctx := context.Background()

	r, _ := runTool(ctx, nil, "exec", `{"program":"echo","args":["\"hi there\""],"unquote_arguments":true,"timeout_seconds":5}`)
	var tr ToolResult
	if err := json.Unmarshal([]byte(r), &tr); err != nil {
		t.Fatal(err)
	}
	if tr.Result.(map[string]interface{})["stdout"] != "hi there\n" {
		t.Fatalf("unquote_arguments not applied: %q", tr.Result)
	}

	r, _ = runTool(ctx, nil, "exec", `{"program":"echo","args":["quoted"],"quote_result":true,"timeout_seconds":5}`)
	if err := json.Unmarshal([]byte(r), &tr); err != nil {
		t.Fatal(err)
	}
	quoted, err := strconv.Unquote(tr.Result.(string))
	if err != nil {
		t.Fatal(err)
	}
	var payload execResult
	if err := json.Unmarshal([]byte(quoted), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Stdout != "quoted\n" {
		t.Fatalf("quote_result not applied: %q", tr.Result)
	}
}

// ---------- misc param helpers ----------

func TestParseExecParams(t *testing.T) {
	d, err := parseTimeout(map[string]interface{}{"timeout_seconds": float64(3)})
	if err != nil || d != 3*time.Second {
		t.Fatalf("timeout: %v %v", d, err)
	}
	if _, err := parseTimeout(map[string]interface{}{}); err == nil {
		t.Fatal("missing timeout must error")
	}
	if _, err := parseTimeout(map[string]interface{}{"timeout_seconds": float64(-1)}); err == nil {
		t.Fatal("negative timeout must error")
	}
	if _, err := parseTimeout(map[string]interface{}{"timeout_seconds": "3"}); err == nil {
		t.Fatal("string timeout must error")
	}

	if got := buildCmdEnv(nil, nil); got != nil {
		t.Fatal("empty environ must be nil")
	}
}

func TestUnquoteAllStrings(t *testing.T) {
	in := map[string]interface{}{"a": `"x"`, "list": []interface{}{"\"y\"", float64(3)}, "keep": "plain"}
	out, ok := unquoteAllStrings(in).(map[string]interface{})
	if !ok {
		t.Fatalf("map type lost: %#v", out)
	}
	list := out["list"].([]interface{})
	if out["a"] != "x" || list[0] != "y" || list[1] != float64(3) || out["keep"] != "plain" {
		t.Fatalf("unquote: %#v", out)
	}
}

// ---------- user message assembly ----------

func TestBuildUserMessage(t *testing.T) {
	m := buildUserMessage("hi", &pendingState{})
	if m.Content != "hi" {
		t.Fatalf("plain text: %#v", m)
	}
	p := &pendingState{
		images: []ContentBlock{{Type: "image_url", ImageURL: &ImageURL{URL: "data:img"}}},
	}
	m = buildUserMessage("look", p)
	blocks, ok := m.Content.([]ContentBlock)
	if !ok || len(blocks) != 2 || blocks[0].Text != "look" || blocks[1].ImageURL == nil {
		t.Fatalf("media block order: %#v", blocks)
	}
	m = buildUserMessage("", p)
	if blocks = m.Content.([]ContentBlock); len(blocks) != 1 {
		t.Fatalf("empty text must drop text block: %#v", blocks)
	}
}

// ---------- memory_edit (immutable transcript + semantic memory) ----------

func assertPairing(t *testing.T, msgs []Message) {
	t.Helper()
	for i := 0; i < len(msgs); i++ {
		m := msgs[i]
		if len(m.ToolCalls) > 0 {
			if i+1+len(m.ToolCalls) > len(msgs) {
				t.Fatalf("assistant(tool_calls) at %d lacks trailing tool results", i)
			}
			for j := 0; j < len(m.ToolCalls); j++ {
				tr := msgs[i+1+j]
				if tr.Role != "tool" || tr.ToolCallID != m.ToolCalls[j].ID {
					t.Fatalf("pairing broken after assistant@%d: %+v", i, tr)
				}
			}
		}
	}
}

func editTestSession() *Session {
	return &Session{
		Transcript: []Message{
			{Role: "system", Content: "s"},
			{Role: "user", Content: "u1"},
			{Role: "assistant", Content: "a1", ToolCalls: []ToolCall{{ID: "a", Type: "function", Function: FunctionCall{Name: "exec", Arguments: `{}`}}}},
			{Role: "tool", ToolCallID: "a", Content: "r"},
			{Role: "user", Content: "u2"},
			{Role: "assistant", Content: "a2"},
		},
		Cursor: 1,
	}
}

// Compare persisted full history, not the intentionally compact resident tail.
func fullTranscript(t *testing.T, s *Session) []Message {
	t.Helper()
	return regressionSavedTranscript(t, s)
}

func TestEditMemory(t *testing.T) {
	fresh := func(t *testing.T) *Session { s := editTestSession(); t.Cleanup(s.close); return s }
	t.Run("memory required, state untouched", func(t *testing.T) {
		s := fresh(t)
		want := append([]Message(nil), s.Transcript...)
		if r := s.editMemory("  ", 0); !strings.HasPrefix(r, "Error") {
			t.Fatal(r)
		}
		if s.Memory != "" || s.Cursor != 1 || !reflect.DeepEqual(s.Transcript, want) {
			t.Fatal("rejected edit changed state")
		}
	})
	t.Run("default compacts completed history", func(t *testing.T) {
		s := fresh(t)
		want := fullTranscript(t, s)
		r := s.editMemory("live goal, constraints and completed facts", 0)
		if s.Cursor != 6 || len(s.Transcript) != 1 || s.Memory == "" || !strings.Contains(r, "Cursor 1 -> 6") {
			t.Fatalf("state: %+v result: %s", s, r)
		}
		if !reflect.DeepEqual(fullTranscript(t, s), want) {
			t.Fatal("archived history changed")
		}
		if len(s.contextMessages()) != 2 {
			t.Fatal("absorbed messages still in context")
		}
	})
	t.Run("advance preserves disk history and frees resident prefix", func(t *testing.T) {
		s := fresh(t)
		want := fullTranscript(t, s)
		s.editMemory("u1 and its answer; continue u2", 4)
		if !reflect.DeepEqual(fullTranscript(t, s), want) {
			t.Fatal("history changed")
		}
		assertPairing(t, s.Transcript)
		view := s.contextMessages()
		if s.Cursor != 4 || len(s.Transcript) != 3 || len(view) != 4 || view[2].Content != "u2" || view[3].Content != "a2" {
			t.Fatalf("view: %+v", view)
		}
	})
	t.Run("cursor does not split tool results", func(t *testing.T) {
		s := fresh(t)
		s.editMemory("facts", 3)
		if s.Cursor != 4 || s.Transcript[s.residentCursor()].Role != "user" {
			t.Fatal("unsafe tool boundary")
		}
	})
	t.Run("stale cursor cannot overwrite newer snapshot", func(t *testing.T) {
		s := fresh(t)
		s.editMemory("newer facts", 4)
		r := s.editMemory("stale facts", 2)
		if !strings.HasPrefix(r, "Error") || s.Cursor != 4 || s.Memory != "newer facts" {
			t.Fatalf("stale snapshot applied: %s", r)
		}
	})
	t.Run("pending tool group stays visible", func(t *testing.T) {
		s := fresh(t)
		s.Transcript = append(s.Transcript, Message{Role: "user", Content: "u3"}, Message{Role: "assistant", ToolCalls: []ToolCall{{ID: "pending", Type: "function", Function: FunctionCall{Name: "memory_edit", Arguments: `{"memory":"continue u3"}`}}}})
		s.editMemory("continue u3", 99)
		if s.Cursor != 7 || len(s.contextMessages()) != 4 || s.contextMessages()[2].Role != "user" || s.contextMessages()[3].Role != "assistant" {
			t.Fatal("pending group removed")
		}
	})
}

func TestContextMessages(t *testing.T) {
	s := newSession()
	v := s.contextMessages()
	if len(v) != 2 || v[0].Role != "system" || v[1].Role != "system" || !strings.Contains(v[1].Content.(string), unsavedSessionNotice) {
		t.Fatalf("fresh view lacks the system anchor or unsaved-session warning: %+v", v)
	}
	s.Transcript = append(s.Transcript, Message{Role: "user", Content: "hi"})
	s.Transcript = append(s.Transcript, Message{Role: "assistant", Content: "hello"})
	v = s.contextMessages()
	if len(v) != 4 || !strings.Contains(v[1].Content.(string), unsavedSessionNotice) || v[2].Content != "hi" || v[3].Content != "hello" {
		t.Fatalf("appending work changed persistence state or history: %+v", v)
	}
	s.editMemory("durable facts", 1)
	v = s.contextMessages()
	mem, _ := v[1].Content.(string)
	if len(v) != 4 || v[1].Role != "system" || !strings.Contains(mem, unsavedSessionNotice) || !strings.Contains(mem, memoryHeader+"durable facts") {
		t.Fatalf("memory header missing or misplaced: %+v", v)
	}
}

// The cache contract: between two memory_edit calls the view only grows by
// appends, so the serialized request of turn N is a byte prefix of turn N+1.
func TestContextPrefixStable(t *testing.T) {
	s := newSession()
	s.Transcript = append(s.Transcript,
		Message{Role: "user", Content: "u1"},
		Message{Role: "assistant", Content: "a1"},
	)
	b1, err := json.Marshal(s.contextMessages())
	if err != nil {
		t.Fatal(err)
	}
	s.Transcript = append(s.Transcript, Message{Role: "user", Content: "u2"})
	b2, _ := json.Marshal(s.contextMessages())
	// b1 = [..messages..] and b2 = [..messages..,new] share every byte up to
	// the array terminator — the appended view re-sends turn-N verbatim, so a
	// provider prefix cache loses at most the final bracket tokens.
	if !bytes.HasPrefix(b2, b1[:len(b1)-1]) || b2[len(b1)-1] != ',' {
		t.Fatalf("append must extend the request as a byte prefix (provider prefix cache):\n b1=%s\n b2=%s", b1, b2)
	}
	s.editMemory("facts", 3)
	b3, _ := json.Marshal(s.contextMessages())
	if bytes.HasPrefix(b3, b2) {
		t.Fatal("an explicit memory edit must change the header slot (documented one-time miss)")
	}
	s.Transcript = append(s.Transcript, Message{Role: "assistant", Content: "a2"})
	b4, _ := json.Marshal(s.contextMessages())
	if !bytes.HasPrefix(b4, b3[:len(b3)-1]) || b4[len(b3)-1] != ',' {
		t.Fatal("view must be append-only again after the edit")
	}
}

// Test convenience wrapper; production uses execCmdOutput directly.
func execCmd(ctx context.Context, program string, args []string, input string, timeout time.Duration, maxChars int, cwd string, environ map[string]string, detach bool) string {
	return execCmdOutput(ctx, program, args, input, timeout, maxChars, cwd, environ, detach, "")
}
