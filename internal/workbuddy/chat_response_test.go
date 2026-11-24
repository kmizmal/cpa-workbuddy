package workbuddy_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"cpa-workbuddy/internal/workbuddy"
)

// responseStreamHost scripts the actual host callback boundary. Each read can
// end the stream while still returning bytes, as permitted by the host API.
type responseStreamHost struct {
	mu               sync.Mutex
	chunks           [][]byte
	next             int
	emitted          [][]byte
	streamError      string
	upstreamClosed   int
	downstreamClosed bool
	callbackError    error
	closed           chan struct{}
	closeOnce        sync.Once
}

// finish closes the completion signal once both lease ends are closed, so
// callers wait for the full stream teardown.
func (h *responseStreamHost) finish() {
	if h.upstreamClosed > 0 && h.downstreamClosed {
		h.closeOnce.Do(func() { close(h.closed) })
	}
}

func (h *responseStreamHost) call(method string, raw []byte) ([]byte, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	var request struct {
		StreamID string `json:"stream_id"`
		Payload  []byte `json:"payload"`
		Error    string `json:"error"`
	}
	if err := json.Unmarshal(raw, &request); err != nil {
		return nil, err
	}
	var result any = map[string]any{}
	switch method {
	case "host.http.do_stream":
		result = map[string]any{"status_code": 200, "headers": map[string][]string{"Content-Type": {"text/event-stream"}}, "stream_id": "response-upstream"}
	case "host.http.stream_read":
		if request.StreamID != "response-upstream" {
			return nil, errors.New("wrong upstream stream ID")
		}
		if h.next < len(h.chunks) {
			result = map[string]any{"payload": h.chunks[h.next], "done": h.next == len(h.chunks)-1}
			h.next++
		} else {
			result = map[string]any{"done": true}
		}
	case "host.http.stream_close":
		if request.StreamID != "response-upstream" {
			return nil, errors.New("wrong upstream close ID")
		}
		h.upstreamClosed++
		h.finish()
	case "host.stream.emit":
		if request.StreamID != "response-downstream" {
			return nil, errors.New("wrong downstream stream ID")
		}
		h.emitted = append(h.emitted, append([]byte(nil), request.Payload...))
		if request.Error != "" {
			h.streamError = request.Error
		}
	case "host.stream.close":
		if request.StreamID != "response-downstream" {
			return nil, errors.New("wrong downstream close ID")
		}
		if request.Error != "" {
			h.streamError = request.Error
		}
		h.downstreamClosed = true
		h.finish()
	case "host.log":
	default:
		h.callbackError = fmt.Errorf("unexpected callback method %s", method)
		return nil, h.callbackError
	}
	return json.Marshal(map[string]any{"ok": true, "result": result})
}

func responseExecute(t *testing.T, chunks [][]byte, streaming bool) ([]byte, *responseStreamHost, error) {
	t.Helper()
	s := workbuddy.New()
	t.Cleanup(s.Close)
	auth := parsedIdentity(t, "workbuddy-ai", "")
	h := &responseStreamHost{chunks: chunks, closed: make(chan struct{})}
	method := "executor.execute"
	if streaming {
		method = "executor.execute_stream"
	}
	result, err := invoke(s, context.Background(), method, map[string]any{
		"AuthID": auth.ID, "AuthProvider": auth.Provider, "StorageJSON": auth.StorageJSON,
		"Model": "wb-ai/custom:model|variant", "Format": "openai", "Stream": streaming,
		"Payload":          []byte(`{"model":"wb-ai/custom:model|variant","messages":[{"role":"user","content":"hello"}]}`),
		"host_callback_id": "response-callback", "stream_id": "response-downstream",
	}, h.call)
	if streaming && err == nil {
		select {
		case <-h.closed:
		case <-time.After(5 * time.Second):
			t.Fatal("executor stream did not close")
		}
	}
	if err != nil {
		return nil, h, err
	}
	if streaming {
		return nil, h, nil
	}
	raw, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	var response struct{ Payload []byte }
	if err := json.Unmarshal(raw, &response); err != nil {
		t.Fatal(err)
	}
	return response.Payload, h, nil
}

func responseChunks(text string, width int) [][]byte {
	var chunks [][]byte
	for len(text) > width {
		chunks = append(chunks, []byte(text[:width]))
		text = text[width:]
	}
	return append(chunks, []byte(text))
}

func responseFrame(payload string) string { return "data: " + payload + "\n\n" }

const responseFirst = `{"id":"chat-visible","object":"chat.completion.chunk","created":123,"model":"custom:model|variant","system_fingerprint":"fp-visible","choices":[{"index":2,"delta":{"role":"assistant","content":"世界"},"finish_reason":null},{"index":0,"delta":{"role":"assistant","content":"Hello"},"finish_reason":null}]}`
const responseLast = `{"id":"chat-visible","object":"chat.completion.chunk","created":123,"model":"custom:model|variant","choices":[{"index":0,"delta":{"content":"!"},"finish_reason":"stop"},{"index":2,"delta":{"content":"🌍"},"finish_reason":"length"}]}`
const responseUsage = `{"id":"chat-visible","object":"chat.completion.chunk","created":123,"model":"custom:model|variant","choices":[],"usage":{"prompt_tokens":7,"completion_tokens":4,"total_tokens":11,"completion_tokens_details":{"accepted_prediction_tokens":2}}}`

func TestExecutorResponseAggregatesAllChoicesAcrossSSEBoundaries(t *testing.T) {
	for _, newline := range []string{"\n", "\r\n", "\r"} {
		for _, width := range []int{1, 7, 8192} {
			t.Run(fmt.Sprintf("newline=%q/chunk=%d", newline, width), func(t *testing.T) {
				text := ": heartbeat\n\n" + responseFrame(responseFirst) + "data: {\ndata: \"choices\":[]}\n\n" + responseFrame(`{"choices":[{"index":0,"delta":{},"finish_reason":null}]}`) + responseFrame(responseLast) + responseFrame(responseUsage) + responseFrame("[DONE]")
				text = strings.ReplaceAll(text, "\n", newline)
				payload, h, err := responseExecute(t, responseChunks(text, width), false)
				if err != nil {
					t.Fatal(err)
				}
				var got map[string]any
				if err := json.Unmarshal(payload, &got); err != nil {
					t.Fatal(err)
				}
				want := `{"id":"chat-visible","object":"chat.completion","created":123,"model":"wb-ai/custom:model|variant","system_fingerprint":"fp-visible","choices":[{"index":0,"message":{"role":"assistant","content":"Hello!"},"finish_reason":"stop"},{"index":2,"message":{"role":"assistant","content":"世界🌍"},"finish_reason":"length"}],"usage":{"prompt_tokens":7,"completion_tokens":4,"total_tokens":11,"completion_tokens_details":{"accepted_prediction_tokens":2}}}`
				var expected map[string]any
				if err := json.Unmarshal([]byte(want), &expected); err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(got, expected) {
					t.Fatalf("completion = %s", payload)
				}
				if h.upstreamClosed != 1 || h.callbackError != nil {
					t.Fatalf("upstream close count = %d; callback error = %v", h.upstreamClosed, h.callbackError)
				}
			})
		}
	}
}

func TestExecutorResponseStreamsWholeEventsInSemanticOrder(t *testing.T) {
	text := ": ignored\r\n\r\nid: event-1\r\nevent: message\r\n" + strings.ReplaceAll(responseFrame(responseFirst), "\n", "\r\n") + responseFrame(responseLast) + responseFrame(responseUsage) + responseFrame("[DONE]")
	_, h, err := responseExecute(t, responseChunks(text, 1), true)
	if err != nil {
		t.Fatal(err)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.streamError != "" || len(h.emitted) != 4 || h.upstreamClosed != 1 || h.callbackError != nil {
		t.Fatalf("stream result: events=%d, close=%d, error=%q, callback=%v", len(h.emitted), h.upstreamClosed, h.streamError, h.callbackError)
	}
	for i, expectedRaw := range []string{responseFirst, responseLast, responseUsage} {
		frame := string(h.emitted[i])
		if !strings.HasSuffix(frame, "\n\n") || strings.Count(frame, "data: ") != 1 {
			t.Fatalf("event %d is not a complete SSE frame", i)
		}
		if i == 0 && !strings.HasPrefix(frame, "id: event-1\nevent: message\n") {
			t.Fatal("SSE event metadata was lost")
		}
		_, data, _ := strings.Cut(frame, "data: ")
		var got, expected map[string]any
		if err := json.Unmarshal([]byte(data), &got); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal([]byte(expectedRaw), &expected); err != nil {
			t.Fatal(err)
		}
		expected["model"] = "wb-ai/custom:model|variant"
		if !reflect.DeepEqual(got, expected) {
			t.Fatalf("stream event %d lost JSON fields: %s", i, frame)
		}
	}
	if string(h.emitted[3]) != "data: [DONE]\n\n" {
		t.Fatal("stream lost terminal DONE event")
	}
}

func TestExecutorResponseDoesNotInventUsage(t *testing.T) {
	payload, _, err := responseExecute(t, [][]byte{[]byte(responseFrame(responseFirst) + responseFrame(responseLast) + responseFrame("[DONE]"))}, false)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(payload, &got); err != nil {
		t.Fatal(err)
	}
	if _, exists := got["usage"]; exists {
		t.Fatal("missing upstream usage was fabricated")
	}
}

func TestExecutorResponseAcceptsWorkBuddyPendingAndTerminalPlaceholders(t *testing.T) {
	const first = `{"id":"chat-placeholder","object":"chat.completion.chunk","created":123,"model":"custom:model|variant","choices":[{"index":0,"delta":{"role":"assistant","content":"Hel","reasoning_content":"","refusal":"","tool_calls":[],"function_call":null,"extra_fields":null},"finish_reason":"","logprobs":null}],"usage":null}`
	const middle = `{"id":"chat-placeholder","object":"chat.completion.chunk","created":123,"model":"custom:model|variant","choices":[{"index":0,"delta":{"content":"lo","reasoning_content":"","refusal":"","tool_calls":[],"function_call":null,"extra_fields":null},"finish_reason":"","logprobs":null}],"usage":null}`
	const last = `{"id":"chat-placeholder","object":"chat.completion.chunk","created":123,"model":"custom:model|variant","choices":[{"index":0,"delta":{"role":"assistant","content":"","reasoning_content":"","refusal":"","tool_calls":[],"function_call":{"name":"","arguments":""},"extra_fields":null},"finish_reason":"stop","logprobs":null}],"usage":{"prompt_tokens":7,"completion_tokens":2,"total_tokens":9}}`
	for _, streaming := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%v", streaming), func(t *testing.T) {
			payload, host, err := responseExecute(t, responseChunks(responseFrame(first)+responseFrame(middle)+responseFrame(last)+responseFrame("[DONE]"), 17), streaming)
			if err != nil {
				t.Fatal(err)
			}
			if streaming {
				if host.streamError != "" {
					t.Fatal(host.streamError)
				}
				if len(host.emitted) != 4 || string(host.emitted[3]) != "data: [DONE]\n\n" {
					t.Fatalf("stream did not preserve the three chunks and terminal event: %q", host.emitted)
				}
				return
			}
			var response struct {
				Choices []struct {
					Message      struct{ Content string }
					FinishReason string `json:"finish_reason"`
				}
				Usage struct {
					TotalTokens int `json:"total_tokens"`
				}
			}
			if err := json.Unmarshal(payload, &response); err != nil {
				t.Fatal(err)
			}
			if len(response.Choices) != 1 || response.Choices[0].Message.Content != "Hello" || response.Choices[0].FinishReason != "stop" || response.Usage.TotalTokens != 9 {
				t.Fatalf("unexpected aggregate: %s", payload)
			}
		})
	}
}

func TestExecutorResponseRejectsIncompleteMalformedAndUnsupportedStreams(t *testing.T) {
	cases := []struct{ name, text, code, safeMessage string }{
		{"empty", "", "upstream_stream_incomplete", ""},
		{"bare-done", responseFrame("[DONE]"), "upstream_stream_incomplete", ""},
		{"usage-only", responseFrame(responseUsage) + responseFrame("[DONE]"), "upstream_stream_incomplete", ""},
		{"unfinished", responseFrame(responseFirst) + responseFrame("[DONE]"), "upstream_stream_incomplete", ""},
		{"finished-without-done", responseFrame(responseFirst) + responseFrame(responseLast), "upstream_stream_incomplete", ""},
		{"unterminated-done", responseFrame(responseFirst) + responseFrame(responseLast) + "data: [DONE]", "upstream_stream_incomplete", ""},
		{"one-unfinished-choice", responseFrame(responseFirst) + responseFrame(`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`) + responseFrame("[DONE]"), "upstream_stream_incomplete", ""},
		{"malformed-json", responseFrame(`{"private-token":"private-stream-secret"`), "upstream_stream_malformed", ""},
		{"invalid-utf8", responseFrame("{\"choices\":[],\"x\":\"\xff\"}"), "upstream_stream_malformed", ""},
		{"invalid-choice-index", responseFrame(`{"choices":[{"index":-1,"delta":{},"finish_reason":"stop"}]}`), "upstream_stream_malformed", ""},
		{"duplicate-index", responseFrame(`{"choices":[{"index":0,"delta":{}},{"index":0,"delta":{}}]}`), "upstream_stream_malformed", ""},
		{"json-error", responseFrame(`{"error":{"message":"private-stream-secret","code":4321}}`), "upstream_stream_error", ""},
		{"error-event", "event: error\n" + responseFrame(`{"message":"private-stream-secret"}`), "upstream_stream_error", ""},
		{"version-error", responseFrame(`{"code":10085,"message":"private-stream-secret"}`), "upstream_client_version_rejected", "WorkBuddy client version rejected"},
		{"nested-version-error", "event: error\n" + responseFrame(`{"error":{"code":"10085","message":"private-stream-secret"}}`), "upstream_client_version_rejected", "WorkBuddy client version rejected"},
		{"nontext-content", responseFrame(`{"choices":[{"index":0,"delta":{"content":[{"type":"image_url"}]},"finish_reason":null}]}`), "upstream_stream_malformed", ""},
		{"pending-empty-reason", responseFrame(`{"choices":[{"index":0,"delta":{"content":"hello"},"finish_reason":""}]}`) + responseFrame("[DONE]"), "upstream_stream_incomplete", ""},
		{"invalid-reason-type", responseFrame(`{"choices":[{"index":0,"delta":{},"finish_reason":3}]}`), "upstream_stream_malformed", ""},
		{"legacy-function-call", responseFrame(`{"choices":[{"index":0,"delta":{"function_call":{"name":"lookup","arguments":"{}"}},"finish_reason":"stop"}]}`), "unsupported_upstream_response", ""},
		{"unknown-function-field", responseFrame(`{"choices":[{"index":0,"delta":{"function_call":{"name":"","arguments":"","other":""}},"finish_reason":"stop"}]}`), "unsupported_upstream_response", ""},
		{"invalid-function-placeholder", responseFrame(`{"choices":[{"index":0,"delta":{"function_call":{"name":7,"arguments":""}},"finish_reason":"stop"}]}`), "unsupported_upstream_response", ""},
	}
	for _, tc := range cases {
		for _, streaming := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%v", tc.name, streaming), func(t *testing.T) {
				_, h, err := responseExecute(t, responseChunks(tc.text, 17), streaming)
				if streaming && err == nil {
					h.mu.Lock()
					message := h.streamError
					closed := h.upstreamClosed
					emitted := append([][]byte(nil), h.emitted...)
					h.mu.Unlock()
					if message == "" || strings.Contains(message, "private-stream-secret") || closed != 1 {
						t.Fatalf("invalid streaming failure: error=%q upstream closes=%d", message, closed)
					}
					if tc.safeMessage != "" && !strings.Contains(message, tc.safeMessage) {
						t.Fatalf("missing client-version diagnosis: %q", message)
					}
					for _, frame := range emitted {
						if string(frame) == "data: [DONE]\n\n" {
							t.Fatal("failed upstream stream emitted success DONE")
						}
					}
					return
				}
				var detail *workbuddy.Error
				if !errors.As(err, &detail) || detail.Code != tc.code || strings.Contains(detail.Message, "private-stream-secret") {
					t.Fatalf("error = %v; want safe %s", err, tc.code)
				}
				if tc.safeMessage != "" && detail.Message != tc.safeMessage {
					t.Fatalf("client-version diagnosis = %q", detail.Message)
				}
			})
		}
	}
}

func TestExecutorResponseBoundsEventsAndAggregate(t *testing.T) {
	for _, text := range []string{
		"data: " + strings.Repeat("x", (1<<20)+1),
		"data: " + strings.Repeat("x", 600000) + "\ndata: " + strings.Repeat("y", 600000) + "\n\n",
	} {
		_, _, err := responseExecute(t, responseChunks(text, 4096), false)
		var detail *workbuddy.Error
		if !errors.As(err, &detail) || detail.Code != "upstream_stream_too_large" {
			t.Fatalf("oversized SSE error = %v", err)
		}
	}
	fragment := []byte(responseFrame(`{"choices":[{"index":0,"delta":{"content":"` + strings.Repeat("x", 512<<10) + `"},"finish_reason":null}]}`))
	chunks := make([][]byte, 66)
	for i := range chunks {
		chunks[i] = fragment
	}
	_, _, err := responseExecute(t, chunks, false)
	var detail *workbuddy.Error
	if !errors.As(err, &detail) || detail.Code != "upstream_stream_too_large" {
		t.Fatalf("oversized aggregate error = %v", err)
	}
}
