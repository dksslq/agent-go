package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"image"
	"image/png"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestLargePayloadsKeepExactBytes(t *testing.T) {
	text := strings.Repeat("数据✓\n", 1<<20)
	got, err := pipePrompt("instruction", strings.NewReader(text))
	if err != nil || got != "instruction\n\n"+text {
		t.Fatalf("pipe changed large input: %v", err)
	}
	// The writer must keep draining even when the preview is bounded.
	for _, limit := range []int{0, 7} {
		out, err := newCommandOutput(limit, "", "")
		if err != nil {
			t.Fatal(err)
		}
		n, err := out.ReadFrom(strings.NewReader(text))
		if err != nil || n != int64(len(text)) {
			t.Fatalf("short drain: %d %v", n, err)
		}
		actual := out.finish()
		if limit == 0 && (actual != text || out.truncated) {
			t.Fatal("unlimited output was truncated")
		}
		if limit > 0 && (actual != string([]rune(text)[:limit]) || !out.truncated) {
			t.Fatal("Unicode preview boundary incorrect")
		}
	}
	s := newSession()
	t.Cleanup(s.close)
	s.Transcript = append(s.Transcript, Message{Role: "user", Content: text})
	path := filepath.Join(t.TempDir(), "large.json")
	if err := saveSession(path, s); err != nil {
		t.Fatal(err)
	}
	loaded, err := loadSession(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(loaded.close)
	if loaded.Transcript[1].Content != text {
		t.Fatal("large session payload changed")
	}
}

func TestLargeImageToolKeepsCompleteAttachment(t *testing.T) {
	// A valid, poorly compressible PNG exceeds typical 1 MiB/4 MiB cutoffs.
	im := image.NewNRGBA(image.Rect(0, 0, 1536, 1024))
	seed := uint32(17)
	for i := range im.Pix {
		seed = 1664525*seed + 1013904223
		im.Pix[i] = byte(seed >> 24)
	}
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, im); err != nil {
		t.Fatal(err)
	}
	if encoded.Len() < 4<<20 {
		t.Fatal("fixture did not cross 4 MiB")
	}
	path := filepath.Join(t.TempDir(), "large.png")
	if err := os.WriteFile(path, encoded.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	args, _ := json.Marshal(map[string]string{"path": path})
	result, msg := runTool(context.Background(), newSession(), "image_read", string(args))
	if msg == nil {
		t.Fatalf("image failed: %s", result)
	}
	blocks, ok := msg.Content.([]ContentBlock)
	if !ok || len(blocks) != 2 {
		t.Fatal("missing image blocks")
	}
	_, b64, ok := strings.Cut(blocks[1].ImageURL.URL, ",")
	if !ok {
		t.Fatal("missing data URI")
	}
	data, err := base64.StdEncoding.DecodeString(b64)
	if err != nil || !bytes.Equal(data, encoded.Bytes()) {
		t.Fatalf("image bytes changed: %v", err)
	}
}

func TestSessionDocumentInvariantFailures(t *testing.T) {
	const system = `"m0":{"role":"system","content":"rules"}`
	const user = `"m1":{"role":"user","content":"task"}`
	const state = `"state":{"base":0,"cursor":1,"memory":""}`
	cases := map[string]string{
		"first index":           `{"m1":{"role":"system","content":"rules"},` + state + `}`,
		"missing state":         `{` + system + `}`,
		"unknown field":         `{` + system + `,"unexpected":1,` + state + `}`,
		"invalid index":         `{` + system + `,"m-1":{"role":"user","content":"x"},` + state + `}`,
		"missing memory":        `{` + system + `,"state":{"base":2,"cursor":3,"memory":""}}`,
		"cursor before base":    `{` + system + `,"state":{"base":2,"cursor":2,"memory":"facts"}}`,
		"base changes":          `{` + system + `,` + state + `,"state":{"base":2,"cursor":3,"memory":"facts"}}`,
		"gap mismatch":          `{` + system + `,` + state + `,"m3":{"role":"user","content":"task"}}`,
		"noncontiguous suffix":  `{` + system + `,` + user + `,"m3":{"role":"user","content":"task"},` + state + `}`,
		"cursor beyond tail":    `{` + system + `,` + user + `,"state":{"base":0,"cursor":3,"memory":"facts"}}`,
		"orphan tool":           `{` + system + `,"m1":{"role":"tool","tool_call_id":"missing","content":"x"},` + state + `}`,
		"incomplete tool group": `{` + system + `,"m1":{"role":"assistant","tool_calls":[{"id":"a","type":"function","function":{"name":"exec","arguments":"{}"}}]},` + state + `}`,
		"invalid role":          `{` + system + `,"m1":{"role":"invalid","content":"x"},` + state + `}`,
		"unsupported version":   `{"version":3,"transcript":[]}`,
		"late version":          `{` + system + `,"version":5,` + state + `}`,
		"extra root":            `{` + system + `,` + state + `}{}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "session.json")
			if err := os.WriteFile(path, []byte(body), 0600); err != nil {
				t.Fatal(err)
			}
			s, err := loadSession(path)
			if s != nil {
				s.close()
			}
			if err == nil {
				t.Fatal("invalid state was accepted")
			}
			after, err := os.ReadFile(path)
			if err != nil || string(after) != body {
				t.Fatal("failed load modified its input")
			}
		})
	}
}

func TestLegacyUnknownFieldsSkipWholeValues(t *testing.T) {
	for _, unknown := range []string{`null`, `17`, `"note"`, `{"nested":[{"x":[1,2]},true]}`, `[1,{"a":[]}]`} {
		path := filepath.Join(t.TempDir(), "legacy.json")
		body := `{"version":3,"extension":` + unknown + `,"transcript":[{"role":"system","content":"rules"},{"role":"user","content":"task"}],"cursor":1,"memory":""}`
		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		s, err := loadSession(path)
		if err != nil {
			t.Fatal(err)
		}
		if s.messageCount() != 2 || s.Transcript[1].Content != "task" {
			t.Fatal("extension consumed conversation")
		}
		s.close()
	}
}

func TestEmptyRoundsBoundedAndRecoverable(t *testing.T) {
	for _, recoverAt := range []int{2, 0} {
		t.Run(string(rune('0'+recoverAt)), func(t *testing.T) {
			withStreamDefaults(t)
			var requests atomic.Int32
			withSSEServer(t, func(w http.ResponseWriter, r *http.Request) {
				io.Copy(io.Discard, r.Body)
				n := requests.Add(1)
				content := ""
				if recoverAt > 0 && int(n) == recoverAt {
					content = "recovered"
				}
				io.WriteString(w, deltaChunk(t, map[string]interface{}{contentField: content}, "stop"))
			})
			s := newSession()
			s.Transcript = append(s.Transcript, Message{Role: "user", Content: "finish"})
			err := runInferenceLoop(context.Background(), s)
			if recoverAt > 0 {
				if err != nil || requests.Load() != int32(recoverAt) {
					t.Fatalf("recovery: %v, calls=%d", err, requests.Load())
				}
			} else {
				if !errors.Is(err, errRoundRetry) || requests.Load() != maxBadRounds+1 {
					t.Fatalf("unbounded or wrong error: %v calls=%d", err, requests.Load())
				}
			}
			for _, m := range s.Transcript {
				if m.Role == "tool" || len(m.ToolCalls) > 0 {
					t.Fatal("empty stream dispatched tools")
				}
			}
		})
	}
}

func TestClosedCheckpointDoesNotDiscardMemory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "active.json")
	s, err := continueSession(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.close)
	s.Transcript = append(s.Transcript, Message{Role: "user", Content: "must survive"})
	before := s.messageCount()
	oldMemory := s.Memory
	oldCursor := s.Cursor
	if err := s.owner.file.Close(); err != nil {
		t.Fatal(err)
	}
	if result := s.editMemory("replacement", 0); !strings.HasPrefix(result, "Error") {
		t.Fatal("closed checkpoint reported success")
	}
	if s.messageCount() != before || s.Cursor != oldCursor || s.Memory != oldMemory || s.Transcript[1].Content != "must survive" {
		t.Fatal("failed checkpoint discarded live state")
	}
}

func TestCanceledToolAndOutputDoNoWork(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	path := filepath.Join(t.TempDir(), "never-created")
	args, _ := json.Marshal(map[string]interface{}{"program": "never-start", "input": "", "timeout_seconds": 1, "output_file": path})
	result, _ := runTool(ctx, newSession(), "exec", string(args))
	if !strings.Contains(result, "canceled") {
		t.Fatal(result)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("canceled tool created output")
	}
	f, err := os.CreateTemp(t.TempDir(), "output")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	root, stop := context.WithCancel(context.Background())
	defer stop()
	out := newOutputWriter(root, f)
	if err := out.write(ctx, "must not write"); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	st, err := f.Stat()
	if err != nil || st.Size() != 0 {
		t.Fatal("canceled output wrote bytes")
	}
}

// Reject incompatible options before starting a process or creating its output file.
func TestExecDetachOutputConflictHasNoEffects(t *testing.T) {
	path := filepath.Join(t.TempDir(), "no-output")
	raw := execCmdOutput(context.Background(), "never-start", nil, "", time.Second, 0, "", nil, true, path)
	r := decodeExecResult(t, raw)
	if r.Error == "" || r.PID != 0 || r.ExitCode != nil {
		t.Fatalf("conflict launched command: %+v", r)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("conflict created a file")
	}
}
