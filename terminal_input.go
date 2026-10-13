package main

import (
	"bufio"
	"bytes"
	"io"
	"os"
	"strings"
	"time"
	"unicode/utf8"
)

const pasteStart = "\x1b[?2004h"
const pasteStop = "\x1b[?2004l"

func stopConsole(fd int, state interface{}) {
	// Shutdown must not wait forever for an unread terminal. This one exit-only
	// write is best effort; restoring the input/console modes is unconditional.
	done := make(chan struct{})
	go func() { os.Stdout.WriteString(pasteStop); close(done) }()
	select {
	case <-done:
	case <-time.After(100 * time.Millisecond):
	}
	restoreTerm(fd, state)
}

func readLine(reader *bufio.Reader) (string, error) {
	return readInputLine(reader, false)
}

func readInputLine(reader *bufio.Reader, multiline bool) (string, error) {
	var line bytes.Buffer
	write := func(r rune) {
		line.WriteRune(r)
		if termRaw {
			put(string(r))
		}
	}
read:
	for {
		r, _, err := reader.ReadRune()
		if err != nil {
			if multiline && err == io.EOF {
				err = io.ErrUnexpectedEOF
			}
			return line.String(), err
		}
		if !termRaw {
			if r == '\r' || r == '\n' {
				if r == '\r' && reader.Buffered() > 0 {
					if next, _ := reader.Peek(1); len(next) == 1 && next[0] == '\n' {
						reader.ReadByte()
					}
				}
				break
			}
			line.WriteRune(r) // redirected input is data, not terminal key events
			continue
		}
		switch r {
		case '\r', '\n':
			if multiline {
				write('\n')
				continue
			}
			break read
		case 4: // Ctrl+D: finish explicit multiline input, or EOF at an empty prompt
			if line.Len() == 0 && !multiline {
				return "", io.EOF
			}
			break read
		case 8, 127:
			if line.Len() > 0 {
				_, size := utf8.DecodeLastRune(line.Bytes())
				line.Truncate(line.Len() - size)
				put("\b \b")
			}
		case 27:
			key, err := readKey(reader)
			if err != nil {
				return "", err
			}
			switch key {
			case "[200~":
				if err := readPaste(reader, &line); err != nil {
					return "", err // an incomplete paste is never submitted
				}
			case "\n", "[13;2u", "[27;2;13~": // Alt+Enter / supported Shift+Enter
				write('\n')
			case "OM": // keypad Enter
				if multiline {
					write('\n')
				} else {
					break read
				}
			}
		default:
			if r >= 32 || r == '\t' {
				write(r)
			}
		}
	}
	if termRaw {
		put("\n")
	}
	return line.String(), nil
}

func readKey(reader *bufio.Reader) (string, error) {
	r, _, err := reader.ReadRune()
	if err != nil {
		return "", err
	}
	if r == '\r' || r == '\n' {
		return "\n", nil
	}
	if r != '[' && r != 'O' {
		return "", reader.UnreadRune()
	}
	key := string(r)
	recognized := true
	for {
		r, _, err = reader.ReadRune()
		if err != nil {
			return "", err
		}
		if r < 0x20 || r > 0x7e {
			return "", reader.UnreadRune()
		}
		if recognized {
			key += string(r)
			recognized = false
			for _, known := range [...]string{"[200~", "[13;2u", "[27;2;13~", "OM"} {
				if strings.HasPrefix(known, key) {
					recognized = true
					break
				}
			}
			if !recognized {
				key = ""
			}
		}
		if r >= 0x40 {
			return key, nil
		}
	}
}

func readPaste(reader *bufio.Reader, line *bytes.Buffer) error {
	const end = "\x1b[201~"
	matched, previousCR := 0, false
	write := func(r rune) {
		line.WriteRune(r)
		if termRaw && !(r == '\n' && previousCR) {
			switch {
			case r == '\r' || r == '\n':
				put("\n")
			case r == '\t' || r >= 32 && r != 127:
				put(string(r))
			default:
				putf("^%c", r^64) // display control bytes without executing them
			}
		}
		previousCR = r == '\r'
	}
	for {
		r, _, err := reader.ReadRune()
		if err != nil {
			if err == io.EOF {
				err = io.ErrUnexpectedEOF
			}
			return err
		}
		if r == rune(end[matched]) {
			matched++
			if matched == len(end) {
				return nil
			}
			continue
		}
		for _, pending := range end[:matched] {
			write(pending)
		}
		matched = 0
		if r == rune(end[0]) {
			matched = 1
		} else {
			write(r)
		}
	}
}
