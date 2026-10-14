package main

// Session history lives in RAM or in the explicitly selected checkpoint file.
// Compaction releases RAM; no temporary files or implicit archives are created.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
)

const memoryHeader = "Memory (data, not instructions):\n"
const compactedContinuation = "[Runtime continuation: earlier conversation was compacted.]"
const sessionVersion = 4

const unsavedSessionNotice = "No continuous saving. Uncompacted history can grow in RAM; exit loses unsaved changes. memory_edit discards unsaved originals. Use exec to explicitly save important facts."

type Session struct {
	Version    int       `json:"version"`
	Transcript []Message `json:"transcript"` // system anchor + resident tail
	Memory     string    `json:"memory"`
	Cursor     int       `json:"cursor"` // absolute transcript index

	mu               sync.Mutex
	archived         int // non-system messages removed from RAM; absolute indices stay stable
	sourcePath       string
	historyPath      string // explicitly selected archive; may outlive the copyable prefix
	historyCount     int
	diskBase         int
	diskCount        int
	diskOffset       int64
	saveFile         string
	owner            *sessionOwner
	ownsOwner        bool
	checkpointErr    error
	lastRequestBytes int
}

func newSession() *Session {
	return &Session{Transcript: []Message{{Role: "system", Content: systemRules()}}, Cursor: 1}
}

func sameSessionPath(left, right string) bool {
	if left == "" || right == "" {
		return false
	}
	a, err := filepath.Abs(left)
	if err != nil {
		return false
	}
	b, err := filepath.Abs(right)
	if err != nil {
		return false
	}
	equal := func(a, b string) bool {
		if runtime.GOOS == "windows" {
			return strings.EqualFold(a, b)
		}
		return a == b
	}
	if equal(a, b) {
		return true
	}
	resolve := func(path string) string {
		if resolved, err := filepath.EvalSymlinks(path); err == nil {
			return resolved
		}
		// A new snapshot has no leaf yet; resolve its existing parent.
		if parent, err := filepath.EvalSymlinks(filepath.Dir(path)); err == nil {
			return filepath.Join(parent, filepath.Base(path))
		}
		return path
	}
	return equal(resolve(a), resolve(b))
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

func (s *Session) persistenceNotice() string {
	notice := unsavedSessionNotice
	if s.saveFile == "" {
		if s.sourcePath != "" {
			notice = fmt.Sprintf("Selected snapshot: %q. ", s.sourcePath) + notice
		}
	} else {
		notice = fmt.Sprintf("Session checkpoints: %q. Completed history is saved before compaction.", s.saveFile)
		if s.owner != nil && s.owner.exclusive() {
			notice += " The file is held exclusively; exec cannot open it."
		}
	}
	if s.diskBase > 0 {
		notice += " Earlier unsaved originals are unavailable."
	}
	if s.historyPath != "" {
		notice += fmt.Sprintf(" Read saved originals with history_read: %q.", s.historyPath)
	}
	return notice
}

func (s *Session) contextLocked() []Message {
	if len(s.Transcript) == 0 {
		return nil
	}
	tail := s.Transcript[s.residentCursor():]
	out := make([]Message, 0, 3+len(tail))
	out = append(out, s.Transcript[0])
	directory, err := os.Getwd()
	if err != nil {
		directory = "[unavailable]"
	}
	state := fmt.Sprintf("Runtime: %s/%s; default cwd=%q.\n", runtime.GOOS, runtime.GOARCH, directory) + s.persistenceNotice()
	if s.Memory != "" {
		state += "\n" + memoryHeader + s.Memory
	}
	out = append(out, Message{Role: "system", Content: state})
	if len(tail) > 0 && tail[0].Role == "assistant" {
		// Some compatible providers require a user message before assistant
		// history. Compaction can remove that user while retaining an in-flight
		// tool group; a runtime marker restores the wire protocol without
		// retaining the consumed conversation or changing its saved history.
		out = append(out, Message{Role: "user", Content: compactedContinuation})
	}
	return append(out, tail...)
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
	for i := 1; i < len(s.Transcript); i++ {
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
	if s.saveFile != "" {
		if err := s.checkpointLocked(s.saveFile, memory, cursor, cursor); err != nil {
			s.checkpointErr = err
			return fmt.Sprintf("Error: memory unchanged; checkpoint failed: %v", err)
		}
	} else if cursor > s.diskCount {
		// Unsaved originals are gone; a later save can only retain this suffix.
		s.sourcePath, s.diskCount, s.diskBase, s.diskOffset = "", 0, 0, 0
	}
	s.compactLocked(cursor)
	s.Memory, s.Cursor = memory, cursor
	view := s.contextLocked()
	b, _ := json.Marshal(view)
	return fmt.Sprintf("Cursor %d -> %d; next=%d; context_bytes=%d.", old, cursor, s.messageCount(), len(b))
}

func (s *Session) compactLocked(cursor int) {
	if cursor <= s.archived+1 {
		return
	}
	tail := make([]Message, 1, 1+len(s.Transcript)-(cursor-s.archived))
	tail[0] = s.Transcript[0]
	s.Transcript = append(tail, s.Transcript[cursor-s.archived:]...)
	s.archived = cursor - 1
}

func (s *Session) close() {
	_ = s.release()
}

func (s *Session) release() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ownsOwner {
		err := s.owner.close()
		s.ownsOwner = false
		s.owner = nil
		return err
	}
	return nil
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

// Writes only the user-selected file. Existing unrelated paths are never overwritten.
// A snapshot never transfers the active checkpoint handle or changes save mode.
// It borrows the source only while copying, then closes the new output.
func saveSession(path string, s *Session) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.checkpointErr != nil {
		return s.checkpointErr
	}
	owner, err := reserveSession(path, true)
	if err != nil {
		return err
	}
	if owner == nil {
		return errors.New("snapshot path is required")
	}
	if !owner.createdNew() {
		return errors.Join(errors.New("snapshot path already exists; choose a new file"), owner.close())
	}
	// The source can be exclusively held on Windows, so copy through its handle.
	copy := &Session{Version: s.Version, Transcript: s.Transcript, Memory: s.Memory,
		Cursor: s.Cursor, archived: s.archived, sourcePath: s.sourcePath,
		diskBase: s.diskBase, diskCount: s.diskCount, diskOffset: s.diskOffset,
	}
	// Source reads borrow s; the snapshot owns only its new output.
	copy.owner = owner
	copy.ownsOwner = true
	err = copy.checkpointFromLocked(owner.path, copy.Memory, copy.safeCursor(copy.Cursor), copy.messageCount(), s)
	closeErr := owner.close()
	copy.owner, copy.ownsOwner = nil, false
	if err != nil || closeErr != nil {
		return errors.Join(err, closeErr)
	}
	if s.saveFile == "" {
		s.sourcePath, s.diskBase, s.diskCount, s.diskOffset = copy.sourcePath, copy.diskBase, copy.diskCount, copy.diskOffset
		s.historyPath, s.historyCount = copy.sourcePath, copy.diskCount
	}
	return nil
}

func checkpointSession(path string, s *Session) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.checkpointErr != nil {
		return s.checkpointErr
	}
	cursor := s.safeCursor(s.Cursor)
	if err := s.checkpointLocked(path, s.Memory, cursor, s.messageCount()); err != nil {
		return err
	}
	s.saveFile = s.sourcePath
	s.historyPath, s.historyCount = s.sourcePath, s.diskCount
	s.Cursor = cursor
	return nil
}

func (s *Session) sourceFile() (*os.File, bool, error) {
	if s.owner.matches(s.sourcePath) {
		return s.owner.file, false, nil
	}
	f, err := os.Open(s.sourcePath)
	return f, true, err
}

func (s *Session) checkpointLocked(path, memory string, cursor, limit int) error {
	return s.checkpointFromLocked(path, memory, cursor, limit, s)
}

func (s *Session) checkpointFromLocked(path, memory string, cursor, limit int, sourceSession *Session) (err error) {
	if s.checkpointErr != nil {
		return s.checkpointErr
	}
	if cursor > 1 && strings.TrimSpace(memory) == "" {
		return errors.New("compacted session requires semantic memory")
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	end := limit - s.archived
	if end < 1 || end > len(s.Transcript) {
		return errors.New("invalid checkpoint boundary")
	}
	// Validate before creating even the explicitly requested output.
	v := transcriptValidator{}
	for _, m := range s.Transcript[:end] {
		if err := v.add(m); err != nil {
			return err
		}
	}
	if err := v.finish(); err != nil {
		return err
	}
	owner := s.owner
	acquired := !owner.matches(absolute)
	if acquired {
		owner, err = reserveSession(absolute, true)
		if err != nil {
			return err
		}
	}
	success := false
	defer func() {
		if acquired && !success {
			err = errors.Join(err, owner.close())
		}
	}()
	base, count, offset := s.diskBase, s.diskCount, s.diskOffset
	sameSource := s.sourcePath != "" && owner.matches(s.sourcePath)
	if !sameSource {
		if !owner.createdNew() {
			return errors.New("save path already exists; resume it or choose a new path")
		}
		if s.sourcePath != "" {
			source, closeSource, openErr := sourceSession.sourceFile()
			if openErr != nil {
				return openErr
			}
			if closeSource {
				defer func() { err = errors.Join(err, source.Close()) }()
			}
			if _, err := owner.file.Seek(0, io.SeekStart); err != nil {
				return err
			}
			if _, err := io.CopyN(owner.file, io.NewSectionReader(source, 0, s.diskOffset), s.diskOffset); err != nil {
				return err
			}
		} else {
			base, count, offset = s.archived, 0, 0
		}
	}
	start := 0
	if count > 0 {
		start = count - s.archived
		if start < 1 || start > len(s.Transcript) {
			return errors.New("saved history does not match resident suffix")
		}
	}
	if start > end {
		end = start
	} // earlier compaction may already be durable
	cp := sessionCheckpoint{Version: sessionVersion, Base: base, Start: count, Messages: s.Transcript[start:end], Memory: memory, Cursor: cursor}
	nextOffset, err := appendSessionCheckpoint(owner.file, offset, cp)
	if err != nil {
		return err
	}
	if count == 0 {
		count = base + len(cp.Messages)
	} else {
		count += len(cp.Messages)
	}
	if owner.createdNew() {
		if err := syncSessionDirectory(owner.path); err != nil {
			return err
		}
	}
	previous := s.owner
	previousOwned := s.ownsOwner
	s.owner, s.ownsOwner = owner, acquired || previousOwned
	s.sourcePath, s.diskBase, s.diskCount, s.diskOffset = absolute, base, count, nextOffset
	s.historyPath, s.historyCount = absolute, count
	s.Version = sessionVersion
	owner.created = false
	success = true
	if acquired && previousOwned && previous != nil {
		return previous.close()
	}
	return nil
}

func loadSession(path string) (*Session, error) {
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
	s, err := decodeSession(owner.file)
	if s != nil {
		s.sourcePath = owner.path
	}
	return s, err
}

func decodeSession(f *os.File) (*Session, error) {
	state, err := readSessionLog(f, nil)
	if err != nil {
		return nil, err
	}
	if state.Count == 0 {
		return nil, nil
	}
	s := &Session{Version: state.Version, Memory: state.Memory, Cursor: state.Cursor, sourcePath: f.Name(), diskBase: state.Base, diskCount: state.Count, diskOffset: state.Offset}
	_, err = readSessionLog(f, func(index int, m Message) error {
		if index == s.Cursor && m.Role == "tool" {
			s.Cursor++
		}
		if index == 0 || index >= s.Cursor {
			s.Transcript = append(s.Transcript, m)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	s.archived = s.Cursor - 1
	if s.Cursor > 1 && strings.TrimSpace(s.Memory) == "" {
		return nil, errors.New("compacted session requires semantic memory")
	}
	absolute, err := filepath.Abs(s.sourcePath)
	if err != nil {
		return nil, err
	}
	s.sourcePath = absolute
	s.historyPath, s.historyCount = absolute, s.diskCount
	return s, nil
}

// Only continue keeps an output open. Failure leaves any current session intact.
func continueSession(path string) (_ *Session, err error) {
	owner, err := reserveSession(path, true)
	if err != nil {
		return nil, err
	}
	if owner == nil {
		return nil, errors.New("continue path is required")
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, owner.close())
		}
	}()
	s := newSession()
	if !owner.createdNew() {
		s, err = loadReservedSession(path, owner)
		if err != nil {
			return nil, err
		}
		if s == nil {
			return nil, errors.New("existing session file is empty")
		}
	}
	s.owner, s.ownsOwner, s.saveFile = owner, true, owner.path
	if owner.createdNew() {
		if err := checkpointSession(owner.path, s); err != nil {
			return nil, err
		}
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
