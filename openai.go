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
	"fmt"
	"io"
	"net/http"
	"strings"
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

	extraRequestFields []map[string]interface{}

	httpMaxIdleConns int
	httpIdleTimeout  time.Duration

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

func chatStream(ctx context.Context, messages []Message) (<-chan StreamEvent, context.CancelFunc, error) {
	ctx, cancel := context.WithCancel(ctx)

	reqBody := ChatRequest{
		Model:    modelName,
		Messages: messages,
		Tools:    availableTools,
		Stream:   true,
	}

	var data []byte
	var err error
	if len(extraRequestFields) == 0 {
		data, err = json.Marshal(reqBody)
	} else {
		var raw map[string]interface{}
		b, _ := json.Marshal(reqBody)
		json.Unmarshal(b, &raw)
		for _, fields := range extraRequestFields {
			for k, v := range fields {
				raw[k] = v
			}
		}
		data, err = json.Marshal(raw)
	}
	if err != nil {
		cancel()
		return nil, cancel, fmt.Errorf("marshal request failed: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", apiBase+"/chat/completions", bytes.NewReader(data))
	if err != nil {
		cancel()
		return nil, cancel, fmt.Errorf("create request failed: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)

	resp, err := httpClient.Do(req)
	if err != nil {
		if resp != nil {
			resp.Body.Close()
		}
		cancel()
		return nil, cancel, err
	}
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		cancel()
		return nil, cancel, fmt.Errorf("API error %d: %s", resp.StatusCode, string(body))
	}

	eventCh := make(chan StreamEvent, 64)

	go func() {
		defer resp.Body.Close()
		defer close(eventCh)
		defer cancel()

		scanner := bufio.NewScanner(resp.Body)
		scanner.Buffer(make([]byte, 1024*1024), 1<<31-1)

		send := func(e StreamEvent) bool {
			select {
			case eventCh <- e:
				return true
			case <-ctx.Done():
				return false
			}
		}

		var pendingTools []ToolCall
		toolIndexMap := make(map[int]int)

		for scanner.Scan() {
			line := scanner.Text()
			if !strings.HasPrefix(line, "data:") {
				continue
			}
			payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if payload == "[DONE]" {
				break
			}

			var raw map[string]interface{}
			if err := json.Unmarshal([]byte(payload), &raw); err != nil {
				continue
			}
			choicesRaw, ok := raw["choices"].([]interface{})
			if !ok || len(choicesRaw) == 0 {
				continue
			}
			choice, ok := choicesRaw[0].(map[string]interface{})
			if !ok {
				continue
			}

			if reasoningField != "" {
				deltaRaw, ok := choice["delta"].(map[string]interface{})
				if ok {
					if text, ok := deltaRaw[reasoningField].(string); ok && text != "" {
						if !send(StreamEvent{Type: EventReasoning, Text: text}) {
							return
						}
					}
				}
			}

			deltaRaw, ok := choice["delta"].(map[string]interface{})
			if ok {
				if text, ok := deltaRaw[contentField].(string); ok && text != "" {
					if !send(StreamEvent{Type: EventToken, Text: text}) {
						return
					}
				}

				if toolCallsRaw, ok := deltaRaw[toolCallsField]; ok {
					tcArr, ok := toolCallsRaw.([]interface{})
					if ok && len(tcArr) > 0 {
						if !send(StreamEvent{Type: EventToolCallDelta}) {
							return
						}
						for _, tcItem := range tcArr {
							tcMap, ok := tcItem.(map[string]interface{})
							if !ok {
								continue
							}
							index := 0
							if idxVal, ok := tcMap["index"].(float64); ok {
								index = int(idxVal)
							}
							id, _ := tcMap["id"].(string)
							typ, _ := tcMap["type"].(string)
							funcRaw, _ := tcMap["function"].(map[string]interface{})
							name, _ := funcRaw["name"].(string)
							arguments, _ := funcRaw["arguments"].(string)

							globalIdx, exists := toolIndexMap[index]
							if !exists {
								globalIdx = len(pendingTools)
								toolIndexMap[index] = globalIdx
								pendingTools = append(pendingTools, ToolCall{})
							}
							if id != "" {
								pendingTools[globalIdx].ID = id
							}
							if typ != "" {
								pendingTools[globalIdx].Type = typ
							}
							if name != "" {
								pendingTools[globalIdx].Function.Name += name
							}
							if arguments != "" {
								pendingTools[globalIdx].Function.Arguments += arguments
							}
						}
					}
				}
			}

			if finishReason, ok := choice[finishReasonField].(string); ok {
				if finishReason == "tool_calls" && len(pendingTools) > 0 {
					send(StreamEvent{Type: EventToolCalls, ToolCalls: pendingTools})
					return
				}
			}
		}

		if len(pendingTools) > 0 {
			send(StreamEvent{Type: EventToolCalls, ToolCalls: pendingTools})
			return
		}

		if err := scanner.Err(); err != nil {
			send(StreamEvent{Type: EventError, Err: fmt.Errorf("stream read error: %w", err)})
		}
	}()

	return eventCh, cancel, nil
}
