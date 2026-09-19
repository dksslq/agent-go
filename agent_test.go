package main

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
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
