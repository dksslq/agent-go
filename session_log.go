package main

// The selected session file is its own journal. Complete, checked records are
// appended and synced before absorbed messages leave memory; no spool or
// replacement file is needed.

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

type sessionCheckpoint struct {
	Version  int       `json:"version"`
	Base     int       `json:"base,omitempty"`
	Start    int       `json:"start"`
	Messages []Message `json:"messages"`
	Memory   string    `json:"memory"`
	Cursor   int       `json:"cursor"`
}

type sessionLogEnvelope struct {
	Checkpoint json.RawMessage `json:"checkpoint"`
	SHA256     string          `json:"sha256"`
}

type sessionLogState struct {
	Version int
	Base    int
	Count   int
	Cursor  int
	Memory  string
	Offset  int64
}

// A section reader fixes this pass to the size observed at entry. The caller
// can run another pass to retain only the final active suffix.
func readSessionLog(f *os.File, visit func(int, Message) error) (sessionLogState, error) {
	var state sessionLogState
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return state, err
	}
	info, err := f.Stat()
	if err != nil {
		return state, err
	}
	if !info.Mode().IsRegular() {
		return state, errors.New("session must be a regular file")
	}
	size := info.Size()
	reader := bufio.NewReader(io.NewSectionReader(f, 0, size))
	var start int64
	var leading byte
	for {
		b, err := reader.ReadByte()
		if err == io.EOF {
			return state, nil
		}
		if err != nil {
			return state, err
		}
		if b != ' ' && b != '\t' && b != '\r' && b != '\n' {
			leading = b
			break
		}
		start++
	}
	decoder := json.NewDecoder(io.NewSectionReader(f, start, size-start))
	first, err := decoder.Token()
	if err != nil {
		if leading != '{' {
			return state, fmt.Errorf("invalid session JSON: %w", err)
		}
		// An interrupted first journal record has no committed state yet.
		reader = bufio.NewReader(io.NewSectionReader(f, start, size-start))
		return readCheckpointLines(reader, start, state, visit)
	}
	if first != json.Delim('[') && first != json.Delim('{') {
		return state, errors.New("session must be an object or legacy array")
	}
	legacy := first == json.Delim('[')
	var firstKey string
	if first == json.Delim('{') {
		if !decoder.More() {
			return state, errors.New("session object is empty")
		}
		key, err := decoder.Token()
		if err != nil {
			reader = bufio.NewReader(io.NewSectionReader(f, start, size-start))
			return readCheckpointLines(reader, start, state, visit)
		}
		firstKey, _ = key.(string)
		legacy = firstKey != "checkpoint" && firstKey != "sha256"
	}
	if legacy {
		state, err = readLegacySession(decoder, first, firstKey, visit)
		if err != nil {
			return state, fmt.Errorf("invalid session JSON: %w", err)
		}
		state.Offset = start + decoder.InputOffset()
		reader = bufio.NewReader(io.NewSectionReader(f, state.Offset, size-state.Offset))
		return readCheckpointLines(reader, state.Offset, state, visit)
	}
	reader = bufio.NewReader(io.NewSectionReader(f, start, size-start))
	return readCheckpointLines(reader, start, state, visit)
}

func readCheckpointLines(reader *bufio.Reader, offset int64, state sessionLogState, visit func(int, Message) error) (sessionLogState, error) {
	for {
		line, err := reader.ReadBytes('\n')
		if err == io.EOF {
			// A newline is the commit marker, including for a complete JSON
			// object whose final write was interrupted before that marker.
			if len(bytes.TrimSpace(line)) == 0 {
				// Preserve trailing whitespace in old snapshots and committed
				// journals, rather than treating it as a torn append.
				state.Offset = offset + int64(len(line))
			} else if state.Count == 0 {
				return state, errors.New("invalid session JSON: no committed checkpoint")
			}
			return state, nil
		}
		if err != nil {
			return state, err
		}
		offset += int64(len(line))
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var envelope sessionLogEnvelope
		fields, err := decodeSessionObject(line, func(key string, d *json.Decoder) error {
			switch key {
			case "checkpoint":
				return d.Decode(&envelope.Checkpoint)
			case "sha256":
				return d.Decode(&envelope.SHA256)
			default:
				return fmt.Errorf("unknown journal field %q", key)
			}
		})
		if err != nil || !fields["checkpoint"] || !fields["sha256"] {
			if err == nil {
				err = errors.New("missing journal field")
			}
			return state, fmt.Errorf("invalid session checkpoint: %w", err)
		}
		digest := sha256.Sum256(envelope.Checkpoint)
		if envelope.SHA256 != hex.EncodeToString(digest[:]) {
			return state, errors.New("session checkpoint checksum mismatch")
		}
		checkpoint, err := decodeSessionCheckpoint(envelope.Checkpoint)
		if err != nil {
			return state, err
		}
		count, err := validateSessionCheckpoint(checkpoint)
		if err != nil {
			return state, err
		}
		if state.Count == 0 {
			if checkpoint.Start != 0 {
				return state, errors.New("session journal lacks an initial checkpoint")
			}
		} else if checkpoint.Start != state.Count || checkpoint.Base != state.Base || checkpoint.Cursor < state.Cursor {
			return state, errors.New("session checkpoint does not continue its predecessor")
		}
		if visit != nil {
			for i, message := range checkpoint.Messages {
				index := checkpoint.Start + i
				if checkpoint.Start == 0 && i > 0 {
					index += checkpoint.Base
				}
				if err := visit(index, message); err != nil {
					return state, err
				}
			}
		}
		state = sessionLogState{Version: checkpoint.Version, Base: checkpoint.Base, Count: count, Cursor: checkpoint.Cursor, Memory: checkpoint.Memory, Offset: offset}
	}
}

func decodeSessionObject(data []byte, field func(string, *json.Decoder) error) (map[string]bool, error) {
	d := json.NewDecoder(bytes.NewReader(data))
	first, err := d.Token()
	if err != nil || first != json.Delim('{') {
		return nil, errors.New("expected a JSON object")
	}
	seen := make(map[string]bool)
	for d.More() {
		key, err := d.Token()
		if err != nil {
			return nil, err
		}
		name, ok := key.(string)
		if !ok || seen[name] {
			return nil, errors.New("invalid or duplicate session field")
		}
		seen[name] = true
		if err := field(name, d); err != nil {
			return nil, err
		}
	}
	if end, err := d.Token(); err != nil || end != json.Delim('}') {
		return nil, errors.New("invalid session object")
	}
	if _, err := d.Token(); err != io.EOF {
		return nil, errors.New("trailing data in session object")
	}
	return seen, nil
}

func decodeSessionCheckpoint(data []byte) (sessionCheckpoint, error) {
	var checkpoint sessionCheckpoint
	seen, err := decodeSessionObject(data, func(key string, d *json.Decoder) error {
		switch key {
		case "version":
			return d.Decode(&checkpoint.Version)
		case "base":
			return d.Decode(&checkpoint.Base)
		case "start":
			return d.Decode(&checkpoint.Start)
		case "messages":
			return d.Decode(&checkpoint.Messages)
		case "memory":
			return d.Decode(&checkpoint.Memory)
		case "cursor":
			return d.Decode(&checkpoint.Cursor)
		default:
			return fmt.Errorf("unknown checkpoint field %q", key)
		}
	})
	if err != nil {
		return checkpoint, err
	}
	for _, key := range []string{"version", "start", "messages", "memory", "cursor"} {
		if !seen[key] {
			return checkpoint, fmt.Errorf("missing checkpoint field %q", key)
		}
	}
	return checkpoint, nil
}

func validateSessionCheckpoint(checkpoint sessionCheckpoint) (int, error) {
	if checkpoint.Version != sessionVersion || checkpoint.Base < 0 || checkpoint.Start < 0 {
		return 0, errors.New("invalid checkpoint version or index")
	}
	count := checkpoint.Start
	v := transcriptValidator{count: 1}
	if checkpoint.Start == 0 {
		count = checkpoint.Base
		v.count = 0
		if len(checkpoint.Messages) == 0 {
			return 0, errors.New("initial checkpoint requires a system message")
		}
	} else if checkpoint.Start <= checkpoint.Base {
		return 0, errors.New("checkpoint start precedes its retained history")
	}
	if count > int(^uint(0)>>1)-len(checkpoint.Messages) {
		return 0, errors.New("checkpoint message count overflow")
	}
	count += len(checkpoint.Messages)
	if checkpoint.Cursor <= checkpoint.Base || checkpoint.Cursor > count {
		return 0, errors.New("checkpoint cursor outside its history")
	}
	if (checkpoint.Base > 0 || checkpoint.Cursor > checkpoint.Base+1) && strings.TrimSpace(checkpoint.Memory) == "" {
		return 0, errors.New("compacted session requires semantic memory")
	}
	for i, message := range checkpoint.Messages {
		if err := v.add(message); err != nil {
			return 0, err
		}
		index := checkpoint.Start + i
		if checkpoint.Start == 0 && i > 0 {
			index += checkpoint.Base
		}
		if index == checkpoint.Cursor && message.Role == "tool" {
			return 0, errors.New("checkpoint cursor splits a tool-call group")
		}
	}
	if err := v.finish(); err != nil {
		return 0, err
	}
	return count, nil
}

func appendSessionCheckpoint(f *os.File, offset int64, checkpoint sessionCheckpoint) (int64, error) {
	if _, err := validateSessionCheckpoint(checkpoint); err != nil {
		return offset, err
	}
	body, err := json.Marshal(checkpoint)
	if err != nil {
		return offset, err
	}
	digest := sha256.Sum256(body)
	data, err := json.Marshal(sessionLogEnvelope{Checkpoint: body, SHA256: hex.EncodeToString(digest[:])})
	if err != nil {
		return offset, err
	}
	info, err := f.Stat()
	if err != nil {
		return offset, err
	}
	if !info.Mode().IsRegular() || offset < 0 || offset > info.Size() {
		return offset, errors.New("invalid checkpoint file or offset")
	}
	rollback := func(cause error) (int64, error) {
		truncateErr := f.Truncate(offset)
		_, seekErr := f.Seek(offset, io.SeekStart)
		syncErr := f.Sync()
		if err := errors.Join(truncateErr, seekErr, syncErr); err != nil {
			cause = errors.Join(cause, fmt.Errorf("rollback checkpoint: %w", err))
		}
		return offset, cause
	}
	if err := f.Truncate(offset); err != nil {
		return rollback(err)
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return rollback(err)
	}
	// A separator also supports old snapshots that had no trailing newline.
	if _, err := f.Write([]byte{'\n'}); err != nil {
		return rollback(err)
	}
	if _, err := f.Write(data); err != nil {
		return rollback(err)
	}
	if _, err := f.Write([]byte{'\n'}); err != nil {
		return rollback(err)
	}
	if err := f.Sync(); err != nil {
		return rollback(err)
	}
	return offset + int64(len(data)) + 2, nil
}

func readLegacySession(d *json.Decoder, first json.Token, firstKey string, visit func(int, Message) error) (sessionLogState, error) {
	var state sessionLogState
	v := transcriptValidator{}
	readMessages := func() error {
		for d.More() {
			var message Message
			if err := d.Decode(&message); err != nil {
				return err
			}
			if err := v.add(message); err != nil {
				return err
			}
			if visit != nil {
				if err := visit(v.count-1, message); err != nil {
					return err
				}
			}
		}
		if end, err := d.Token(); err != nil || end != json.Delim(']') {
			return errors.New("invalid transcript array")
		}
		return v.finish()
	}
	if first == json.Delim('[') {
		if err := readMessages(); err != nil {
			return state, err
		}
	} else if first == json.Delim('{') {
		seen := make(map[string]bool)
		key := firstKey
		for {
			if seen[key] {
				return state, errors.New("duplicate session field")
			}
			seen[key] = true
			var err error
			switch key {
			case "version":
				err = d.Decode(&state.Version)
			case "memory":
				err = d.Decode(&state.Memory)
			case "cursor":
				err = d.Decode(&state.Cursor)
			case "transcript":
				start, tokenErr := d.Token()
				err = tokenErr
				if err == nil && start != json.Delim('[') {
					err = errors.New("transcript must be an array")
				}
				if err == nil {
					err = readMessages()
				}
			default:
				err = skipJSONValue(d)
			}
			if err != nil {
				return state, err
			}
			if !d.More() {
				break
			}
			token, err := d.Token()
			if err != nil {
				return state, err
			}
			var ok bool
			key, ok = token.(string)
			if !ok {
				return state, errors.New("invalid session field")
			}
		}
		if end, err := d.Token(); err != nil || end != json.Delim('}') {
			return state, errors.New("invalid session object")
		}
		if !seen["transcript"] {
			return state, errors.New("missing transcript")
		}
	} else {
		return state, errors.New("session must be an object or legacy array")
	}
	if state.Version < 0 || state.Version > 3 {
		return state, fmt.Errorf("unsupported session version %d", state.Version)
	}
	state.Count = v.count
	if state.Cursor < 1 {
		state.Cursor = 1
	}
	if state.Cursor > state.Count {
		state.Cursor = state.Count
	}
	if state.Cursor > 1 && strings.TrimSpace(state.Memory) == "" {
		return state, errors.New("compacted session requires semantic memory")
	}
	return state, nil
}
