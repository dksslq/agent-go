package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"
)

func streamFailure(t *testing.T, events []StreamEvent) error {
	t.Helper()
	for _, event := range events {
		if event.Type == EventToolCalls {
			t.Fatalf("failed response emitted executable tool calls: %+v", event.ToolCalls)
		}
	}
	if len(events) == 0 || events[len(events)-1].Type != EventError || events[len(events)-1].Err == nil {
		t.Fatalf("expected terminal stream error, got %+v", events)
	}
	return events[len(events)-1].Err
}

func TestStreamRejectsMalformedAndIncompleteResponses(t *testing.T) {
	withStreamDefaults(t)
	for _, tc := range []struct {
		name, wire, want string
	}{
		{"invalid JSON", sseData("{invalid") + sseData("[DONE]"), "invalid stream JSON"},
		{"in-band error", sseData(`{"error":{"message":"quota exhausted"}}`), "quota exhausted"},
		{"missing choices", sseData(`{"unexpected":true}`), "missing choices"},
		{"truncated text", sseData(`{"choices":[{"delta":{"content":"partial"}}]}`), "before completion"},
		{"unterminated event", `data: {"choices":[{"delta":{"content":"lost"}}]}`, "before completion"},
		{"length finish", sseData(`{"choices":[{"delta":{"content":"partial"},"finish_reason":"length"}]}`), "length"},
		{"filtered finish", sseData(`{"choices":[{"delta":{},"finish_reason":"content_filter"}]}`), "content_filter"},
		{"invalid index", sseData(`{"choices":[{"delta":{"tool_calls":[{"index":-1,"id":"a","function":{"name":"exec","arguments":"{}"}}]}}]}`), "nonnegative integer index"},
		{"missing index", sseData(`{"choices":[{"delta":{"tool_calls":[{"id":"a","function":{"name":"exec","arguments":"{}"}}]}}]}`), "nonnegative integer index"},
		{"fractional index", sseData(`{"choices":[{"delta":{"tool_calls":[{"index":0.5}]}}]}`), "invalid tool call delta"},
		{"missing tool ID", toolCallChunk(t, 0, "", "exec", `{}`) + sseData("[DONE]"), "missing ID"},
		{"missing function", toolCallChunk(t, 0, "a", "", `{}`) + sseData("[DONE]"), "function name"},
		{"unfinished arguments", toolCallChunk(t, 0, "a", "exec", `{"program":`) + sseData("[DONE]"), "JSON arguments"},
		{"array arguments", toolCallChunk(t, 0, "a", "exec", `[]`) + sseData("[DONE]"), "non-object"},
		{"null arguments", toolCallChunk(t, 0, "a", "exec", `null`) + sseData("[DONE]"), "non-object"},
		{"changed ID", toolCallChunk(t, 0, "a", "exec", `{}`) + toolCallChunk(t, 0, "b", "", "") + sseData("[DONE]"), "ID changed"},
		{"duplicate ID", toolCallChunk(t, 0, "a", "exec", `{}`) + toolCallChunk(t, 1, "a", "exec", `{}`) + sseData("[DONE]"), "duplicate tool call ID"},
		{"stop with tools", toolCallChunk(t, 0, "a", "exec", `{}`) + deltaChunk(t, nil, "stop"), "without completing tool calls"},
		{"finish without tools", deltaChunk(t, nil, "tool_calls"), "without any calls"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withSSEServer(t, func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, tc.wire) })
			events, err := collectStream(t, nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := streamFailure(t, events); !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestStreamMultilineDataAndLineEndings(t *testing.T) {
	withStreamDefaults(t)
	for _, ending := range []string{"\n", "\r\n", "\r"} {
		t.Run(fmt.Sprintf("%q", ending), func(t *testing.T) {
			wire := strings.Join([]string{
				"\uFEFF: comment", "event: completion", `data: {"choices": [`,
				`data: {"delta": {"content":"multiline"}}]}`, "", "data: [DONE]", "", "",
			}, ending)
			withSSEServer(t, func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, wire) })
			events, err := collectStream(t, nil)
			if err != nil {
				t.Fatal(err)
			}
			assertEvents(t, events, []StreamEvent{{Type: EventToken, Text: "multiline"}})
		})
	}
}

func TestStreamLargeFrameAndSparseOrderedToolIndices(t *testing.T) {
	withStreamDefaults(t)
	large := strings.Repeat("x", 2*1024*1024)
	withSSEServer(t, func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, deltaChunk(t, map[string]interface{}{contentField: large}, "")+
			toolCallChunk(t, 900000000, "b", "ex", "{")+
			toolCallChunk(t, 3, "a", "exec", `{}`)+
			toolCallChunk(t, 900000000, "", "ec", "}")+
			sseData("[DONE]"))
	})
	events, err := collectStream(t, nil)
	if err != nil {
		t.Fatal(err)
	}
	if events[0].Type != EventToken || events[0].Text != large {
		t.Fatal("large stream text was truncated")
	}
	calls := events[len(events)-1].ToolCalls
	if len(calls) != 2 || calls[0].ID != "a" || calls[1].ID != "b" || calls[1].Function.Name != "exec" || calls[1].Function.Arguments != "{}" {
		t.Fatalf("sparse tool-call accumulation: %+v", calls)
	}
}

func TestStreamSelectsChoiceZeroAndUsageChunks(t *testing.T) {
	withStreamDefaults(t)
	withSSEServer(t, func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, sseData(`{"choices":[{"index":1,"delta":{"content":"other"}},{"index":0,"delta":{"content":"chosen"}}]}`)+
			sseData(`{"choices":[],"usage":{"total_tokens":5}}`)+
			deltaChunk(t, nil, "stop"))
	})
	events, err := collectStream(t, nil)
	if err != nil {
		t.Fatal(err)
	}
	assertEvents(t, events, []StreamEvent{{Type: EventToken, Text: "chosen"}})
}

func TestStreamIdleTimeoutHeadersAndBody(t *testing.T) {
	for _, headers := range []bool{false, true} {
		t.Run(fmt.Sprintf("headers=%t", headers), func(t *testing.T) {
			withStreamDefaults(t)
			streamIdleTimeout = 40 * time.Millisecond
			closed := make(chan struct{})
			withSSEServer(t, func(w http.ResponseWriter, r *http.Request) {
				defer close(closed)
				io.Copy(io.Discard, r.Body)
				if headers {
					w.Header().Set("Content-Type", "text/event-stream")
					w.WriteHeader(http.StatusOK)
					w.(http.Flusher).Flush()
				}
				<-r.Context().Done()
			})
			events, err := collectStream(t, nil)
			if headers {
				if err != nil {
					t.Fatal(err)
				}
				err = streamFailure(t, events)
			}
			if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "stream idle timeout") {
				t.Fatalf("expected idle timeout, got %v", err)
			}
			select {
			case <-closed:
			case <-time.After(time.Second):
				t.Fatal("idle timeout did not cancel HTTP request")
			}
		})
	}
}

func TestStreamIdleTimeoutTracksBytesAndCanBeDisabled(t *testing.T) {
	for _, timeout := range []time.Duration{0, 2 * time.Second} {
		t.Run(timeout.String(), func(t *testing.T) {
			withStreamDefaults(t)
			streamIdleTimeout = timeout
			withSSEServer(t, func(w http.ResponseWriter, r *http.Request) {
				// Keep the stream alive longer than the idle timeout, with enough
				// scheduling headroom for race-instrumented CI runners.
				duration := 250 * time.Millisecond
				if timeout > 0 {
					duration = timeout + time.Second
				}
				ticker := time.NewTicker(20 * time.Millisecond)
				defer ticker.Stop()
				until := time.Now().Add(duration)
				for time.Now().Before(until) {
					io.WriteString(w, ": heartbeat\n\n")
					w.(http.Flusher).Flush()
					select {
					case <-ticker.C:
					case <-r.Context().Done():
						return
					}
				}
				io.WriteString(w, deltaChunk(t, map[string]interface{}{contentField: "alive"}, "")+sseData("[DONE]"))
			})
			events, err := collectStream(t, nil)
			if err != nil {
				t.Fatal(err)
			}
			assertEvents(t, events, []StreamEvent{{Type: EventToken, Text: "alive"}})
		})
	}
}

type streamRoundTripper func(*http.Request) (*http.Response, error)

func (f streamRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type trackedStreamBody struct {
	io.Reader
	once   sync.Once
	closed chan struct{}
}

func (b *trackedStreamBody) Close() error {
	b.once.Do(func() { close(b.closed) })
	return nil
}

func TestStreamCancellationReleasesBlockedConsumerAndBody(t *testing.T) {
	withStreamDefaults(t)
	body := &trackedStreamBody{Reader: strings.NewReader(strings.Repeat(
		deltaChunk(t, map[string]interface{}{contentField: "token"}, ""), 100)), closed: make(chan struct{})}
	httpClient = &http.Client{Transport: streamRoundTripper(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: body}, nil
	})}
	ch, cancel, err := chatStream(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	// Ensure the parser reached an event send, then abandon its next event.
	<-ch
	cancel()
	select {
	case <-body.closed:
	case <-time.After(time.Second):
		t.Fatal("cancel left the parser blocked on an abandoned event channel")
	}
}

func TestStreamSlowConsumerDoesNotTripNetworkIdleTimeout(t *testing.T) {
	withStreamDefaults(t)
	streamIdleTimeout = 30 * time.Millisecond
	withSSEServer(t, func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, deltaChunk(t, map[string]interface{}{contentField: "one"}, "")+
			deltaChunk(t, map[string]interface{}{contentField: "two"}, "")+sseData("[DONE]"))
	})
	ch, cancel, err := chatStream(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	first := <-ch
	time.Sleep(80 * time.Millisecond)
	events := []StreamEvent{first}
	for event := range ch {
		events = append(events, event)
	}
	assertEvents(t, events, []StreamEvent{{Type: EventToken, Text: "one"}, {Type: EventToken, Text: "two"}})
}

func TestStreamCRCompletionDoesNotWaitForAnotherByte(t *testing.T) {
	withStreamDefaults(t)
	streamIdleTimeout = 100 * time.Millisecond
	withSSEServer(t, func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		io.WriteString(w, "data: [DONE]\r\r")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	})
	events, err := collectStream(t, nil)
	if err != nil {
		t.Fatal(err)
	}
	assertEvents(t, events, nil)
}

func TestStreamRejectsReservedExtraFieldsAndInvalidRequestValues(t *testing.T) {
	withStreamDefaults(t)
	for _, key := range []string{"model", "messages", "tools", "stream"} {
		extraRequestFields = []map[string]json.RawMessage{{key: json.RawMessage(`false`)}}
		if _, _, err := chatStream(context.Background(), nil); err == nil || !strings.Contains(err.Error(), "cannot override") {
			t.Fatalf("reserved extra field %q: %v", key, err)
		}
	}
	extraRequestFields = []map[string]json.RawMessage{{"temperature": json.RawMessage(`0.5`)}}
	if _, _, err := chatStream(context.Background(), []Message{{Role: "user", Content: make(chan int)}}); err == nil {
		t.Fatal("unsupported message content was silently lost when merging extras")
	}
}

func TestHTTPClientRetainsStandardTransportDefaults(t *testing.T) {
	client := newHTTPClient()
	transport := client.Transport.(*http.Transport)
	defer transport.CloseIdleConnections()
	if transport.Proxy == nil || transport.DialContext == nil || transport.TLSHandshakeTimeout == 0 || !transport.ForceAttemptHTTP2 {
		t.Fatal("HTTP transport discarded standard proxy, dial, TLS or HTTP/2 defaults")
	}
	if err := client.CheckRedirect(nil, nil); !errors.Is(err, http.ErrUseLastResponse) {
		t.Fatalf("completion POST redirects must be surfaced, got %v", err)
	}
}

func TestHTTPClientUsesConfiguredProxy(t *testing.T) {
	withStreamDefaults(t)
	if os.Getenv("AGENT_STREAM_PROXY_TEST") == "1" {
		httpClient = newHTTPClient()
		defer httpClient.CloseIdleConnections()
		apiBase = "http://model.example.invalid/v1"
		events, err := collectStream(t, nil)
		if err != nil {
			t.Fatal(err)
		}
		assertEvents(t, events, []StreamEvent{{Type: EventToken, Text: "via proxy"}})
		return
	}
	proxy := withSSEServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Host != "model.example.invalid" || r.URL.Path != "/v1/chat/completions" {
			t.Errorf("unexpected proxy request destination: %s", r.URL)
		}
		io.WriteString(w, sseData(`{"choices":[{"delta":{"content":"via proxy"}}]}`)+sseData("[DONE]"))
	})
	// ProxyFromEnvironment caches its environment on first use, so verify the
	// real default transport in a fresh process instead of mutating that cache.
	cmd := exec.Command(os.Args[0], "-test.run=^TestHTTPClientUsesConfiguredProxy$", "-test.timeout=10s")
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		switch strings.ToUpper(key) {
		case "HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY", "AGENT_STREAM_PROXY_TEST":
		default:
			cmd.Env = append(cmd.Env, entry)
		}
	}
	cmd.Env = append(cmd.Env, "AGENT_STREAM_PROXY_TEST=1", "HTTP_PROXY="+proxy.URL)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("HTTP proxy integration: %v\n%s", err, output)
	}
}

func TestStreamBrokenHTTPBodyDoesNotExecuteTools(t *testing.T) {
	withStreamDefaults(t)
	withSSEServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "100000")
		io.WriteString(w, toolCallChunk(t, 0, "a", "exec", `{}`))
	})
	events, err := collectStream(t, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := streamFailure(t, events); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("broken HTTP body should report its read failure: %v", err)
	}
}

func TestStreamIdleTimeoutDuringHTTPErrorBody(t *testing.T) {
	withStreamDefaults(t)
	streamIdleTimeout = 40 * time.Millisecond
	withSSEServer(t, func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusTooManyRequests)
		io.WriteString(w, "partial error")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	})
	_, err := collectStream(t, nil)
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "API error 429") {
		t.Fatalf("stalled error body must time out with API status: %v", err)
	}
}

func TestStreamMarshalFailureDoesNotIssueRequest(t *testing.T) {
	withStreamDefaults(t)
	called := false
	httpClient = &http.Client{Transport: streamRoundTripper(func(*http.Request) (*http.Response, error) {
		called = true
		return nil, errors.New("unexpected request")
	})}
	extraRequestFields = []map[string]json.RawMessage{{"bad": json.RawMessage(`{`)}}
	if _, _, err := chatStream(context.Background(), nil); err == nil || called {
		t.Fatalf("failed marshal issued a request: called=%t err=%v", called, err)
	}
}

func TestStreamRequestSizeMatchesHTTPPayload(t *testing.T) {
	withStreamDefaults(t)
	extraRequestFields = []map[string]json.RawMessage{{"temperature": json.RawMessage(`0.5`), "provider_option": json.RawMessage(`"追加\n字段"`)}}
	bodyCh := make(chan []byte, 1)
	withSSEServer(t, func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
		}
		bodyCh <- body
		io.WriteString(w, deltaChunk(t, map[string]interface{}{contentField: "answer"}, "stop"))
	})
	reported, reports := 0, 0
	ch, cancel, err := chatStream(context.Background(), []Message{{Role: "user", Content: "你好\n世界"}}, func(n int) {
		reported, reports = n, reports+1
	})
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	if reports != 1 || reported == 0 {
		t.Fatalf("request size was not reported once before chatStream returned: bytes=%d reports=%d", reported, reports)
	}
	var events []StreamEvent
	for event := range ch {
		events = append(events, event)
	}
	assertEvents(t, events, []StreamEvent{{Type: EventToken, Text: "answer"}})
	body := <-bodyCh
	if reported != len(body) {
		t.Fatalf("reported %d request bytes, HTTP server read %d", reported, len(body))
	}
	var request map[string]interface{}
	if err := json.Unmarshal(body, &request); err != nil || request["temperature"] != 0.5 || request["provider_option"] != "追加\n字段" {
		t.Fatalf("request-size report omitted merged extra fields: %v", err)
	}
}
