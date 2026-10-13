//go:build linux

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
	"unsafe"
)

func TestCLIMultilinePaste(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "agent")
	if out, err := exec.Command("go", "build", "-race", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v %s", err, out)
	}
	requests := make(chan ChatRequest, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var q ChatRequest
		if err := json.NewDecoder(r.Body).Decode(&q); err != nil {
			t.Error(err)
			return
		}
		requests <- q
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"done\"},\"finish_reason\":\"stop\"}]}\n\n")
	}))
	defer srv.Close()
	master, slave := openPTY(t)
	defer master.Close()
	defer slave.Close()
	terminalState := func() syscall.Termios {
		t.Helper()
		var state syscall.Termios
		_, _, err := syscall.Syscall(syscall.SYS_IOCTL, slave.Fd(), syscall.TCGETS, uintptr(unsafe.Pointer(&state)))
		if err != 0 {
			t.Fatal(err)
		}
		return state
	}
	initialState := terminalState()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "-model", "test", "-api-base", srv.URL)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	defer cancel()
	// One reader reports prompt completion; terminal output never backs up.
	prompts := make(chan struct{}, 8)
	disabled := make(chan struct{}, 1)
	readerDone := make(chan struct{})
	defer func() {
		master.Close()
		select {
		case <-readerDone:
		case <-time.After(time.Second):
			t.Error("terminal reader did not stop")
		}
	}()
	go func() {
		defer close(readerDone)
		reader := bufio.NewReader(master)
		previous := byte(0)
		var tail []byte
		for {
			b, err := reader.ReadByte()
			if err != nil {
				return
			}
			tail = append(tail, b)
			if len(tail) > len(pasteStop) {
				tail = tail[len(tail)-len(pasteStop):]
			}
			if string(tail) == pasteStop {
				disabled <- struct{}{}
			}
			if previous == '>' && b == ' ' {
				prompts <- struct{}{}
			}
			previous = b
		}
	}()
	awaitPrompt := func() {
		t.Helper()
		select {
		case <-prompts:
		case <-ctx.Done():
			t.Fatal("terminal prompt timed out")
		}
	}
	send := func(s string) {
		t.Helper()
		if _, err := master.WriteString(s); err != nil {
			t.Fatal(err)
		}
	}
	assertRequest := func(want string) {
		t.Helper()
		select {
		case q := <-requests:
			if got := q.Messages[len(q.Messages)-1].Content; got != want {
				t.Fatalf("input altered: %q, want %q", got, want)
			}
		case <-ctx.Done():
			t.Fatal("request timed out")
		}
	}
	awaitPrompt()
	payload := " 代码\r\n\t/save ignored\nquit\n\x1b[A\x7f"
	send("\x1b[200~" + payload + "\x1b[201~")
	select {
	case q := <-requests:
		t.Fatalf("paste auto-submitted: %+v", q)
	case <-time.After(100 * time.Millisecond):
	}
	send("\r")
	assertRequest(payload)
	awaitPrompt()
	send("/paste\r")
	send("/save ignored\rquit\r\x04")
	assertRequest("/save ignored\nquit\n")
	awaitPrompt()
	send("quit\r")
	select {
	case err := <-exited:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("exit timed out")
	}
	select {
	case q := <-requests:
		t.Fatalf("unexpected request: %+v", q)
	default:
	}
	if after := terminalState(); after != initialState {
		t.Fatal("exit did not restore terminal state")
	}
	select {
	case <-disabled:
	case <-ctx.Done():
		t.Fatal("exit did not disable bracketed paste")
	}
}
