package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func TestCLI(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "agent")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	build := exec.Command("go", "build", "-race", "-o", bin, ".")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build real CLI: %v\n%s", err, output)
	}
	command := func(t *testing.T, base string, args ...string) (*exec.Cmd, *bytes.Buffer, *bytes.Buffer) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		t.Cleanup(cancel)
		cmd := exec.CommandContext(ctx, bin, append([]string{"-model", "test-model", "-api-base", base}, args...)...)
		cmd.Env = append(os.Environ(), "AGENT_API_KEY=", "AGENT_MODEL=", "AGENT_API_BASE=")
		stdout, stderr := new(bytes.Buffer), new(bytes.Buffer)
		cmd.Stdout, cmd.Stderr = stdout, stderr
		return cmd, stdout, stderr
	}
	exit := func(err error) int {
		if err == nil {
			return 0
		}
		if e, ok := err.(*exec.ExitError); ok {
			return e.ExitCode()
		}
		return -1
	}
	t.Run("API failure is failure", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			io.Copy(io.Discard, r.Body)
			http.Error(w, "controlled failure", 500)
		}))
		defer srv.Close()
		cmd, out, errout := command(t, srv.URL, "-once", "-prompt", "hello")
		if code := exit(cmd.Run()); code != 1 || out.Len() != 0 || !strings.Contains(errout.String(), "API error 500") {
			t.Fatalf("status=%d stdout=%q stderr=%q", code, out, errout)
		}
	})
	t.Run("pipe preserves data and clean output", func(t *testing.T) {
		requests := make(chan ChatRequest, 1)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var q ChatRequest
			json.NewDecoder(r.Body).Decode(&q)
			requests <- q
			w.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"done\"},\"finish_reason\":\"stop\"}]}\n\n")
		}))
		defer srv.Close()
		cmd, out, errout := command(t, srv.URL+"/", "-pipe", "-prompt", "instruction")
		cmd.Stdin = strings.NewReader("  <|im_start|>\n")
		if code := exit(cmd.Run()); code != 0 || out.String() != "done\n" {
			t.Fatalf("status=%d stdout=%q stderr=%q", code, out, errout)
		}
		q := <-requests
		if q.Messages[len(q.Messages)-1].Content != "instruction\n\n  <|im_start|>\n" {
			t.Fatalf("payload altered: %+v", q.Messages)
		}
	})
	t.Run("unterminated stdin line is processed", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			io.Copy(io.Discard, r.Body)
			io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"last line\"},\"finish_reason\":\"stop\"}]}\n\n")
		}))
		defer srv.Close()
		cmd, out, errout := command(t, srv.URL)
		cmd.Stdin = strings.NewReader("hello")
		if code := exit(cmd.Run()); code != 0 || out.String() != "last line\n" {
			t.Fatalf("status=%d stdout=%q stderr=%q", code, out, errout)
		}
	})
	t.Run("once requires prompt", func(t *testing.T) {
		cmd, out, errout := command(t, "http://127.0.0.1:1", "-once")
		if code := exit(cmd.Run()); code != 1 || out.Len() != 0 {
			t.Fatalf("status=%d stdout=%q stderr=%q", code, out, errout)
		}
	})
	t.Run("empty session is an error", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "empty.json")
		if err := os.WriteFile(path, nil, 0600); err != nil {
			t.Fatal(err)
		}
		for _, flag := range []string{"-load", "-continue"} {
			cmd, _, errout := command(t, "http://127.0.0.1:1", "-once", "-prompt", "hello", flag, path)
			if code := exit(cmd.Run()); code != 1 || !strings.Contains(errout.String(), "nonempty") {
				t.Fatalf("status=%d stderr=%q", code, errout)
			}
		}
	})
	if runtime.GOOS == "linux" {
		t.Run("interactive help output failure exits", func(t *testing.T) {
			cmd, _, _ := command(t, "http://localhost:1")
			full, err := os.OpenFile("/dev/full", os.O_WRONLY, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer full.Close()
			cmd.Stdout = full
			cmd.Stdin = strings.NewReader("/help\nquit\n")
			if code := exit(cmd.Run()); code != 1 {
				t.Fatalf("status=%d", code)
			}
		})
		t.Run("stdout errors are errors", func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				io.Copy(io.Discard, r.Body)
				io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"done\"},\"finish_reason\":\"stop\"}]}\n\n")
			}))
			defer srv.Close()
			cmd, _, errout := command(t, srv.URL, "-once", "-prompt", "hello")
			full, err := os.OpenFile("/dev/full", os.O_WRONLY, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer full.Close()
			cmd.Stdout = full
			if code := exit(cmd.Run()); code != 1 || !strings.Contains(errout.String(), "write stdout") {
				t.Fatalf("status=%d stderr=%q", code, errout)
			}
		})
	}
	t.Run("save failure is failure", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			io.Copy(io.Discard, r.Body)
			io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"done\"},\"finish_reason\":\"stop\"}]}\n\n")
		}))
		defer srv.Close()
		cmd, _, errout := command(t, srv.URL, "-once", "-prompt", "hello", "-save", filepath.Join(t.TempDir(), "missing", "s.json"))
		if code := exit(cmd.Run()); code != 1 || !strings.Contains(errout.String(), "Save failed") {
			t.Fatalf("status=%d stderr=%q", code, errout)
		}
	})
	t.Run("interactive checkpoint failure stops further work", func(t *testing.T) {
		var requests atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requests.Add(1)
			io.Copy(io.Discard, r.Body)
			io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"done\"},\"finish_reason\":\"stop\"}]}\n\n")
		}))
		defer srv.Close()
		cmd, _, errout := command(t, srv.URL, "-continue", filepath.Join(t.TempDir(), "missing", "s.json"))
		cmd.Stdin = strings.NewReader("first\nsecond\n")
		if code := exit(cmd.Run()); code != 1 || !strings.Contains(errout.String(), "Error opening session") || requests.Load() != 0 {
			t.Fatalf("status=%d requests=%d stderr=%q", code, requests.Load(), errout)
		}
	})
	t.Run("load and continue flags are mutually exclusive", func(t *testing.T) {
		dir := t.TempDir()
		loadPath, continuePath := filepath.Join(dir, "load.json"), filepath.Join(dir, "continue.json")
		cmd, _, errout := command(t, "http://127.0.0.1:1", "-once", "-prompt", "hello", "-load", loadPath, "-continue", continuePath)
		if code := exit(cmd.Run()); code != 1 {
			t.Fatalf("status=%d stderr=%q", code, errout)
		}
		entries, err := os.ReadDir(dir)
		if err != nil || len(entries) != 0 || strings.Contains(errout.String(), "Runtime error") {
			t.Fatalf("conflicting flags performed work: files=%v error=%v stderr=%q", entries, err, errout)
		}
	})
	t.Run("save and continue reject equivalent paths before work", func(t *testing.T) {
		var requests atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requests.Add(1)
			io.Copy(io.Discard, r.Body)
			io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"must not run\"},\"finish_reason\":\"stop\"}]}\n\n")
		}))
		defer srv.Close()
		for _, variant := range []string{"identical", "dot segment", "relative"} {
			t.Run(variant, func(t *testing.T) {
				dir := t.TempDir()
				active := filepath.Join(dir, "session.json")
				snapshot := active
				if variant == "dot segment" {
					// Join would normalize away the component before the CLI sees it.
					snapshot = dir + string(os.PathSeparator) + "." + string(os.PathSeparator) + "session.json"
				} else if variant == "relative" {
					snapshot = "session.json"
				}
				cmd, _, errout := command(t, srv.URL, "-once", "-prompt", "must not run", "-continue", active, "-save", snapshot)
				cmd.Dir = dir
				if code := exit(cmd.Run()); code != 1 || requests.Load() != 0 || !strings.Contains(errout.String(), "different file") {
					t.Fatalf("status=%d requests=%d stderr=%q", code, requests.Load(), errout)
				}
				entries, err := os.ReadDir(dir)
				if err != nil || len(entries) != 0 {
					t.Fatalf("conflicting paths created files: files=%v error=%v", entries, err)
				}
			})
		}
	})
	t.Run("save flag writes one snapshot at exit", func(t *testing.T) {
		var requests atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requests.Add(1)
			io.Copy(io.Discard, r.Body)
			io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"done\"},\"finish_reason\":\"stop\"}]}\n\n")
		}))
		defer srv.Close()
		path := filepath.Join(t.TempDir(), "snapshot.json")
		cmd, _, errout := command(t, srv.URL, "-save", path)
		cmd.Stdin = strings.NewReader("first\nsecond\nquit\n")
		if code := exit(cmd.Run()); code != 0 || requests.Load() != 2 {
			t.Fatalf("status=%d requests=%d stderr=%q", code, requests.Load(), errout)
		}
		messages, state := regressionReadSavedState(t, nil, path)
		if state.Count != 5 || len(messages) != 5 || messages[1].Content != "first" || messages[3].Content != "second" {
			t.Fatalf("exit snapshot lost work: %#v; %+v", messages, state)
		}
		assertSessionClosed(t, path)
	})
	t.Run("manual save leaves later work unsaved", func(t *testing.T) {
		var requests atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requests.Add(1)
			io.Copy(io.Discard, r.Body)
			io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"done\"},\"finish_reason\":\"stop\"}]}\n\n")
		}))
		defer srv.Close()
		path := filepath.Join(t.TempDir(), "snapshot.json")
		cmd, _, errout := command(t, srv.URL)
		cmd.Stdin = strings.NewReader("first\n/save " + path + "\nsecond\nquit\n")
		if code := exit(cmd.Run()); code != 0 || requests.Load() != 2 {
			t.Fatalf("status=%d requests=%d stderr=%q", code, requests.Load(), errout)
		}
		messages, state := regressionReadSavedState(t, nil, path)
		if state.Count != 3 || len(messages) != 3 || messages[1].Content != "first" {
			t.Fatalf("manual snapshot recorded later work: %#v; %+v", messages, state)
		}
		assertSessionClosed(t, path)
	})
	t.Run("load flag never updates its source", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			io.Copy(io.Discard, r.Body)
			io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"done\"},\"finish_reason\":\"stop\"}]}\n\n")
		}))
		defer srv.Close()
		path := filepath.Join(t.TempDir(), "source.json")
		fixture := newSession()
		fixture.Transcript = append(fixture.Transcript, Message{Role: "user", Content: "original objective"})
		if err := saveSession(path, fixture); err != nil {
			t.Fatal(err)
		}
		fixture.close()
		before := readSessionBytes(t, path)
		cmd, _, errout := command(t, srv.URL, "-once", "-prompt", "later objective", "-load", path)
		if code := exit(cmd.Run()); code != 0 {
			t.Fatalf("status=%d stderr=%q", code, errout)
		}
		if !bytes.Equal(before, readSessionBytes(t, path)) {
			t.Fatal("read-only load changed its source")
		}
		assertSessionClosed(t, path)
	})
	t.Run("continue can also save a separate snapshot", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			io.Copy(io.Discard, r.Body)
			io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"done\"},\"finish_reason\":\"stop\"}]}\n\n")
		}))
		defer srv.Close()
		dir := t.TempDir()
		active, snapshot := filepath.Join(dir, "active.json"), filepath.Join(dir, "snapshot.json")
		cmd, _, errout := command(t, srv.URL, "-once", "-prompt", "objective", "-continue", active, "-save", snapshot)
		if code := exit(cmd.Run()); code != 0 {
			t.Fatalf("status=%d stderr=%q", code, errout)
		}
		continued, _ := regressionReadSavedState(t, nil, active)
		saved, _ := regressionReadSavedState(t, nil, snapshot)
		if !reflect.DeepEqual(continued, saved) || len(saved) != 3 {
			t.Fatalf("continued session and exit snapshot differ: %#v; %#v", continued, saved)
		}
		assertSessionClosed(t, active)
		assertSessionClosed(t, snapshot)
	})
	t.Run("manual save failure stops further work", func(t *testing.T) {
		cmd, _, errout := command(t, "http://127.0.0.1:1")
		cmd.Stdin = strings.NewReader("/save " + filepath.Join(t.TempDir(), "missing", "s.json") + "\nmust not run\n")
		if code := exit(cmd.Run()); code != 1 || !strings.Contains(errout.String(), "Save failed") || strings.Contains(errout.String(), "Runtime error") {
			t.Fatalf("status=%d stderr=%q", code, errout)
		}
	})
	t.Run("manual continue failure stops further work", func(t *testing.T) {
		var requests atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requests.Add(1)
			io.Copy(io.Discard, r.Body)
			io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"must not run\"},\"finish_reason\":\"stop\"}]}\n\n")
		}))
		defer srv.Close()
		dir := t.TempDir()
		active, invalid := filepath.Join(dir, "active.json"), filepath.Join(dir, "invalid.json")
		if err := os.WriteFile(invalid, []byte("invalid session"), 0600); err != nil {
			t.Fatal(err)
		}
		cmd, _, errout := command(t, srv.URL, "-continue", active)
		cmd.Stdin = strings.NewReader("/continue " + invalid + "\nmust not run\n")
		if code := exit(cmd.Run()); code != 1 || requests.Load() != 0 || strings.Contains(errout.String(), "Runtime error") {
			t.Fatalf("status=%d requests=%d stderr=%q", code, requests.Load(), errout)
		}
		if string(readSessionBytes(t, invalid)) != "invalid session" {
			t.Fatal("failed continue changed its target")
		}
		assertSessionClosed(t, active)
		assertSessionClosed(t, invalid)
	})
	t.Run("environment defaults", func(t *testing.T) {
		auth := make(chan string, 1)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			io.Copy(io.Discard, r.Body)
			auth <- r.Header.Get("Authorization")
			io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"done\"},\"finish_reason\":\"stop\"}]}\n\n")
		}))
		defer srv.Close()
		cmd := exec.Command(bin, "-once", "-prompt", "hello")
		cmd.Env = append(os.Environ(), "AGENT_MODEL=test-model", "AGENT_API_BASE="+srv.URL, "AGENT_API_KEY=test-placeholder")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("env defaults failed: %v %s", err, out)
		}
		if got := <-auth; got != "Bearer test-placeholder" {
			t.Fatalf("auth header %q", got)
		}
	})
	t.Run("help hides environment API key", func(t *testing.T) {
		cmd, out, errout := command(t, "http://127.0.0.1:1", "-h")
		const key = "help-must-not-display-this"
		cmd.Env = append(cmd.Env, "AGENT_API_KEY="+key)
		if code := exit(cmd.Run()); code != 0 || strings.Contains(out.String()+errout.String(), key) || !strings.Contains(errout.String(), "AGENT_API_KEY") {
			t.Fatalf("status=%d stdout=%q stderr=%q", code, out, errout)
		}
	})
	if runtime.GOOS == "windows" {
		t.Run("session conflict fails before reading pipe", func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "s.json")
			initial := newSession()
			if err := checkpointSession(path, initial); err != nil {
				t.Fatal(err)
			}
			initial.close()
			owner, err := reserveSession(path, false)
			if err != nil {
				t.Fatal(err)
			}
			defer owner.close()
			reader, writer, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			defer reader.Close()
			defer writer.Close() // no EOF: ownership must be checked first
			cmd, _, errout := command(t, "http://127.0.0.1:1", "-pipe", "-continue", path)
			cmd.Stdin = reader
			done := make(chan error, 1)
			go func() { done <- cmd.Run() }()
			select {
			case err := <-done:
				if code := exit(err); code != 1 || !strings.Contains(errout.String(), "Error opening session") {
					t.Fatalf("status=%d stderr=%q", code, errout)
				}
			case <-time.After(5 * time.Second):
				writer.Close()
				<-done
				t.Fatal("session conflict waited for stdin")
			}
		})
		return
	} // Windows console signals need a console.
	for _, sig := range []os.Signal{os.Interrupt, syscall.SIGTERM} {
		t.Run("signal "+sig.String(), func(t *testing.T) {
			ready := make(chan struct{}, 1)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				io.Copy(io.Discard, r.Body)
				w.Header().Set("Content-Type", "text/event-stream")
				io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n")
				w.(http.Flusher).Flush()
				ready <- struct{}{}
				<-r.Context().Done()
			}))
			defer srv.Close()
			path := filepath.Join(t.TempDir(), "s.json")
			cmd, _, errout := command(t, srv.URL, "-once", "-prompt", "hello", "-continue", path)
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			select {
			case <-ready:
			case <-time.After(5 * time.Second):
				cmd.Process.Kill()
				cmd.Wait()
				t.Fatal("request never started")
			}
			if err := cmd.Process.Signal(sig); err != nil {
				t.Fatal(err)
			}
			want := 130
			if sig == syscall.SIGTERM {
				want = 143
			}
			if got := exit(cmd.Wait()); got != want {
				t.Fatalf("status=%d want=%d stderr=%q", got, want, errout)
			}
			s, err := loadSession(path)
			if err != nil {
				t.Fatalf("interrupted checkpoint: %v", err)
			}
			defer s.close()
		})
	}
	for _, sig := range []os.Signal{os.Interrupt, syscall.SIGTERM} {
		t.Run("blocked stdout "+sig.String(), func(t *testing.T) {
			ready := make(chan struct{}, 1)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				io.Copy(io.Discard, r.Body)
				b, _ := json.Marshal(map[string]interface{}{"choices": []interface{}{map[string]interface{}{"delta": map[string]interface{}{"content": strings.Repeat("x", 1024*1024)}, "finish_reason": "stop"}}})
				io.WriteString(w, "data: "+string(b)+"\n\n")
				ready <- struct{}{}
			}))
			defer srv.Close()
			cmd, _, errout := command(t, srv.URL, "-once", "-prompt", "hello")
			reader, writer, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			defer reader.Close()
			defer writer.Close()
			cmd.Stdout = writer
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			select {
			case <-ready:
			case <-time.After(5 * time.Second):
				cmd.Process.Kill()
				cmd.Wait()
				t.Fatal("response never sent")
			}
			// Reading one byte proves the CLI entered the oversized write; leave
			// the remaining pipe unread to create real output backpressure.
			var first [1]byte
			if _, err := reader.Read(first[:]); err != nil {
				t.Fatal(err)
			}
			if err := cmd.Process.Signal(sig); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { done <- cmd.Wait() }()
			want := 130
			if sig == syscall.SIGTERM {
				want = 143
			}
			select {
			case err := <-done:
				if code := exit(err); code != want {
					t.Fatalf("status=%d stderr=%q", code, errout)
				}
			case <-time.After(3 * time.Second):
				cmd.Process.Kill()
				<-done
				t.Fatal("blocked output prevented shutdown")
			}
		})
	}
}

func TestMemoryToolsApplyInRequestOrder(t *testing.T) {
	s := newSession()
	defer s.close()
	calls := []ToolCall{
		{ID: "one", Type: "function", Function: FunctionCall{Name: "memory_edit", Arguments: `{"memory":"first objective"}`}},
		{ID: "two", Type: "function", Function: FunctionCall{Name: "memory_edit", Arguments: `{"memory":"second objective"}`}},
	}
	s.Transcript = append(s.Transcript, Message{Role: "user", Content: "task"}, Message{Role: "assistant", ToolCalls: calls})
	results := runToolsParallel(context.Background(), s, calls)
	if s.Memory != "second objective" || len(results) != 2 || results[0].ToolCallID != "one" || results[1].ToolCallID != "two" {
		t.Fatalf("nondeterministic tools: memory=%q results=%+v", s.Memory, results)
	}
	s.Transcript = append(s.Transcript, results...)
	assertPairing(t, s.Transcript)
}

func TestToolCancellationHasNoSideEffects(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s := newSession()
	r, _ := runTool(ctx, s, "memory_edit", `{"memory":"must not apply"}`)
	if s.Memory != "" || !strings.Contains(r, "canceled") {
		t.Fatalf("canceled tool mutated state: %s", r)
	}
}

func TestExecRejectsInvalidPreviewBeforeLaunch(t *testing.T) {
	for _, limit := range []string{"-1", "0.5", "9223372036854775808"} {
		r, _ := runTool(context.Background(), newSession(), "exec", `{"program":"does-not-exist","timeout_seconds":1,"max_output_chars":`+limit+`}`)
		if !strings.Contains(r, "invalid exec parameter") {
			t.Fatalf("invalid budget accepted: %s", r)
		}
	}
}

func TestReadImageRejectsDevices(t *testing.T) {
	if runtime.GOOS == "windows" {
		return
	}
	if _, _, err := readImage("/dev/zero"); err == nil || !strings.Contains(err.Error(), "regular file") {
		t.Fatalf("accepted device: %v", err)
	}
}
