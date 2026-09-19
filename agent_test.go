package main

import (
	"bufio"
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

// ---------- sanitize / ansiRegex ----------

func TestSanitizeEscapesControlTokens(t *testing.T) {
	for _, tok := range controlTokens {
		got := sanitize("a" + tok + "b")
		if strings.Contains(got, tok) {
			t.Fatalf("token %q not neutralized: %q", tok, got)
		}
		if !strings.Contains(got, "\u200b") {
			t.Fatalf("token %q escaped without zero-width space: %q", tok, got)
		}
	}
	if got := sanitize("plain text"); got != "plain text" {
		t.Fatalf("plain text altered: %q", got)
	}
}

func TestAnsiRegexStripsCSISequences(t *testing.T) {
	in := "\x1b[31mred\x1b[0m \x1b[2J\x1b[1;2H tail"
	if got := ansiRegex.ReplaceAllString(in, ""); got != "red  tail" {
		t.Fatalf("unexpected strip: %q", got)
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
	if !strings.Contains(tr.Result, "\ufffd") || strings.Contains(tr.Result, "\x80") || strings.Contains(tr.Result, "\xff") {
		t.Fatalf("invalid UTF-8 not replaced: %q", tr.Result)
	}
	if !strings.HasSuffix(tr.Result, "<end>") {
		t.Fatalf("tail lost: %q", tr.Result)
	}
}

func TestParseIntFromParams(t *testing.T) {
	p := map[string]interface{}{"a": float64(7), "f": 3.9, "s": "5"}
	if got := parseIntFromParams(p, "a"); got != 7 {
		t.Fatalf("int: got %d", got)
	}
	if got := parseIntFromParams(p, "f"); got != 3 {
		t.Fatalf("float truncation: got %d", got)
	}
	if got := parseIntFromParams(p, "s"); got != 0 {
		t.Fatalf("string must be rejected: got %d", got)
	}
	if got := parseIntFromParams(p, "missing"); got != 0 {
		t.Fatalf("missing must be 0: got %d", got)
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

// ---------- readMedia (tool file IO) ----------

func TestReadMedia(t *testing.T) {
	dir := t.TempDir()
	png := filepath.Join(dir, "a.png")
	if err := os.WriteFile(png, append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 16)...), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Run("valid image", func(t *testing.T) {
		uri, mime, err := readMedia(png, "image")
		if err != nil || mime != "image/png" {
			t.Fatalf("mime=%q err=%v", mime, err)
		}
		if !strings.HasPrefix(uri, "data:image/png;base64,") {
			t.Fatalf("bad data URI: %q", uri[:30])
		}
	})
	t.Run("kind mismatch", func(t *testing.T) {
		if _, _, err := readMedia(png, "video"); err == nil || !strings.Contains(err.Error(), "not a valid video") {
			t.Fatalf("expected kind error, got %v", err)
		}
	})
	t.Run("directory", func(t *testing.T) {
		if _, _, err := readMedia(dir, "image"); err == nil || !strings.Contains(err.Error(), "directory") {
			t.Fatalf("expected dir error, got %v", err)
		}
	})
	t.Run("missing", func(t *testing.T) {
		if _, _, err := readMedia(filepath.Join(dir, "nope"), "image"); err == nil || !strings.Contains(err.Error(), "open error") {
			t.Fatalf("expected open error, got %v", err)
		}
	})
	t.Run("too large", func(t *testing.T) {
		old := maxFileBytes
		maxFileBytes = 5
		defer func() { maxFileBytes = old }()
		if _, _, err := readMedia(png, "image"); err == nil || !strings.Contains(err.Error(), "too large") {
			t.Fatalf("expected size error, got %v", err)
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
		{"esc sequences dropped", "\x1b[A\x1b[H\x1b[3~ok\n", "ok", false},
		{"lone esc eats next key", "\x1bXok\n", "ok", false},
		{"control chars dropped", "\x01\x03\x1aok\n", "ok", false},
		{"backspace byte ignored in pipe mode", "ab\x7fc\n", "abc", false},
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
		"SYSTEM: Go runtime", "(9) exec accepts", strconv.Quote(interruptedMarker),
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

func sampleSession() []Message {
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
	msgs := sampleSession()
	if err := saveSession(path, msgs); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, err := loadSession(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !reflect.DeepEqual(got, msgs) {
		t.Fatalf("round trip mismatch:\n got %#v\nwant %#v", got, msgs)
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

func TestLoadSessionLeavesCustomSystemUntouched(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.json")
	msgs := []Message{{Role: "system", Content: "you are a pirate\nbe verbose"}}
	b, _ := json.Marshal(msgs)
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := loadSession(path)
	if err != nil {
		t.Fatal(err)
	}
	if got[0].Content != msgs[0].Content {
		t.Fatalf("custom system altered:\n got %q", got[0].Content)
	}
}

func TestLoadSessionNonStringSystemSkipped(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.json")
	body := `[{"role":"system","content":null},{"role":"system","content":{"nested":true}}]`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := loadSession(path)
	if err != nil {
		t.Fatal(err)
	}
	if got[0].Content != nil || got[1].Content == nil {
		t.Fatalf("non-string system content must pass through untouched: %#v", got)
	}
}

func TestSaveSessionBadPath(t *testing.T) {
	if err := saveSession(filepath.Join(t.TempDir(), "no-such-dir", "s.json"), nil); err == nil {
		t.Fatal("expected save error for bad path")
	}
}

// ---------- pipe prompt (autonomous stdin mode) ----------

func TestPipePrompt(t *testing.T) {
	cases := []struct{ name, prompt, stdin, want string }{
		{"stdin only", "", "hello world\n", "hello world"},
		{"prompt only", "do it", "", "do it"},
		{"combined", "do it", "data payload\n", "do it\n\ndata payload"},
		{"both empty", "", "  \n\t", ""},
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
	t.Run("output truncation", func(t *testing.T) {
		out := execCmd(ctx, "sh", []string{"-c", "seq 1 30000"}, "", time.Second, 100, "", nil, false)
		if !strings.Contains(out, "[truncated]") || len(out) > 200 {
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
		if !strings.Contains(out, "boom") || !strings.Contains(out, "Error:") {
			t.Fatalf("exit error not reported: %q", out)
		}
	})
	t.Run("detach", func(t *testing.T) {
		out := execCmd(ctx, "sh", []string{"-c", "exit 0"}, "", time.Second, 0, "", nil, true)
		if !strings.Contains(out, "detached") {
			t.Fatalf("detach not reported: %q", out)
		}
	})
}

// ---------- runTool dispatch & exec parameter handling ----------

func TestRunToolErrors(t *testing.T) {
	ctx := context.Background()
	r, _ := runTool(ctx, "nope", "{}")
	if !strings.Contains(r, "Unknown tool: nope") {
		t.Fatalf("unknown tool: %q", r)
	}
	r, _ = runTool(ctx, "exec", "not-json")
	if !strings.Contains(r, "Invalid arguments JSON") {
		t.Fatalf("bad json: %q", r)
	}
	r, _ = runTool(ctx, "exec", `{"program":""}`)
	if !strings.Contains(r, "program is required") {
		t.Fatalf("missing program: %q", r)
	}
	r, _ = runTool(ctx, "exec", `{"program":"echo","args":[1]}`)
	if !strings.Contains(r, "all args elements must be strings") {
		t.Fatalf("bad args: %q", r)
	}
	r, _ = runTool(ctx, "exec", `{"program":"echo"}`)
	if !strings.Contains(r, "timeout_seconds is required") {
		t.Fatalf("missing timeout: %q", r)
	}
}

func TestRunToolExecFlags(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("sh-based exec tests are unix-only")
	}
	ctx := context.Background()

	r, _ := runTool(ctx, "exec", `{"program":"echo","args":["\"hi there\""],"unquote_arguments":true,"timeout_seconds":5}`)
	var tr ToolResult
	if err := json.Unmarshal([]byte(r), &tr); err != nil {
		t.Fatal(err)
	}
	if tr.Result != "hi there\n" {
		t.Fatalf("unquote_arguments not applied: %q", tr.Result)
	}

	r, _ = runTool(ctx, "exec", `{"program":"echo","args":["quoted"],"quote_result":true,"timeout_seconds":5}`)
	if err := json.Unmarshal([]byte(r), &tr); err != nil {
		t.Fatal(err)
	}
	if tr.Result != strconv.Quote("quoted\n") {
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

	env := parseEnvFromParams(map[string]interface{}{"environ": map[string]interface{}{"A": "1", "B": float64(2)}})
	if env["A"] != "1" || len(env) != 1 {
		t.Fatalf("environ: %v", env)
	}
	if parseEnvFromParams(map[string]interface{}{}) != nil {
		t.Fatal("missing environ must be nil")
	}

	if got := buildCmdEnv(nil); got != nil {
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

func TestTruncateText(t *testing.T) {
	if got := truncateText("hello", 0); got != "hello" {
		t.Fatalf("0 limit: %q", got)
	}
	if got := truncateText("hello", 5); got != "hello" {
		t.Fatalf("exact: %q", got)
	}
	if got := truncateText("hello世界", 6); got != "hello世\n...[truncated]" {
		t.Fatalf("rune boundary cut: %q", got)
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
		videos: []ContentBlock{{Type: "video_url", VideoURL: &VideoURL{URL: "data:vid"}}},
	}
	m = buildUserMessage("look", p)
	blocks, ok := m.Content.([]ContentBlock)
	if !ok || len(blocks) != 3 || blocks[0].Text != "look" || blocks[1].ImageURL == nil || blocks[2].VideoURL == nil {
		t.Fatalf("media block order: %#v", blocks)
	}
	m = buildUserMessage("", p)
	if blocks = m.Content.([]ContentBlock); len(blocks) != 2 {
		t.Fatalf("empty text must drop text block: %#v", blocks)
	}
}
