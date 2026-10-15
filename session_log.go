package main

// The selected session file is its own journal, stored as one JSON document.
// Messages are object members keyed by their absolute transcript index
// ("m13"), and a "state" member (base, cursor, memory) marks compaction
// points. Appends extend the document in place: the writer truncates the
// closing brace, appends the new members, and re-closes the document before
// syncing. No version tags and no checksums: the document grammar itself is
// the format, a torn write simply leaves an unterminated member, and the
// streaming reader stops at the last complete member. Files carrying a
// leading "version": 5 member (v2.5.0 output), v4 journals and legacy
// arrays/objects still load; continuing one appends the same document form
// after the old bytes.

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
)

const legacyJournalVersion = 4

// sessionCheckpoint is the record format of v4 journals; the writer never
// produces them, but old files still load.
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
	SHA256     string          `json:"sha256"` // accepted for old journals; never verified
}

// sessionState is the compaction state recorded in the document. Members
// after a state member are committed; base is constant for the life of a
// file, so repeated state members (memory_edit) carry the same base and the
// last one wins.
type sessionState struct {
	Base   int    `json:"base"`
	Cursor int    `json:"cursor"`
	Memory string `json:"memory"`
}

type sessionMessage struct {
	Index   int
	Message Message
}

type sessionLogState struct {
	Base    int
	Count   int
	Cursor  int
	Memory  string
	Offset  int64
	DocOpen bool // a session document with committed members ends at Offset
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
	return readSessionLogPrefix(f, info.Size(), visit)
}

// Read only the committed prefix when copying a session to a new snapshot.
func readSessionLogPrefix(f *os.File, size int64, visit func(int, Message) error) (sessionLogState, error) {
	var state sessionLogState
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
		return readCheckpointLines(f, reader, start, size, state, visit)
	}
	if first != json.Delim('[') && first != json.Delim('{') {
		return state, errors.New("session must be an object or legacy array")
	}
	legacy := first == json.Delim('[')
	var firstKey string
	var preVersion *int
	if first == json.Delim('{') {
		if !decoder.More() {
			return state, errors.New("session object is empty")
		}
		key, err := decoder.Token()
		if err != nil {
			reader = bufio.NewReader(io.NewSectionReader(f, start, size-start))
			return readCheckpointLines(f, reader, start, size, state, visit)
		}
		firstKey, _ = key.(string)
		// Leading keys split the formats: journal envelopes and legacy object
		// fields are a closed set, so anything else — "m<index>", "state" or
		// a corrupted member name — is read as a session document.
		switch firstKey {
		case "checkpoint", "sha256": // v4 journal envelope line
		case "version", "memory", "cursor", "transcript": // legacy object
			legacy = true
		default:
			return readSessionDoc(f, start, size, sessionLogState{}, visit)
		}
		if firstKey == "version" {
			value, verr := decoder.Token()
			if verr == nil {
				if number, ok := value.(float64); ok && number == 5 {
					// A v2.5.0 document carries its version; the format is
					// otherwise identical to the current one.
					return readSessionDoc(f, start, size, sessionLogState{}, visit)
				}
				if number, ok := value.(float64); ok {
					version := int(number)
					preVersion = &version
				}
				// A non-5 leading version belongs to a legacy object.
				state, err = readLegacySession(decoder, first, firstKey, preVersion, visit)
				if err != nil {
					return state, fmt.Errorf("invalid session JSON: %w", err)
				}
				state.Offset = start + decoder.InputOffset()
				reader = bufio.NewReader(io.NewSectionReader(f, state.Offset, size-state.Offset))
				return readCheckpointLines(f, reader, state.Offset, size, state, visit)
			}
			// Torn after "version": fall through and treat as journal lines.
			legacy = false
		}
	}
	if legacy {
		state, err = readLegacySession(decoder, first, firstKey, preVersion, visit)
		if err != nil {
			return state, fmt.Errorf("invalid session JSON: %w", err)
		}
		state.Offset = start + decoder.InputOffset()
		reader = bufio.NewReader(io.NewSectionReader(f, state.Offset, size-state.Offset))
		return readCheckpointLines(f, reader, state.Offset, size, state, visit)
	}
	reader = bufio.NewReader(io.NewSectionReader(f, start, size-start))
	return readCheckpointLines(f, reader, start, size, state, visit)
}

// docReadErr classifies a decode failure: end-of-input means the write was
// torn and everything decoded so far is committed; anything else is real
// corruption and stays fatal.
func docReadErr(err error) error {
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return nil
	}
	return err
}

// readSessionDoc streams one session document starting at start. prefix
// carries the state of a preceding v4 journal or legacy object; when the
// document is a continuation (prefix.Count > 0) its first message key must
// continue that count and its state must keep the same base. A v2.5.0
// document may open with "version": 5; current documents omit it.
func readSessionDoc(f *os.File, start, size int64, prefix sessionLogState, visit func(int, Message) error) (sessionLogState, error) {
	state := prefix
	fresh := prefix.Count == 0
	d := json.NewDecoder(io.NewSectionReader(f, start, size-start))
	open, err := d.Token()
	if err != nil {
		if err := docReadErr(err); err != nil {
			return state, fmt.Errorf("invalid session document: %w", err)
		}
		state.DocOpen = false
		state.Offset = start
		return state, nil
	}
	if open != json.Delim('{') {
		return state, errors.New("invalid session document")
	}
	v := transcriptValidator{count: 1}
	if fresh {
		v.count = 0
	}
	base, baseKnown := prefix.Base, !fresh
	members, lastKey := 0, -1
	seenVersion, seenState := false, false
	var memberEnd int64
	for d.InputOffset() < size-start && d.More() {
		keyToken, err := d.Token()
		if err != nil {
			if err := docReadErr(err); err != nil {
				return state, fmt.Errorf("invalid session document: %w", err)
			}
			break
		}
		key, ok := keyToken.(string)
		if !ok {
			return state, errors.New("invalid session document field")
		}
		switch {
		case key == "version":
			if seenVersion || members != 0 || seenState {
				return state, errors.New("version must be the first document field")
			}
			var version float64
			if err := d.Decode(&version); err != nil {
				if err := docReadErr(err); err != nil {
					return state, fmt.Errorf("invalid session document version: %w", err)
				}
				break
			}
			if version != 5 {
				return state, fmt.Errorf("unsupported session document version %d", int(version))
			}
			seenVersion = true
		case key == "state":
			var st sessionState
			if err := d.Decode(&st); err != nil {
				if err := docReadErr(err); err != nil {
					return state, fmt.Errorf("invalid session state: %w", err)
				}
				break
			}
			count := prefix.Count
			if lastKey >= 0 {
				count = lastKey + 1
			}
			if fresh && members == 1 {
				// Only m0 remains after full compaction; there is no suffix
				// index from which to infer the archived message count.
				count = st.Base + 1
			}
			if st.Base < 0 || st.Cursor <= st.Base || st.Cursor > count {
				return state, errors.New("session state cursor outside its history")
			}
			if (st.Base > 0 || st.Cursor > st.Base+1) && strings.TrimSpace(st.Memory) == "" {
				return state, errors.New("compacted session requires semantic memory")
			}
			if baseKnown && st.Base != base {
				return state, errors.New("session state base does not match the document")
			}
			base, baseKnown = st.Base, true
			state.Base, state.Cursor, state.Memory = st.Base, st.Cursor, st.Memory
			seenState = true
			memberEnd = d.InputOffset()
		case strings.HasPrefix(key, "m") && len(key) > 1:
			index, err := strconv.Atoi(key[1:])
			if err != nil || index < 0 {
				return state, fmt.Errorf("invalid session document field %q", key)
			}
			if members == 0 {
				if fresh && index != 0 {
					return state, errors.New("session document must begin with the system message")
				}
				if !fresh && index != prefix.Count {
					return state, errors.New("session document does not continue its predecessor")
				}
			} else if index != lastKey+1 {
				if !(fresh && members == 1 && index > 1) {
					return state, errors.New("session document message index is not contiguous")
				}
			}
			if fresh && members == 1 {
				// The member after m0 starts the retained suffix; its index
				// reveals the compaction gap.
				if baseKnown && index-1 != base {
					return state, errors.New("session document gap does not match its base")
				}
				base, baseKnown = index-1, true
			}
			var m Message
			if err := d.Decode(&m); err != nil {
				if err := docReadErr(err); err != nil {
					return state, fmt.Errorf("invalid session message: %w", err)
				}
				break
			}
			if err := v.add(m); err != nil {
				return state, err
			}
			if visit != nil {
				if err := visit(index, m); err != nil {
					return state, err
				}
			}
			lastKey = index
			members++
			memberEnd = d.InputOffset()
		default:
			return state, fmt.Errorf("unknown session document field %q", key)
		}
	}
	// Snapshot copying intentionally stops at the last committed member,
	// before the closing brace. Do not ask the decoder for another token
	// once that explicit boundary has been reached.
	if d.InputOffset() < size-start {
		if closing, err := d.Token(); err != nil {
			if err := docReadErr(err); err != nil {
				return state, fmt.Errorf("invalid session document: %w", err)
			}
		} else if closing != json.Delim('}') {
			return state, errors.New("invalid session document")
		} else if _, err := d.Token(); err == nil || !errors.Is(err, io.EOF) {
			return state, errors.New("trailing data in session document")
		}
	}
	if members > 0 {
		if err := v.finish(); err != nil {
			return state, err
		}
		state.Count = max(lastKey+1, base+1)
		state.Base = base
		state.Offset = start + memberEnd
		state.DocOpen = true
		if fresh && !seenState {
			return state, errors.New("session document lacks its initial state")
		}
	} else if seenState {
		state.Offset = start + memberEnd
		state.DocOpen = true
	} else {
		state.Offset = start
		state.DocOpen = false
	}
	return state, nil
}

func readCheckpointLines(f *os.File, reader *bufio.Reader, offset int64, size int64, state sessionLogState, visit func(int, Message) error) (sessionLogState, error) {
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
		if bytes.Equal(bytes.TrimSpace(line), []byte("{")) {
			// A v5 document continues the journal here.
			return readSessionDoc(f, offset-int64(len(line)), size, state, visit)
		}
		var envelope sessionLogEnvelope
		fields, err := decodeSessionObject(line, func(key string, d *json.Decoder) error {
			switch key {
			case "checkpoint":
				return d.Decode(&envelope.Checkpoint)
			case "sha256":
				// Accepted for old journals; deliberately not verified and
				// not even required — only the checkpoint matters.
				var ignored string
				return d.Decode(&ignored)
			default:
				return fmt.Errorf("unknown journal field %q", key)
			}
		})
		if err != nil || !fields["checkpoint"] {
			if err == nil {
				err = errors.New("missing journal field")
			}
			return state, fmt.Errorf("invalid session checkpoint: %w", err)
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
		state = sessionLogState{Base: checkpoint.Base, Count: count, Cursor: checkpoint.Cursor, Memory: checkpoint.Memory, Offset: offset}
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
	if checkpoint.Version != legacyJournalVersion || checkpoint.Base < 0 || checkpoint.Start < 0 {
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

// sessionUpdate describes one document append. OpenDoc continues the document
// whose last committed member ends at offset; otherwise a new document is
// opened at offset (after a legacy or v4 prefix, or at the start of a fresh
// file). A nil State appends messages only.
type sessionUpdate struct {
	OpenDoc  bool
	Messages []sessionMessage
	State    *sessionState
}

// writeSessionUpdate appends members to the session document, then closes and
// syncs it. It returns the offset of the end of the last member — the
// position the next append or a recovery truncates back to. A failed write
// rolls the file back to the incoming offset.
func writeSessionUpdate(f *os.File, offset int64, update sessionUpdate) (int64, error) {
	if len(update.Messages) == 0 && update.State == nil {
		return offset, nil
	}
	var stateJSON []byte
	if update.State != nil {
		var err error
		stateJSON, err = json.Marshal(*update.State)
		if err != nil {
			return offset, err
		}
	}
	info, err := f.Stat()
	if err != nil {
		return offset, err
	}
	if !info.Mode().IsRegular() || offset < 0 || offset > info.Size() {
		return offset, errors.New("invalid checkpoint file or offset")
	}
	separator := false
	if !update.OpenDoc && offset > 0 {
		// Keep the document's opening brace on its own line after a legacy
		// object or journal record that lacks a trailing newline.
		if _, err := f.Seek(offset-1, io.SeekStart); err != nil {
			return offset, err
		}
		var previous [1]byte
		if _, err := io.ReadFull(f, previous[:]); err != nil {
			return offset, err
		}
		separator = previous[0] != '\n'
	}
	var tail bytes.Buffer
	if !update.OpenDoc {
		tail.WriteString("{")
	}
	// The first member of a fresh document opens it without a separator;
	// appends to an open document continue after the last member's comma.
	first := !update.OpenDoc
	for _, m := range update.Messages {
		data, err := json.Marshal(m.Message)
		if err != nil {
			return offset, err
		}
		if first {
			tail.WriteString("\n  \"m")
			first = false
		} else {
			tail.WriteString(",\n  \"m")
		}
		tail.WriteString(strconv.Itoa(m.Index))
		tail.WriteString("\": ")
		tail.Write(data)
	}
	if update.State != nil {
		if first {
			tail.WriteString("\n  \"state\": ")
			first = false
		} else {
			tail.WriteString(",\n  \"state\": ")
		}
		tail.Write(stateJSON)
	}
	tail.WriteString("\n}\n")
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
	if separator {
		if _, err := f.Write([]byte{'\n'}); err != nil {
			return rollback(err)
		}
	}
	written := int64(0)
	if separator {
		written++
	}
	if _, err := f.Write(tail.Bytes()); err != nil {
		return rollback(err)
	}
	written += int64(tail.Len())
	if err := f.Sync(); err != nil {
		return rollback(err)
	}
	// The document closes with "\n}\n"; the next append resumes at the end
	// of the last member, matching what a streaming read reports.
	return offset + written - 3, nil
}

func readLegacySession(d *json.Decoder, first json.Token, firstKey string, preVersion *int, visit func(int, Message) error) (sessionLogState, error) {
	var state sessionLogState
	legacyVersion := 0
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
				if preVersion != nil {
					legacyVersion = *preVersion
					preVersion = nil
				} else {
					err = d.Decode(&legacyVersion)
				}
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
	if legacyVersion < 0 || legacyVersion > 3 {
		return state, fmt.Errorf("unsupported session version %d", legacyVersion)
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
