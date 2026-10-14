package main

// OpenAI-compatible vendor layer: wire vocabulary (the types below), transport
// (chatStream — request building, SSE parsing, tool-call accumulation) and
// provider field mapping (-reasoning-field etc.). The rest of the agent never
// touches SSE bytes or vendor field names; it consumes only the StreamEvent
// channel defined in agent.go. This file is the program's ONLY network surface:
// one POST to <api-base>/chat/completions per inference round.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

var (
	apiBase   string
	apiKey    string
	modelName string

	// Delta field names for providers deviating from the OpenAI defaults
	// (e.g. DeepSeek-R1 style reasoning streams).
	reasoningField    string
	contentField      string
	toolCallsField    string
	finishReasonField string

	extraRequestFields []map[string]json.RawMessage

	httpMaxIdleConns  int
	httpIdleTimeout   time.Duration
	streamIdleTimeout time.Duration

	httpClient *http.Client
)

type ContentBlock struct {
	Type     string    `json:"type"`
	Text     string    `json:"text,omitempty"`
	ImageURL *ImageURL `json:"image_url,omitempty"`
}

type ImageURL struct {
	URL    string `json:"url"`
	Detail string `json:"detail,omitempty"`
}

type Message struct {
	Role       string      `json:"role,omitempty"`
	Content    interface{} `json:"content,omitempty"`
	ToolCalls  []ToolCall  `json:"tool_calls,omitempty"`
	ToolCallID string      `json:"tool_call_id,omitempty"`
	Name       string      `json:"name,omitempty"`
}

type ToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function FunctionCall `json:"function"`
}

type FunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type Tool struct {
	Type     string         `json:"type"`
	Function ToolDefinition `json:"function"`
}

type ToolDefinition struct {
	Name        string                 `json:"name"`
	Description string                 `json:"description"`
	Parameters  map[string]interface{} `json:"parameters"`
}

type ChatRequest struct {
	Model    string    `json:"model"`
	Messages []Message `json:"messages"`
	Tools    []Tool    `json:"tools,omitempty"`
	Stream   bool      `json:"stream"`
}

// Keep Go's proxy, dial, TLS and HTTP/2 defaults rather than constructing a
// transport whose zero values silently remove those protections.
func newHTTPClient() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxIdleConns = httpMaxIdleConns
	transport.IdleConnTimeout = httpIdleTimeout
	return &http.Client{
		Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// The deadline applies only while awaiting network I/O, not while the caller
// handles an event. One timer is stopped and released with each request.
type streamDeadline struct {
	mu       sync.Mutex
	timer    *time.Timer
	timeout  time.Duration
	deadline time.Time
	active   bool
	closed   bool
	cancel   context.CancelCauseFunc
}

func (d *streamDeadline) arm() {
	if d.timeout <= 0 {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return
	}
	d.active = true
	d.deadline = time.Now().Add(d.timeout)
	if d.timer == nil {
		d.timer = time.AfterFunc(d.timeout, d.expire)
	} else {
		d.timer.Reset(d.timeout)
	}
}

func (d *streamDeadline) expire() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.active || d.closed {
		return
	}
	// A callback already queued before Stop/Reset may run after another read
	// began. It must honor that read's new deadline instead of canceling it.
	if remaining := time.Until(d.deadline); remaining > 0 {
		d.timer.Reset(remaining)
		return
	}
	d.active = false
	d.cancel(fmt.Errorf("stream idle timeout after %s: %w", d.timeout, context.DeadlineExceeded))
}

func (d *streamDeadline) disarm() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.active = false
	if d.timer != nil {
		d.timer.Stop()
	}
}

func (d *streamDeadline) close() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.closed, d.active = true, false
	if d.timer != nil {
		d.timer.Stop()
	}
}

type deadlineReader struct {
	io.Reader
	deadline *streamDeadline
}

func (r deadlineReader) Read(p []byte) (int, error) {
	r.deadline.arm()
	n, err := r.Reader.Read(p)
	r.deadline.disarm()
	return n, err
}

// Optional reports run synchronously and receive the exact request JSON byte
// count. This measures the HTTP payload, not model tokens or context capacity.
func chatStream(parent context.Context, messages []Message, report ...func(int)) (<-chan StreamEvent, context.CancelFunc, error) {
	ctx, cancelCause := context.WithCancelCause(parent)
	deadline := &streamDeadline{timeout: streamIdleTimeout, cancel: cancelCause}
	stopped := make(chan struct{})
	var cancelOnce sync.Once
	cancel := func() {
		cancelOnce.Do(func() {
			close(stopped)
			deadline.close()
			cancelCause(context.Canceled)
		})
	}

	reqBody := ChatRequest{Model: modelName, Messages: messages, Tools: availableTools, Stream: true}
	data, err := json.Marshal(reqBody)
	if err == nil && len(extraRequestFields) > 0 {
		var raw map[string]json.RawMessage
		err = json.Unmarshal(data, &raw)
		for _, fields := range extraRequestFields {
			if err != nil {
				break
			}
			for key, value := range fields {
				switch key {
				case "model", "messages", "tools", "stream":
					err = fmt.Errorf("-extra cannot override %q", key)
				default:
					raw[key] = value
				}
				if err != nil {
					break
				}
			}
		}
		if err == nil {
			data, err = json.Marshal(raw)
		}
	}
	if err != nil {
		cancel()
		return nil, cancel, fmt.Errorf("marshal request failed: %w", err)
	}
	for _, reportSize := range report {
		if reportSize != nil {
			reportSize(len(data))
		}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(apiBase, "/")+"/chat/completions", bytes.NewReader(data))
	if err != nil {
		cancel()
		return nil, cancel, fmt.Errorf("create request failed: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}

	deadline.arm()
	resp, err := httpClient.Do(req)
	deadline.disarm()
	if err != nil {
		if resp != nil {
			resp.Body.Close()
		}
		if ctx.Err() != nil {
			err = context.Cause(ctx)
		}
		cancel()
		return nil, cancel, err
	}
	if resp.StatusCode != http.StatusOK {
		body, readErr := io.ReadAll(deadlineReader{Reader: resp.Body, deadline: deadline})
		resp.Body.Close()
		if ctx.Err() != nil {
			readErr = context.Cause(ctx)
		}
		cancel()
		if readErr != nil {
			return nil, cancel, fmt.Errorf("API error %d: reading error response: %w", resp.StatusCode, readErr)
		}
		return nil, cancel, fmt.Errorf("API error %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	// Backpressure keeps unread events in the HTTP connection rather than
	// retaining an arbitrary queue of model text in the agent.
	eventCh := make(chan StreamEvent)
	go func() {
		defer resp.Body.Close()
		defer close(eventCh)
		defer cancel()
		send := func(e StreamEvent) error {
			select {
			case eventCh <- e:
				return nil
			case <-ctx.Done():
				return context.Cause(ctx)
			}
		}
		err := consumeChatStream(deadlineReader{Reader: resp.Body, deadline: deadline}, send)
		if ctx.Err() != nil {
			err = context.Cause(ctx)
		}
		if err != nil && parent.Err() == nil {
			// An internal idle cancellation still needs to reach the caller.
			// Parent cancellation lets an abandoned consumer release this goroutine.
			select {
			case eventCh <- StreamEvent{Type: EventError, Err: err}:
			case <-parent.Done():
			case <-stopped:
			}
		}
	}()
	return eventCh, cancel, nil
}

// Read full SSE events, including multiple data fields and CR/LF delimiters.
// Unlike Scanner, Reader has no hard token-size ceiling. Buffers are scoped to
// an individual line/event; a completed response cannot retain them.
func readSSE(r io.Reader, handle func(string) (bool, error)) error {
	reader := bufio.NewReader(r)
	var data strings.Builder
	var line strings.Builder
	hasData := false
	firstLine := true
	skipLF := false
	processLine := func() (bool, error) {
		value := line.String()
		line.Reset()
		if firstLine {
			value = strings.TrimPrefix(value, "\uFEFF")
			firstLine = false
		}
		if value == "" {
			if !hasData {
				return false, nil
			}
			payload := data.String()
			data.Reset()
			hasData = false
			return handle(payload)
		}
		field, value, found := strings.Cut(value, ":")
		if !found {
			value = ""
		}
		if field == "data" {
			if hasData {
				data.WriteByte('\n')
			}
			data.WriteString(strings.TrimPrefix(value, " "))
			hasData = true
		}
		return false, nil
	}
	for {
		b, err := reader.ReadByte()
		if err != nil {
			if !errors.Is(err, io.EOF) {
				return fmt.Errorf("stream read error: %w", err)
			}
			// EOF is not a completion marker. Do not dispatch an unterminated
			// event or execute tool calls from a severed response.
			return fmt.Errorf("stream ended before completion: %w", io.ErrUnexpectedEOF)
		}
		if skipLF {
			skipLF = false
			if b == '\n' {
				continue
			}
		}
		if b != '\n' && b != '\r' {
			line.WriteByte(b)
			continue
		}
		skipLF = b == '\r'
		done, err := processLine()
		if err != nil || done {
			return err
		}
	}
}

type streamedTool struct {
	id, kind string
	name     strings.Builder
	args     strings.Builder
}

func consumeChatStream(r io.Reader, send func(StreamEvent) error) error {
	tools := make(map[int]*streamedTool)
	finishTools := func() error {
		indices := make([]int, 0, len(tools))
		for index := range tools {
			indices = append(indices, index)
		}
		sort.Ints(indices)
		calls := make([]ToolCall, 0, len(indices))
		ids := make(map[string]bool)
		for _, index := range indices {
			tool := tools[index]
			if tool.id == "" || tool.name.Len() == 0 {
				return fmt.Errorf("incomplete tool call at index %d: missing ID or function name", index)
			}
			if ids[tool.id] {
				return fmt.Errorf("duplicate tool call ID %q", tool.id)
			}
			ids[tool.id] = true
			if tool.kind != "" && tool.kind != "function" {
				return fmt.Errorf("unsupported tool call type %q", tool.kind)
			}
			args := tool.args.String()
			var object map[string]json.RawMessage
			if err := json.Unmarshal([]byte(args), &object); err != nil || object == nil {
				return fmt.Errorf("tool call %q has incomplete or non-object JSON arguments", tool.id)
			}
			calls = append(calls, ToolCall{ID: tool.id, Type: "function",
				Function: FunctionCall{Name: tool.name.String(), Arguments: args}})
		}
		return send(StreamEvent{Type: EventToolCalls, ToolCalls: calls})
	}
	return readSSE(r, func(payload string) (bool, error) {
		if strings.TrimSpace(payload) == "[DONE]" {
			if len(tools) != 0 {
				return true, finishTools()
			}
			return true, nil
		}
		var envelope struct {
			Choices []json.RawMessage `json:"choices"`
			Error   json.RawMessage   `json:"error"`
		}
		if err := json.Unmarshal([]byte(payload), &envelope); err != nil {
			return false, fmt.Errorf("invalid stream JSON: %w", err)
		}
		if len(envelope.Error) > 0 && string(envelope.Error) != "null" {
			return false, fmt.Errorf("API stream error: %s", envelope.Error)
		}
		if envelope.Choices == nil {
			return false, errors.New("stream event is missing choices")
		}
		for _, rawChoice := range envelope.Choices {
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(rawChoice, &fields); err != nil || fields == nil {
				return false, errors.New("invalid stream choice object")
			}
			var index int
			if value := fields["index"]; len(value) > 0 {
				if err := json.Unmarshal(value, &index); err != nil {
					return false, fmt.Errorf("invalid stream choice index: %w", err)
				}
			}
			if index != 0 {
				continue
			}
			var delta map[string]json.RawMessage
			if value := fields["delta"]; len(value) > 0 && string(value) != "null" {
				if err := json.Unmarshal(value, &delta); err != nil || delta == nil {
					return false, errors.New("invalid stream delta object")
				}
			}
			for _, field := range []struct {
				name string
				kind StreamEventType
			}{{reasoningField, EventReasoning}, {contentField, EventToken}} {
				if field.name == "" {
					continue
				}
				if value := delta[field.name]; len(value) > 0 && string(value) != "null" {
					var text string
					if err := json.Unmarshal(value, &text); err != nil {
						return false, fmt.Errorf("invalid stream %s: %w", field.name, err)
					}
					if text != "" {
						if err := send(StreamEvent{Type: field.kind, Text: text}); err != nil {
							return false, err
						}
					}
				}
			}
			if value := delta[toolCallsField]; len(value) > 0 && string(value) != "null" {
				var fragments []struct {
					Index    *int   `json:"index"`
					ID       string `json:"id"`
					Type     string `json:"type"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				}
				if err := json.Unmarshal(value, &fragments); err != nil {
					return false, fmt.Errorf("invalid tool call delta: %w", err)
				}
				for _, fragment := range fragments {
					if fragment.Index == nil || *fragment.Index < 0 {
						return false, errors.New("tool call delta requires a nonnegative integer index")
					}
					index := *fragment.Index
					tool := tools[index]
					if tool == nil {
						tool = &streamedTool{}
						tools[index] = tool
					}
					if fragment.ID != "" {
						if tool.id != "" && tool.id != fragment.ID {
							return false, fmt.Errorf("tool call ID changed at index %d", index)
						}
						tool.id = fragment.ID
					}
					if fragment.Type != "" {
						if tool.kind != "" && tool.kind != fragment.Type {
							return false, fmt.Errorf("tool call type changed at index %d", index)
						}
						tool.kind = fragment.Type
					}
					tool.name.WriteString(fragment.Function.Name)
					tool.args.WriteString(fragment.Function.Arguments)
				}
			}
			if value := fields[finishReasonField]; len(value) > 0 && string(value) != "null" {
				var finish string
				if err := json.Unmarshal(value, &finish); err != nil {
					return false, fmt.Errorf("invalid finish reason: %w", err)
				}
				switch finish {
				case "":
				case "stop":
					if len(tools) > 0 {
						return false, errors.New("stream stopped without completing tool calls")
					}
					return true, nil
				case "tool_calls":
					if len(tools) == 0 {
						return false, errors.New("stream finished tool calls without any calls")
					}
					return true, finishTools()
				default:
					return false, fmt.Errorf("stream did not complete: finish_reason=%q", finish)
				}
			}
		}
		return false, nil
	})
}
