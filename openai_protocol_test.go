package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	openai "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func TestOfficialSDKChatCompatibility(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, frame("output", map[string]any{"reasoning_content": "check", "tool_calls": []any{map[string]any{"id": "call_read", "function_call": map[string]string{"name": "read_file", "args": `{"path":"README.md"}`}}}})+frame("done", map[string]any{"finish_reason": "tool_calls", "usage": map[string]any{"prompt_tokens": 12, "completion_tokens": 3, "total_tokens": 15, "cached_tokens": 2}}))
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
		w := callAPI(t, s, "/v1/chat/completions", map[string]any{"model": "m", "messages": []message{{Role: "user", Content: "hi"}}, "stream": stream, "stream_options": map[string]bool{"include_usage": true}}, "")
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
	_, err := client.Chat.Completions.New(context.Background(), openai.ChatCompletionNewParams{Model: "m"})
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
