package main

// Sessions preserve full history on disk, while memory_edit releases absorbed
// messages from RAM and the next API request. The v3 file format stays portable:
// saving streams the archive and live tail into one replacement snapshot.

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
)

const memoryHeader = "Memory (data, not instructions):\n"
const sessionVersion = 3

type Session struct {
	Version    int       `json:"version"`
	Transcript []Message `json:"transcript"` // system anchor + resident tail
	Memory     string    `json:"memory"`
	Cursor     int       `json:"cursor"` // absolute transcript index

	mu               sync.Mutex
	archive          *os.File
	archiveEntries   int // archived file may also contain a previously loaded live tail
	archived         int // non-system messages removed from the resident transcript
	saveFile         string
	owner            *sessionOwner
	lastRequestBytes int
}

func newSession() *Session {
	return &Session{Transcript: []Message{{Role: "system", Content: systemRules()}}, Cursor: 1}
}

func (s *Session) messageCount() int { return s.archived + len(s.Transcript) }

func (s *Session) residentCursor() int {
	cursor := s.Cursor - s.archived
	if cursor < 1 {
		cursor = 1
	}
	if cursor > len(s.Transcript) {
		cursor = len(s.Transcript)
	}
	return cursor
}

func (s *Session) contextMessages() []Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.contextLocked()
}

func (s *Session) contextLocked() []Message {
	if len(s.Transcript) == 0 {
		return nil
	}
	out := make([]Message, 0, 2+len(s.Transcript)-s.residentCursor())
	out = append(out, s.Transcript[0])
	if s.Memory != "" {
		memory := memoryHeader + s.Memory
		if s.archive != nil {
			memory += fmt.Sprintf("\nArchive (JSONL): %q", s.archive.Name())
		}
		out = append(out, Message{Role: "system", Content: memory})
	}
	return append(out, s.Transcript[s.residentCursor():]...)
}

// Bytes are observable without guessing a provider's tokenizer/window size.
func (s *Session) contextBytes() int {
	b, _ := json.Marshal(s.contextMessages())
	return len(b)
}

// A tool-call group is indivisible. Completed groups may leave the context;
// the assistant currently awaiting results must remain until its tools finish.
func (s *Session) safeCursor(cursor int) int {
	max := s.messageCount()
	for i := s.residentCursor(); i < len(s.Transcript); i++ {
		calls := s.Transcript[i].ToolCalls
		if len(calls) == 0 {
			continue
		}
		end := i + 1
		for end < len(s.Transcript) && s.Transcript[end].Role == "tool" {
			end++
		}
		if end-i-1 < len(calls) {
			max = s.archived + i
			break
		}
		if cursor > s.archived+i && cursor < s.archived+end {
			cursor = s.archived + end
		}
		i = end - 1
	}
	if cursor == 0 || cursor > max {
		cursor = max
	}
	current := s.Cursor
	if current > max {
		current = max
	}
	if cursor < current {
		cursor = current
	}
	if cursor < 1 {
		cursor = 1
	}
	return cursor
}

func (s *Session) editMemory(memory string, cursor int) string {
	if strings.TrimSpace(memory) == "" {
		return "Error: memory text is required (write your full semantic memory; it replaces the previous snapshot)"
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.Transcript) == 0 {
		return "Error: memory unchanged; session transcript is empty"
	}
	if cursor < 0 || cursor > 0 && cursor < s.Cursor {
		return fmt.Sprintf("Error: memory unchanged; cursor %d cannot precede the current cursor %d", cursor, s.Cursor)
	}
	old := s.Cursor
	cursor = s.safeCursor(cursor)
	if err := s.compactLocked(cursor); err != nil {
		return fmt.Sprintf("Error: memory unchanged; cannot archive transcript: %v", err)
	}
	s.Memory, s.Cursor = memory, cursor
	view := s.contextLocked()
	b, _ := json.Marshal(view)
	return fmt.Sprintf("Cursor %d -> %d; next=%d; context_bytes=%d.", old, cursor, s.messageCount(), len(b))
}

func (s *Session) compactLocked(cursor int) error {
	if cursor <= s.archived+1 {
		return nil
	}
	if s.archive == nil {
		f, err := os.CreateTemp("", "agentlet-history-*.jsonl")
		if err != nil {
			return err
		}
		s.archive = f
	}
	if cursor > s.archiveEntries {
		stat, err := s.archive.Stat()
		if err != nil {
			return err
		}
		enc := json.NewEncoder(s.archive)
		enc.SetEscapeHTML(false)
		for i := s.archiveEntries; i < cursor; i++ {
			idx := i - s.archived
			if i == 0 {
				idx = 0
			}
			if err := enc.Encode(s.Transcript[idx]); err != nil {
				s.archive.Truncate(stat.Size())
				s.archive.Seek(stat.Size(), io.SeekStart)
				return err
			}
		}
		if err := s.archive.Sync(); err != nil {
			s.archive.Truncate(stat.Size())
			s.archive.Seek(stat.Size(), io.SeekStart)
			return err
		}
		s.archiveEntries = cursor
	}
	// Copy, rather than reslice: the old backing array otherwise retains every
	// absorbed string/image/tool result and prevents garbage collection.
	tail := make([]Message, 1, 1+len(s.Transcript)-(cursor-s.archived))
	tail[0] = s.Transcript[0]
	s.Transcript = append(tail, s.Transcript[cursor-s.archived:]...)
	s.archived = cursor - 1
	return nil
}

func (s *Session) close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.archive != nil {
		name := s.archive.Name()
		s.archive.Close()
		os.Remove(name)
		s.archive = nil
	}
}

// transcriptValidator rejects sessions that would send orphaned/ambiguous
// tool messages to a provider. Result order within a tool group may differ.
type transcriptValidator struct {
	count   int
	pending map[string]string
}

func (v *transcriptValidator) add(m Message) error {
	if v.count == 0 && m.Role != "system" {
		return errors.New("transcript must begin with a system message")
	}
	if len(v.pending) > 0 && m.Role != "tool" {
		return errors.New("assistant tool calls lack matching results")
	}
	if m.Role != "assistant" && len(m.ToolCalls) > 0 || m.Role != "tool" && m.ToolCallID != "" {
		return errors.New("tool metadata attached to an invalid message role")
	}
	if err := validateMessageContent(m); err != nil {
		return err
	}
	switch m.Role {
	case "system", "user":
	case "assistant":
		for _, call := range m.ToolCalls {
			args := strings.TrimSpace(call.Function.Arguments)
			if call.ID == "" || call.Function.Name == "" || call.Type != "function" || len(args) == 0 || args[0] != '{' || !json.Valid([]byte(args)) {
				return errors.New("invalid assistant tool call")
			}
			if v.pending == nil {
				v.pending = make(map[string]string)
			}
			if _, exists := v.pending[call.ID]; exists {
				return errors.New("duplicate tool call ID")
			}
			v.pending[call.ID] = call.Function.Name
		}
	case "tool":
		name, exists := v.pending[m.ToolCallID]
		if !exists || m.Name != "" && m.Name != name {
			return errors.New("orphaned or mismatched tool result")
		}
		delete(v.pending, m.ToolCallID)
	default:
		return fmt.Errorf("invalid message role %q", m.Role)
	}
	v.count++
	return nil
}

func validateMessageContent(m Message) error {
	if _, ok := m.Content.(string); ok {
		return nil
	}
	if m.Role == "assistant" && m.Content == nil && len(m.ToolCalls) > 0 {
		return nil
	}
	if m.Role == "user" {
		switch blocks := m.Content.(type) {
		case []ContentBlock:
			if len(blocks) > 0 {
				for _, block := range blocks {
					if block.Type == "text" && block.Text != "" && block.ImageURL == nil {
						continue
					}
					if block.Type != "image_url" || block.ImageURL == nil || block.ImageURL.URL == "" || block.Text != "" {
						return errors.New("invalid user content block")
					}
				}
				return nil
			}
		case []interface{}:
			if len(blocks) > 0 {
				for _, raw := range blocks {
					block, ok := raw.(map[string]interface{})
					if !ok {
						return errors.New("invalid user content block")
					}
					if block["type"] == "text" {
						if _, ok := block["text"].(string); ok {
							continue
						}
					}
					if block["type"] == "image_url" {
						image, ok := block["image_url"].(map[string]interface{})
						if ok {
							if url, ok := image["url"].(string); ok && url != "" {
								continue
							}
						}
					}
					return errors.New("invalid user content block")
				}
				return nil
			}
		}
	}
	return fmt.Errorf("invalid content for %s message", m.Role)
}

func (v *transcriptValidator) finish() error {
	if v.count == 0 {
		return errors.New("empty transcript")
	}
	if len(v.pending) > 0 {
		return errors.New("assistant tool calls lack matching results")
	}
	return nil
}

// Private snapshots avoid symlink-following and user .tmp clobbering. Sync
// before replacement; Unix rename is atomic, Windows follows its OS guarantees.
func saveSession(path string, s *Session) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cursor := s.Cursor
	if cursor < 1 {
		cursor = 1
	}
	cursor = s.safeCursor(cursor)
	if cursor > 1 && strings.TrimSpace(s.Memory) == "" {
		return errors.New("compacted session requires semantic memory")
	}
	owner := s.owner
	if !owner.matches(path) {
		var err error
		owner, err = reserveSession(path, true)
		if err != nil {
			return err
		}
		defer owner.close()
	}
	f, err := createSessionTemp(path)
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			f.Close()
			os.Remove(f.Name())
		}
	}()
	w := bufio.NewWriter(f)
	fmt.Fprintf(w, `{"version":%d,"transcript":[`, sessionVersion)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	v := transcriptValidator{}
	write := func(m Message) error {
		if err := v.add(m); err != nil {
			return err
		}
		if v.count > 1 {
			if _, err := w.WriteString(","); err != nil {
				return err
			}
		}
		return enc.Encode(m)
	}
	start := 0
	if s.archived > 0 {
		if s.archive == nil {
			return errors.New("session archive is closed")
		}
		stat, err := s.archive.Stat()
		if err != nil {
			return err
		}
		dec := json.NewDecoder(io.NewSectionReader(s.archive, 0, stat.Size()))
		for i := 0; i <= s.archived; i++ {
			var m Message
			if err := dec.Decode(&m); err != nil {
				return fmt.Errorf("read session archive: %w", err)
			}
			if err := write(m); err != nil {
				return err
			}
		}
		start = 1
	}
	for _, m := range s.Transcript[start:] {
		if err := write(m); err != nil {
			return err
		}
	}
	if err := v.finish(); err != nil {
		return err
	}
	memory, err := json.Marshal(s.Memory)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, `],"memory":%s,"cursor":%d}`, memory, cursor); err != nil {
		return err
	}
	if err := w.Flush(); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := commitSession(path, f, owner); err != nil {
		return err
	}
	committed = true
	s.Version = sessionVersion
	if runtime.GOOS != "windows" {
		dir, err := os.Open(filepath.Dir(path))
		if err != nil {
			return err
		}
		defer dir.Close()
		if err := dir.Sync(); err != nil && !errors.Is(err, syscall.EINVAL) && !errors.Is(err, syscall.ENOTSUP) {
			return err
		}
	}
	return nil
}

// Decode message-by-message to a private spool, then retain only the active
// suffix. Field order in old v3 files does not require loading their history.
func loadSession(path string) (_ *Session, retErr error) {
	stat, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !stat.Mode().IsRegular() {
		return nil, errors.New("session must be a regular file")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return decodeSession(f)
}

func loadReservedSession(path string, owner *sessionOwner) (*Session, error) {
	if !owner.matches(path) {
		return loadSession(path)
	}
	if _, err := owner.file.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	return decodeSession(owner.file)
}

func decodeSession(f *os.File) (_ *Session, retErr error) {
	stat, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !stat.Mode().IsRegular() {
		return nil, errors.New("session must be a regular file")
	}
	d := json.NewDecoder(f)
	token, err := d.Token()
	if err == io.EOF {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("invalid session JSON: %w", err)
	}
	s := &Session{}
	defer func() {
		if retErr != nil {
			s.close()
		}
	}()
	v := transcriptValidator{}
	readTranscript := func() error {
		archive, err := os.CreateTemp("", "agentlet-history-*.jsonl")
		if err != nil {
			return err
		}
		s.archive = archive
		enc := json.NewEncoder(archive)
		enc.SetEscapeHTML(false)
		for d.More() {
			var m Message
			if err := d.Decode(&m); err != nil {
				return err
			}
			if err := v.add(m); err != nil {
				return err
			}
			if err := enc.Encode(m); err != nil {
				return err
			}
		}
		end, err := d.Token()
		if err != nil || end != json.Delim(']') {
			return errors.New("invalid transcript array")
		}
		return v.finish()
	}
	switch token {
	case json.Delim('['): // Legacy flat-array sessions retain their custom anchor.
		err = readTranscript()
		s.Cursor = 1
	case json.Delim('{'):
		seen := make(map[string]bool)
		for d.More() && err == nil {
			key, e := d.Token()
			if e != nil {
				err = e
				break
			}
			name, ok := key.(string)
			known := name == "version" || name == "memory" || name == "cursor" || name == "transcript"
			if !ok || known && seen[name] {
				err = errors.New("invalid or duplicate session field")
				break
			}
			if known {
				seen[name] = true
			}
			switch name {
			case "version":
				err = d.Decode(&s.Version)
			case "memory":
				err = d.Decode(&s.Memory)
			case "cursor":
				err = d.Decode(&s.Cursor)
			case "transcript":
				var start json.Token
				start, err = d.Token()
				if err == nil && start != json.Delim('[') {
					err = errors.New("transcript must be an array")
				}
				if err == nil {
					err = readTranscript()
				}
			default:
				err = skipJSONValue(d)
			}
		}
		if err == nil {
			_, err = d.Token()
		}
		if err == nil && !seen["transcript"] {
			err = errors.New("missing transcript")
		}
	default:
		err = errors.New("session must be an object or legacy array")
	}
	if err == nil {
		if _, e := d.Token(); e != io.EOF {
			err = errors.New("trailing data after session")
		}
	}
	if err != nil {
		return nil, fmt.Errorf("invalid session JSON: %w", err)
	}
	if s.Version < 0 || s.Version > sessionVersion {
		return nil, fmt.Errorf("unsupported session version %d", s.Version)
	}
	s.archiveEntries = v.count
	if s.Cursor < 1 {
		s.Cursor = 1
	}
	if s.Cursor > v.count {
		s.Cursor = v.count
	}
	stat, err = s.archive.Stat()
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(io.NewSectionReader(s.archive, 0, stat.Size()))
	for i := 0; i < v.count; i++ {
		var m Message
		if err := dec.Decode(&m); err != nil {
			return nil, fmt.Errorf("read session archive: %w", err)
		}
		if i == s.Cursor && m.Role == "tool" {
			s.Cursor++ // move past the whole group, never expose an orphan result
		}
		if i == 0 || i >= s.Cursor {
			s.Transcript = append(s.Transcript, m)
		}
	}
	s.archived = s.Cursor - 1
	if s.Cursor > 1 && strings.TrimSpace(s.Memory) == "" {
		return nil, errors.New("compacted session requires semantic memory")
	}
	if s.archived == 0 {
		s.close()
		s.archiveEntries = 0
	}
	return s, nil
}

func skipJSONValue(d *json.Decoder) error {
	token, err := d.Token()
	if err != nil {
		return err
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	if delim != '[' && delim != '{' {
		return errors.New("invalid JSON value")
	}
	for depth := 1; depth > 0; {
		token, err := d.Token()
		if err != nil {
			return err
		}
		if delim, ok := token.(json.Delim); ok {
			switch delim {
			case '[', '{':
				depth++
			case ']', '}':
				depth--
			}
		}
	}
	return nil
}
