package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"unicode/utf8"
)

// commandOutput drains the entire stream while retaining only the requested
// preview. Passing the same writer for stdout and stderr gives os/exec one
// shared pipe and one writer goroutine, preserving their combined order.
type commandOutput struct {
	file      *os.File
	path      string
	buffer    bytes.Buffer
	maxChars  int
	chars     int
	pending   []byte
	truncated bool
	bytes     int64
}

func newCommandOutput(maxChars int, cwd, path string) (*commandOutput, error) {
	out := &commandOutput{maxChars: maxChars}
	if path == "" {
		return out, nil
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(cwd, path)
	}
	path, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return nil, err
	}
	out.file, out.path = f, path
	return out, nil
}

func (out *commandOutput) Write(p []byte) (int, error) {
	n := len(p)
	var err error
	if out.file != nil {
		n, err = out.file.Write(p)
	}
	out.bytes += int64(n)
	p = p[:n]
	if out.maxChars <= 0 {
		if out.file == nil {
			out.buffer.Write(p)
		}
		return n, err
	}
	for (len(p) > 0 || len(out.pending) > 0) && out.chars < out.maxChars {
		if len(out.pending) > 0 {
			if !utf8.FullRune(out.pending) {
				if len(p) == 0 {
					break
				}
				out.pending = append(out.pending, p[0])
				p = p[1:]
				continue
			}
			_, size := utf8.DecodeRune(out.pending)
			out.buffer.Write(out.pending[:size])
			out.chars++
			out.pending = out.pending[size:]
			continue
		}
		if !utf8.FullRune(p) {
			out.pending = append(out.pending, p...)
			break
		}
		_, size := utf8.DecodeRune(p)
		out.buffer.Write(p[:size])
		p = p[size:]
		out.chars++
	}
	if (len(p) > 0 || len(out.pending) > 0) && out.chars == out.maxChars {
		out.truncated = true
	}
	return n, err
}

func (out *commandOutput) finish() (string, error) {
	for len(out.pending) > 0 {
		_, size := utf8.DecodeRune(out.pending)
		if out.chars < out.maxChars {
			out.buffer.Write(out.pending[:size])
			out.chars++
		} else {
			out.truncated = true
		}
		out.pending = out.pending[size:]
	}
	result := out.buffer.String()
	if out.truncated {
		result += "\n...[truncated]"
	}
	if out.file == nil {
		return result, nil
	}
	err := out.file.Close()
	header := fmt.Sprintf("Output saved to %s (%d bytes)", out.path, out.bytes)
	if result != "" {
		header += "\n" + result
	}
	return header, err
}
