package main

import (
	"bufio"
	"context"
	"io"
	"os"
	"strings"
	"testing"
)

func quietRawInput(t *testing.T) {
	t.Helper()
	file, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	oldRaw, oldWriter, oldErr := termRaw, stdoutWriter, outputErr
	termRaw, stdoutWriter, outputErr = true, newOutputWriter(ctx, file), nil
	t.Cleanup(func() { cancel(); file.Close(); termRaw, stdoutWriter, outputErr = oldRaw, oldWriter, oldErr })
}

func TestTerminalPaste(t *testing.T) {
	quietRawInput(t)
	payload := " 代码\r\n\tquit\n/save ignored\n\x1b[A\x7f\x04\x1b[20x" + strings.Repeat("大", 20000)
	reader := bufio.NewReader(strings.NewReader("\x1b[200~" + payload + "\x1b[201~\rnext\r"))
	got, err := readLine(reader)
	if err != nil || got != payload {
		t.Fatalf("paste: length %d, error %v", len(got), err)
	}
	got, err = readLine(reader)
	if err != nil || got != "next" {
		t.Fatalf("next input: %q %v", got, err)
	}
}

func TestTerminalIncompletePaste(t *testing.T) {
	quietRawInput(t)
	for _, input := range []string{"\x1b[200~quit\n", "\x1b[200~text\x1b[201"} {
		got, err := readLine(bufio.NewReader(strings.NewReader(input)))
		if got != "" || err != io.ErrUnexpectedEOF {
			t.Fatalf("partial paste submitted: %q %v", got, err)
		}
	}
}

func TestTerminalMultilineKeys(t *testing.T) {
	quietRawInput(t)
	for _, key := range []string{"\x1b\r", "\x1b[13;2u", "\x1b[27;2;13~"} {
		got, err := readLine(bufio.NewReader(strings.NewReader("a" + key + "b\r")))
		if got != "a\nb" || err != nil {
			t.Fatalf("key %q: %q %v", key, got, err)
		}
	}
	got, err := readInputLine(bufio.NewReader(strings.NewReader("/save x\rquit\r\x04")), true)
	if got != "/save x\nquit\n" || err != nil {
		t.Fatalf("explicit paste: %q %v", got, err)
	}
	got, err = readInputLine(bufio.NewReader(strings.NewReader("unfinished")), true)
	if err != io.ErrUnexpectedEOF {
		t.Fatalf("unfinished: %q %v", got, err)
	}
}
