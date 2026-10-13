package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"sync"
	"unicode/utf8"
)

// Each pipe has one independent preview. A shared file serializes raw writes;
// ordering between stdout and stderr is intentionally unspecified.
type commandOutput struct {
	file      *commandFile
	buffer    bytes.Buffer
	maxChars  int
	chars     int
	pending   []byte
	truncated bool
	copyErr   error
}

type commandFile struct {
	mu    sync.Mutex
	file  *os.File
	path  string
	bytes int64
}

func (f *commandFile) write(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	n, err := f.file.Write(p)
	f.bytes += int64(n)
	return n, err
}

// Called after Cmd.Wait joins both pipe readers.
func (f *commandFile) close() error { return f.file.Close() }

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
	out.file = &commandFile{file: f, path: path}
	return out, nil
}

// Retain copy errors even when Cmd.Wait prioritizes a nonzero exit status.
func (out *commandOutput) ReadFrom(reader io.Reader) (int64, error) {
	n, err := io.Copy(struct{ io.Writer }{out}, reader)
	out.copyErr = err
	return n, err
}

func (out *commandOutput) Write(p []byte) (int, error) {
	n := len(p)
	var err error
	if out.file != nil {
		n, err = out.file.write(p)
	}
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

func (out *commandOutput) finish() string {
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
	return out.buffer.String()
}
