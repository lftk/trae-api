package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	openai "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
)

func TestOfficialSDKChatCompatibility(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, frame("output", map[string]any{"reasoning_content": "check", "tool_calls": []any{map[string]any{"id": "call_read", "function_call": map[string]string{"name": "read_file", "args": `{"path":"README.md"}`}}}})+frame("done", map[string]any{"finish_reason": "tool_calls", "usage": map[string]any{
			"prompt_tokens":     12,
			"completion_tokens": 3,
			"total_tokens":      15,
			"cached_tokens":     2,
		}}))
	}))
	defer up.Close()
	cfg := fixtureConfig()
	cfg.BaseURL = up.URL
	s := authenticatedServer(t, cfg)
	api := httptest.NewServer(s.routes())
	defer api.Close()
	client := openai.NewClient(option.WithBaseURL(api.URL+"/v1/"), option.WithAPIKey("test"), option.WithMaxRetries(0))
	var params openai.ChatCompletionNewParams
	if err := json.Unmarshal([]byte(`{"model":"m","messages":[{"role":"user","content":"read"}],"tools":[{"type":"function","function":{"name":"read_file","parameters":{"type":"object"}}}],"stream_options":{"include_usage":true}}`), &params); err != nil {
		t.Fatal(err)
	}
	result, err := client.Chat.Completions.New(context.Background(), params)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Choices) != 1 || result.Choices[0].FinishReason != "tool_calls" || !result.Choices[0].Message.JSON.Content.Valid() || result.Choices[0].Message.Content != "" || len(result.Choices[0].Message.ToolCalls) != 1 {
		t.Fatalf("invalid completion: %s", result.RawJSON())
	}
	call := result.Choices[0].Message.ToolCalls[0].AsFunction()
	if call.ID != "call_read" || call.Function.Arguments != `{"path":"README.md"}` || result.Usage.PromptTokens != 12 || result.Usage.PromptTokensDetails.CachedTokens != 2 {
		t.Fatal(result.RawJSON())
	}
	if field, ok := result.Choices[0].Message.JSON.ExtraFields["reasoning_content"]; !ok || field.Raw() != `"check"` {
		t.Fatal("missing reasoning extension")
	}
	stream := client.Chat.Completions.NewStreaming(context.Background(), params)
	defer stream.Close()
	var arguments string
	usageSeen := false
	finishSeen := false
	reasoningSeen := false
	for stream.Next() {
		chunk := stream.Current()
		if len(chunk.Choices) == 0 {
			usageSeen = true
			if chunk.Usage.TotalTokens != 15 {
				t.Fatal(chunk.RawJSON())
			}
			continue
		}
		choice := chunk.Choices[0]
		if field, ok := choice.Delta.JSON.ExtraFields["reasoning_content"]; ok {
			reasoningSeen = true
			if field.Raw() != `"check"` {
				t.Fatal(chunk.RawJSON())
			}
		}
		if choice.FinishReason != "" {
			finishSeen = true
			if choice.FinishReason != "tool_calls" {
				t.Fatal(chunk.RawJSON())
			}
		}
		for _, call := range choice.Delta.ToolCalls {
			arguments += call.Function.Arguments
		}
	}
	if err := stream.Err(); err != nil {
		t.Fatal(err)
	}
	if arguments != `{"path":"README.md"}` || !usageSeen || !finishSeen || !reasoningSeen {
		t.Fatalf("args=%q usage=%v finish=%v reasoning=%v", arguments, usageSeen, finishSeen, reasoningSeen)
	}
}

func TestChatUsesSDKDefaultJSON(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, textFrames("hello")) }))
	defer up.Close()
	cfg := fixtureConfig()
	cfg.BaseURL = up.URL
	s := authenticatedServer(t, cfg)
	for _, stream := range []bool{false, true} {
		w := callAPI(t, s, "/v1/chat/completions", map[string]any{"model": "m", "messages": []message{{
			Role:    "user",
			Content: "hi",
		}}, "stream": stream, "stream_options": map[string]bool{"include_usage": true}}, "")
		if w.Code != 200 {
			t.Fatal(w.Code, w.Body.String())
		}
		if !stream {
			var value openai.ChatCompletion
			if err := json.Unmarshal(w.Body.Bytes(), &value); err != nil {
				t.Fatal(err)
			}
			expected, err := json.Marshal(value)
			if err != nil {
				t.Fatal(err)
			}
			assertJSONEqual(t, w.Body.Bytes(), expected)
			continue
		}
		ev := events(t, w)
		for _, event := range ev {
			body, err := json.Marshal(event)
			if err != nil {
				t.Fatal(err)
			}
			var chunk openai.ChatCompletionChunk
			if err = json.Unmarshal(body, &chunk); err != nil {
				t.Fatal(err)
			}
			expected, err := json.Marshal(chunk)
			if err != nil {
				t.Fatal(err)
			}
			assertJSONEqual(t, body, expected)
		}
		last := ev[len(ev)-1]
		if len(last["choices"].([]any)) != 0 || last["usage"].(map[string]any)["total_tokens"] != float64(8) {
			t.Fatal(last)
		}
	}
}

func assertJSONEqual(t *testing.T, actual, expected []byte) {
	t.Helper()
	var a, b any
	if err := json.Unmarshal(actual, &a); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(expected, &b); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(a, b) {
		t.Fatalf("JSON differs from SDK encoding:\nactual: %s\nexpected: %s", actual, expected)
	}
}

func TestStreamEncodingFailureDoesNotStartResponse(t *testing.T) {
	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	sink := newStreamSink(w, req)
	defer sink.close()
	if err := sink.send("", make(chan int)); err == nil {
		t.Fatal("invalid output union was encoded")
	}
	if sink.hasStarted() || w.Body.Len() != 0 || w.Header().Get("Content-Type") == "text/event-stream" {
		t.Fatal("encoding failure started SSE response")
	}
}

func TestOfficialSDKErrorCompatibility(t *testing.T) {
	s := authenticatedServer(t, fixtureConfig())
	api := httptest.NewServer(s.routes())
	defer api.Close()
	client := openai.NewClient(option.WithBaseURL(api.URL+"/v1/"), option.WithAPIKey("test"), option.WithMaxRetries(0))
	_, err := client.Chat.Completions.New(context.Background(), openai.ChatCompletionNewParams{
		Model: "m",
	})
	var apiError *openai.Error
	if !errors.As(err, &apiError) || apiError.StatusCode != 400 || apiError.Type != "invalid_request_error" || apiError.Message == "" {
		t.Fatalf("unexpected SDK error: %v", err)
	}
	if apiError.JSON.Code.Raw() != "null" {
		t.Fatal(apiError.RawJSON())
	}
	for _, field := range []string{"Request", "Response", "StatusCode", "param"} {
		if strings.Contains(apiError.RawJSON(), `"`+field+`"`) {
			t.Fatal("SDK runtime field leaked into API error", apiError.RawJSON())
		}
	}
}

func TestTRAE003ChatFullHistoryToolRoundTrip(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			turns := 0
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/ide/v1/batch_get_detail_param" {
					fmt.Fprint(w, `{"function_configs":[{"function":"solo_work_lite","config_info_list":[{"config_name":"m"}]}]}`)
					return
				}
				turns++
				var b map[string]any
				json.NewDecoder(r.Body).Decode(&b)
				if turns == 1 {
					fmt.Fprint(w, frame("output", map[string]any{"reasoning_content": "thinking", "tool_call": map[string]any{"id": "a", "function_call": map[string]string{"name": "read_file", "arguments": "{}"}}})+frame("output", map[string]any{"tool_call": map[string]any{"id": "b", "function_call": map[string]string{"name": "read_file", "arguments": "{"}}})+frame("output", map[string]any{"function_call": map[string]string{"arguments": "}"}})+frame("done", map[string]any{}))
					return
				}
				wire := jsonText(b)
				for _, want := range []string{"original", `"id":"a"`, `"id":"b"`, `"tool_call_id":"a"`, `"tool_call_id":"b"`, "result A", "result B"} {
					if !strings.Contains(wire, want) {
						t.Errorf("missing %s in %s", want, wire)
					}
				}
				fmt.Fprint(w, textFrames("verified"))
			}))
			defer up.Close()
			cfg := fixtureConfig()
			cfg.BaseURL = up.URL
			s := authenticatedServer(t, cfg)
			first := callAPI(t, s, "/v1/chat/completions", map[string]any{"stream": stream, "tools": []tool{fileTool()}, "messages": []message{{
				Role:    "user",
				Content: "original",
			}}}, "")
			if first.Code != 200 || !strings.Contains(first.Body.String(), "reasoning_content") || !strings.Contains(first.Body.String(), "tool_calls") {
				t.Fatal(first.Body.String())
			}
			history := []message{{
				Role:    "user",
				Content: "original",
			}, {
				Role: "assistant",
				ToolCalls: []toolCall{{
					ID:   "a",
					Type: "function",
					Function: function{
						Name:      "read_file",
						Arguments: "{}",
					},
				}, {
					ID:   "b",
					Type: "function",
					Function: function{
						Name:      "read_file",
						Arguments: "{}",
					},
				}},
			}, {
				Role:       "tool",
				ToolCallID: "a",
				Content:    "result A",
			}, {
				Role:       "tool",
				ToolCallID: "b",
				Content:    "result B",
			}}
			second := callAPI(t, s, "/v1/chat/completions", map[string]any{
				"model":    "m",
				"stream":   stream,
				"tools":    []tool{fileTool()},
				"messages": history,
			}, "")
			if second.Code != 200 || !strings.Contains(second.Body.String(), "verified") || turns != 2 {
				t.Fatalf("turns=%d body=%s", turns, second.Body.String())
			}
			incomplete := callAPI(t, s, "/v1/chat/completions", map[string]any{"model": "m", "messages": []message{{
				Role:       "tool",
				ToolCallID: "a",
				Content:    "only result",
			}}}, "same")
			if incomplete.Code != 400 || turns != 2 {
				t.Fatal("incremental tool history accepted")
			}
		})
	}
}

// These inputs pin the gateway's accepted and rejected JSON shapes during
// decoder simplification. All credentials and upstream responses are synthetic.
func TestSimplifyMessageNormalization(t *testing.T) {
	raw := []byte(`{"role":"assistant","content":[{"type":"output_text","text":"hello"}],"tool_calls":[{"id":"call_1","function":{"name":"read_file","arguments":"{}"}}]}`)
	var direct message
	if err := json.Unmarshal(raw, &direct); err != nil {
		t.Fatal(err)
	}
	if direct.Content != "hello" || len(direct.ToolCalls) != 1 || direct.ToolCalls[0].Type != "function" {
		t.Fatalf("unexpected direct message: %+v", direct)
	}
	body := []byte(`{"model":"m","messages":[` + string(raw) + `]}`)
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		t.Fatal(err)
	}
	request, err := decodeChatRequest(body, fields)
	if err != nil {
		t.Fatal(err)
	}
	if len(request.Messages) != 1 || request.Messages[0].Content != direct.Content || request.Messages[0].ToolCalls[0].Type != direct.ToolCalls[0].Type {
		t.Fatalf("normalization differs: %+v", request.Messages)
	}
}

func TestSimplifyRequestCompatibility(t *testing.T) {
	base := `"model":"m","messages":[{"role":"user","content":"hi"}]`
	tests := []struct {
		name   string
		body   string
		status int
	}{
		{
			name:   "plain",
			body:   "{" + base + "}",
			status: 200,
		},
		{
			name:   "stop null",
			body:   "{" + base + `,"stop":null}`,
			status: 200,
		},
		{
			name:   "stop list",
			body:   "{" + base + `,"stop":["bye"]}`,
			status: 400,
		},
		{
			name:   "n one",
			body:   "{" + base + `,"n":1}`,
			status: 200,
		},
		{
			name:   "n float",
			body:   "{" + base + `,"n":1.0}`,
			status: 400,
		},
		{
			name:   "n null",
			body:   "{" + base + `,"n":null}`,
			status: 400,
		},
		{
			name:   "reasoning empty",
			body:   "{" + base + `,"reasoning":{}}`,
			status: 200,
		},
		{
			name:   "reasoning nonempty",
			body:   "{" + base + `,"reasoning":{"effort":"high"}}`,
			status: 400,
		},
		{
			name:   "text format",
			body:   "{" + base + `,"response_format":{"type":"text"}}`,
			status: 200,
		},
		{
			name:   "json format",
			body:   "{" + base + `,"response_format":{"type":"json_object"}}`,
			status: 400,
		},
		{
			name:   "input text block",
			body:   `{"model":"m","messages":[{"role":"user","content":[{"type":"input_text","text":"hi"}]}]}`,
			status: 200,
		},
		{
			name:   "image block",
			body:   `{"model":"m","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"x"}}]}]}`,
			status: 400,
		},
		{
			name:   "two values",
			body:   "{" + base + "} {}",
			status: 400,
		},
		{
			name:   "array top level",
			body:   `[{"model":"m"}]`,
			status: 400,
		},
		{
			name:   "wrong max type",
			body:   "{" + base + `,"max_tokens":"10"}`,
			status: 400,
		},
		{
			name:   "wrong parallel type",
			body:   "{" + base + `,"parallel_tool_calls":"true"}`,
			status: 400,
		},
		{
			name:   "wrong temperature type",
			body:   "{" + base + `,"temperature":"1"}`,
			status: 400,
		},
		{
			name:   "wrong tool name type",
			body:   "{" + base + `,"tools":[{"type":"function","function":{"name":7,"parameters":{}}]}`,
			status: 400,
		},
		{
			name:   "wrong strict type",
			body:   "{" + base + `,"tools":[{"type":"function","function":{"name":"f","strict":"true","parameters":{}}]}`,
			status: 400,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			calls := 0
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++; fmt.Fprint(w, textFrames("ok")) }))
			defer up.Close()
			cfg := fixtureConfig()
			cfg.BaseURL = up.URL
			s := authenticatedServer(t, cfg)
			req := httptest.NewRequest("POST", "http://127.0.0.1:8723/v1/chat/completions", strings.NewReader(tt.body))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			s.routes().ServeHTTP(w, req)
			if w.Code != tt.status {
				t.Fatalf("status=%d want=%d body=%s", w.Code, tt.status, w.Body.String())
			}
			if tt.status == 200 {
				if calls != 1 || !strings.Contains(w.Body.String(), `"content":"ok"`) {
					t.Fatalf("calls=%d body=%s", calls, w.Body.String())
				}
			} else if calls != 0 {
				t.Fatalf("rejected request sent upstream %d times", calls)
			}
		})
	}
}
