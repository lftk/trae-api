package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestQA006MalformedChatRequestsRejectedBeforeUpstream(t *testing.T) {
	var sent atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sent.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer upstream.Close()
	cfg := fixtureConfig()
	cfg.BaseURL = upstream.URL
	s := authenticatedServer(t, cfg)
	cases := []struct {
		name    string
		body    string
		status  int
		message string
	}{
		{
			name:    "trailing JSON value",
			body:    `{"model":"m","messages":[{"role":"user","content":"x"}]} {}`,
			status:  http.StatusBadRequest,
			message: "multiple JSON values",
		},
		{
			name:    "array at root",
			body:    `[]`,
			status:  http.StatusBadRequest,
			message: "request must be a JSON object",
		},
		{
			name:    "reasoning effort not null",
			body:    `{"model":"m","messages":[{"role":"user","content":"x"}],"reasoning_effort":"high"}`,
			status:  http.StatusBadRequest,
			message: "reasoning_effort is unsupported",
		},
		{
			name:    "n decimal is not exact one",
			body:    `{"model":"m","messages":[{"role":"user","content":"x"}],"n":1.0}`,
			status:  http.StatusBadRequest,
			message: "only n=1",
		},
		{
			name:    "stop unsupported",
			body:    `{"model":"m","messages":[{"role":"user","content":"x"}],"stop":["."]}`,
			status:  http.StatusBadRequest,
			message: "stop is unsupported",
		},
		{
			name:    "JSON response format unsupported",
			body:    `{"model":"m","messages":[{"role":"user","content":"x"}],"response_format":{"type":"json_object"}}`,
			status:  http.StatusBadRequest,
			message: "only text response_format",
		},
		{
			name:    "stream wrong type",
			body:    `{"model":"m","messages":[{"role":"user","content":"x"}],"stream":"yes"}`,
			status:  http.StatusBadRequest,
			message: "invalid chat completion request",
		},
		{
			name:    "image block unsupported",
			body:    `{"model":"m","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://example.invalid/x"}}]}]}`,
			status:  http.StatusBadRequest,
			message: "only text is supported",
		},
		{
			name:    "tool role missing call id",
			body:    `{"model":"m","messages":[{"role":"tool","content":"x"}]}`,
			status:  http.StatusBadRequest,
			message: "tool message requires tool_call_id",
		},
		{
			name:    "string token limit",
			body:    `{"model":"m","messages":[{"role":"user","content":"x"}],"max_tokens":"1"}`,
			status:  http.StatusBadRequest,
			message: "invalid chat completion request",
		},
		{
			name:    "null messages",
			body:    `{"model":"m","messages":null}`,
			status:  http.StatusBadRequest,
			message: "messages/input is required",
		},
		{
			name:    "oversized body",
			body:    `{"model":"m","messages":[{"role":"user","content":"` + strings.Repeat("x", maxRequestBody) + `"}]}`,
			status:  http.StatusRequestEntityTooLarge,
			message: "request body exceeds 4 MiB limit",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := sent.Load()
			r := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8723/v1/chat/completions", strings.NewReader(tc.body))
			r.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			s.routes().ServeHTTP(w, r)
			var value map[string]any
			if err := json.Unmarshal(w.Body.Bytes(), &value); err != nil {
				t.Fatalf("invalid error JSON: status=%d body=%q", w.Code, w.Body.String())
			}
			errorBody, ok := value["error"].(map[string]any)
			expectedType := "invalid_request_error"
			if tc.status == http.StatusRequestEntityTooLarge {
				expectedType = "server_error"
			}
			if w.Code != tc.status || w.Header().Get("Content-Type") != "application/json" || strings.Contains(w.Body.String(), "data:") || !ok || errorBody["type"] != expectedType || errorBody["code"] != nil || !strings.Contains(fmt.Sprint(errorBody["message"]), tc.message) || sent.Load() != before {
				t.Fatalf("status=%d want=%d body=%q upstream_calls=%d", w.Code, tc.status, w.Body.String(), sent.Load()-before)
			}
		})
	}
}

func TestQA006AcceptedNullsAndTextBlocksPreserveInput(t *testing.T) {
	body := []byte(`{"model":"m","messages":[{"role":"user","content":[{"type":"input_text","text":"A"},{"type":"output_text","text":"B"}]}],"n":1,"stop":null,"response_format":null,"reasoning_effort":null,"tool_choice":null,"parallel_tool_calls":false,"temperature":0,"top_p":0}`)
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		t.Fatal(err)
	}
	req, err := decodeChatRequest(body, fields)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateRequest(&req); err != nil {
		t.Fatal(err)
	}
	if len(req.Messages) != 1 || req.Messages[0].Role != "user" || req.Messages[0].Content != "AB" || req.Choice != "auto" || req.Parallel == nil || *req.Parallel || req.Temperature == nil || *req.Temperature != 0 || req.TopP == nil || *req.TopP != 0 {
		t.Fatalf("accepted null/text/explicit-zero semantics changed: %+v", req)
	}
}

func TestQA006ToolHistoryAndSchemaNormalization(t *testing.T) {
	body := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"},{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","function":{"name":"read_file","arguments":"{}"}}]},{"role":"tool","tool_call_id":"call_1","content":"synthetic result"}],"tools":[{"type":"function","function":{"name":"read_file","parameters":{"type":"object","properties":{}}}}],"tool_choice":null}`)
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		t.Fatal(err)
	}
	req, err := decodeChatRequest(body, fields)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateRequest(&req); err != nil {
		t.Fatal(err)
	}
	if err := validateToolHistory(req.Messages); err != nil {
		t.Fatal(err)
	}
	if len(req.Messages) != 3 || req.Messages[1].Content != "" || len(req.Messages[1].ToolCalls) != 1 || req.Messages[1].ToolCalls[0].Type != "function" || req.Messages[2].ToolCallID != "call_1" || len(req.Tools) != 1 || string(req.Tools[0].Function.Parameters) != `{"type":"object","properties":{}}` {
		t.Fatalf("tool role/schema/default type changed: %+v", req)
	}
}

func TestQA006RequestBodyLimitBoundary(t *testing.T) {
	var sent atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sent.Add(1)
		fmt.Fprint(w, textFrames("ok"))
	}))
	defer upstream.Close()
	cfg := fixtureConfig()
	cfg.BaseURL = upstream.URL
	s := authenticatedServer(t, cfg)
	prefix := `{"model":"m","messages":[{"role":"user","content":"`
	suffix := `"}]}`
	valid := prefix + strings.Repeat("x", maxRequestBody-len(prefix)-len(suffix)) + suffix
	cases := []struct {
		name      string
		body      string
		status    int
		wantCalls int32
	}{
		{
			name:      "exact limit",
			body:      valid,
			status:    http.StatusOK,
			wantCalls: 1,
		},
		{
			name:      "one byte over limit",
			body:      prefix + strings.Repeat("x", maxRequestBody-len(prefix)-len(suffix)+1) + suffix,
			status:    http.StatusRequestEntityTooLarge,
			wantCalls: 1,
		},
		{
			name:      "trailing whitespace after limit",
			body:      valid + " ",
			status:    http.StatusBadRequest,
			wantCalls: 1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8723/v1/chat/completions", strings.NewReader(tc.body))
			w := httptest.NewRecorder()
			s.routes().ServeHTTP(w, r)
			if w.Code != tc.status || sent.Load() != tc.wantCalls {
				t.Fatalf("body_bytes=%d status=%d want=%d upstream_calls=%d want_calls=%d", len(tc.body), w.Code, tc.status, sent.Load(), tc.wantCalls)
			}
		})
	}
}

func TestQA006FullToolHistoryReachesWireWithoutExecution(t *testing.T) {
	wires := make(chan map[string]any, 2)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		wires <- body
		fmt.Fprint(w, textFrames("answer"))
	}))
	defer upstream.Close()
	cfg := fixtureConfig()
	cfg.BaseURL = upstream.URL
	s := authenticatedServer(t, cfg)
	for _, stream := range []bool{false, true} {
		body := fmt.Sprintf(`{"model":"m","messages":[{"role":"user","content":"read"},{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","function":{"name":"read_file","arguments":"{}"}}]},{"role":"tool","tool_call_id":"call_1","content":"synthetic caller result"},{"role":"user","content":"summarize"}],"tools":[{"type":"function","function":{"name":"read_file","parameters":{"type":"object"}}}],"stream":%t}`, stream)
		r := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8723/v1/chat/completions", strings.NewReader(body))
		w := httptest.NewRecorder()
		s.routes().ServeHTTP(w, r)
		if w.Code != http.StatusOK || (stream && !strings.Contains(w.Body.String(), "data: [DONE]")) || (!stream && !strings.Contains(w.Body.String(), `"content":"answer"`)) {
			t.Fatalf("stream=%t status=%d response=%q", stream, w.Code, w.Body.String())
		}
		select {
		case wire := <-wires:
			messages, ok := wire["messages"].([]any)
			if !ok || len(messages) != 5 {
				t.Fatalf("stream=%t tool instruction/history missing: %v", stream, wire["messages"])
			}
			instruction, ok := messages[0].(map[string]any)
			if !ok || instruction["role"] != "system" || !strings.Contains(jsonText(instruction["content"]), "Tools belong to the API caller") {
				t.Fatalf("stream=%t caller-owned tool instruction changed: %v", stream, instruction)
			}
			tools, ok := wire["tools"].([]any)
			if !ok || len(tools) != 1 || !strings.Contains(jsonText(tools[0]), `"parameters":"{\"type\":\"object\"}"`) || wire["tool_choice"] != "auto" {
				t.Fatalf("stream=%t tool schema/choice missing: %v", stream, wire["tools"])
			}
			assistant, ok := messages[2].(map[string]any)
			if !ok || assistant["role"] != "assistant" || assistant["content"] != nil {
				t.Fatalf("stream=%t assistant tool call changed: %v", stream, assistant)
			}
			calls, ok := assistant["tool_calls"].([]any)
			if !ok || len(calls) != 1 || !strings.Contains(jsonText(calls[0]), `"id":"call_1"`) || !strings.Contains(jsonText(calls[0]), `"name":"read_file"`) {
				t.Fatalf("stream=%t tool call identity lost: %v", stream, calls)
			}
			toolResult, ok := messages[3].(map[string]any)
			if !ok || toolResult["role"] != "tool" || toolResult["tool_call_id"] != "call_1" || !strings.Contains(jsonText(toolResult), "synthetic caller result") {
				t.Fatalf("stream=%t caller result changed: %v", stream, toolResult)
			}
		default:
			t.Fatalf("stream=%t no tool-history request observed", stream)
		}
	}
}

func TestQA006AcceptedInputsReachUpstreamWireInBothModes(t *testing.T) {
	type observed struct {
		body          map[string]any
		authorization string
	}
	wires := make(chan observed, 2)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		wires <- observed{
			body:          body,
			authorization: r.Header.Get("Authorization"),
		}
		fmt.Fprint(w, textFrames("ok"))
	}))
	defer upstream.Close()
	cfg := fixtureConfig()
	cfg.BaseURL = upstream.URL
	s := authenticatedServer(t, cfg)
	for _, stream := range []bool{false, true} {
		body := fmt.Sprintf(`{"model":"m","messages":[{"role":"user","content":[{"type":"input_text","text":"A"},{"type":"output_text","text":"B"}]}],"tools":[{"type":"function","function":{"name":"read_file","parameters":{"type":"object","properties":{}}}}],"tool_choice":null,"parallel_tool_calls":false,"temperature":0,"top_p":0,"stream":%t,"stop":null,"response_format":null,"reasoning_effort":null,"n":1}`, stream)
		r := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8723/v1/chat/completions", strings.NewReader(body))
		w := httptest.NewRecorder()
		s.routes().ServeHTTP(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("stream=%t gateway status=%d body=%q", stream, w.Code, w.Body.String())
		}
		if stream && !strings.Contains(w.Body.String(), "data: [DONE]") {
			t.Fatal("stream lost terminal marker")
		}
		if !stream && !strings.Contains(w.Body.String(), `"choices"`) {
			t.Fatal("nonstream lost completion")
		}
		select {
		case wire := <-wires:
			if wire.authorization != "Cloud-IDE-JWT test-token" || wire.body["model"] != "m" || wire.body["function"] != "solo_work_lite" || wire.body["temperature"] != float64(0) || wire.body["top_p"] != float64(0) || wire.body["parallel_tool_calls"] != false || wire.body["tool_choice"] != "auto" {
				t.Fatalf("stream=%t wire fields changed: %+v", stream, wire)
			}
			messages, ok := wire.body["messages"].([]any)
			if !ok || len(messages) != 2 {
				t.Fatalf("stream=%t wire messages changed: %v", stream, wire.body["messages"])
			}
			user, ok := messages[1].(map[string]any)
			if !ok || user["role"] != "user" || !strings.Contains(jsonText(user), `"text":"AB"`) {
				t.Fatalf("stream=%t text block normalization changed: %v", stream, user)
			}
			tools, ok := wire.body["tools"].([]any)
			if !ok || len(tools) != 1 || !strings.Contains(jsonText(tools[0]), `"parameters":"{\"type\":\"object\",\"properties\":{}}"`) {
				t.Fatalf("stream=%t tool schema wire changed: %v", stream, wire.body["tools"])
			}
		default:
			t.Fatalf("stream=%t no upstream request observed", stream)
		}
	}
}
