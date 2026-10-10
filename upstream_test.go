package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func fixtureConfig() config {
	c := defaultConfig()
	c.RequestTimeout = 5 * time.Second
	c.QueueTimeout = time.Second
	c.ShutdownTimeout = time.Second
	return c
}
func frame(event string, v any) string { return "event: " + event + "\ndata: " + jsonText(v) + "\n\n" }
func textFrames(text string) string {
	return frame("output", map[string]any{"response": text}) + frame("token_usage", map[string]int{"input_token": 3, "output_token": 5}) + frame("done", map[string]string{"finish_reason": "stop"})
}
func fileTool() tool {
	var t tool
	_ = json.Unmarshal([]byte(`{"type":"function","function":{"name":"read_file","parameters":{"type":"object","properties":{"path":{"type":"string"}}}}}`), &t)
	return t
}
func TestIDEStreamNativeArgumentsAndParallelIdentity(t *testing.T) {
	frames := frame("output", map[string]any{"tool_calls": []any{map[string]any{"index": 0, "id": "call_a", "function_call": map[string]string{"name": "read_file", "args": ""}}, map[string]any{"index": 1, "id": "call_b", "function_call": map[string]string{"name": "read_file", "arguments": ""}}}})
	for _, f := range []struct {
		index int
		args  string
	}{{0, "{"}, {0, `"outer":`}, {0, "{"}, {1, `{"path":"b.txt"}`}, {0, `"x":1`}, {0, "}"}, {0, "}"}} {
		frames += frame("output", map[string]any{"tool_calls": []any{map[string]any{"index": f.index, "function_call": map[string]string{"arguments": f.args}}}})
	}
	frames += frame("done", map[string]string{"finish_reason": "tool_calls"})
	var deltas []delta
	r := chatRequest{Tools: []tool{fileTool()}, Choice: "auto"}
	result, e := readIDEStream(strings.NewReader(frames), r, func(d delta) error { deltas = append(deltas, d); return nil })
	if e != nil {
		t.Fatal(e)
	}
	calls := result.Message.ToolCalls
	if len(calls) != 2 || calls[0].ID != "call_a" || calls[1].ID != "call_b" || calls[0].Function.Arguments != `{"outer":{"x":1}}` || calls[1].Function.Arguments != `{"path":"b.txt"}` {
		t.Fatalf("bad calls %+v", calls)
	}
	var reconstructed [2]string
	for _, d := range deltas {
		if d.Call != nil {
			reconstructed[d.Index] += d.Call.Function.Arguments
		}
	}
	for i, c := range calls {
		if reconstructed[i] != c.Function.Arguments {
			t.Fatalf("stream %q != final %q", reconstructed[i], c.Function.Arguments)
		}
	}
}
func TestIDEStreamSnapshotsReasoningAndFailures(t *testing.T) {
	r := chatRequest{Choice: "auto"}
	frames := frame("output", map[string]string{"response": "你", "reasoning_content": "思"}) + frame("output", map[string]string{"response": "你好", "reasoning_content": "思考"}) + frame("output", map[string]string{"response": "你好", "reasoning_content": "思考"}) + frame("done", map[string]string{})
	c, e := readIDEStream(strings.NewReader(frames), r, nil)
	if e != nil || c.Message.Content != "你好" || c.Reasoning != "思考" {
		t.Fatalf("%+v %v", c, e)
	}
	for _, test := range []struct {
		name, frames string
		req          chatRequest
	}{
		{"truncated", frame("output", map[string]string{"response": "partial"}), r},
		{"empty", frame("done", map[string]string{}), r},
		{"business", frame("output", map[string]int{"code": 4028}), r},
		{"required missing", textFrames("answer"), chatRequest{Tools: []tool{fileTool()}, Choice: "required"}},
		{"undeclared", frame("output", map[string]any{"tool_calls": []any{map[string]any{"id": "a", "function_call": map[string]string{"name": "bash", "args": "{}"}}}}) + frame("done", map[string]string{}), r},
		{"bad JSON", "data: garbage\n\n", r},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, e := readIDEStream(strings.NewReader(test.frames), test.req, nil); e == nil {
				t.Fatal("invalid stream accepted")
			}
		})
	}
}
func TestIDEBodyKeepsToolHistory(t *testing.T) {
	r := chatRequest{Model: "m", Choice: "required", Tools: []tool{fileTool()}, Messages: []message{{Role: "developer", Content: "constraint"}, {Role: "assistant", ToolCalls: []toolCall{{ID: "a", Type: "function", Function: function{Name: "read_file", Arguments: `{"path":"README.md"}`}}}}, {Role: "tool", ToolCallID: "a", Content: "file content"}}}
	body := buildIDEBody(r, "session")
	wire := jsonText(body)
	for _, want := range []string{`"function":"solo_work_lite"`, `"function_call"`, `"tool_call_id":"a"`, `"name":"read_file"`, `"role":"system"`, `"stream":true`} {
		if !strings.Contains(wire, want) {
			t.Fatalf("missing %s in %s", want, wire)
		}
	}
	tools := body["tools"].([]any)
	fn := tools[0].(map[string]any)["function"].(map[string]any)
	if _, ok := fn["parameters"].(string); !ok {
		t.Fatal("schema is not a JSON string")
	}
	r.Choice = "none"
	if _, ok := buildIDEBody(r, "s")["tools"]; ok {
		t.Fatal("none exposed tools")
	}
}
func TestModelsAndIDETransport(t *testing.T) {
	var prompts int
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Cloud-IDE-JWT test-token" || r.Header.Get("X-Cloudide-Token") != "test-token" {
			t.Error("wrong auth")
		}
		switch r.URL.Path {
		case "/api/remote/v1/models":
			fmt.Fprint(w, `{"code":0,"data":{"list":[{"models":[{"name":"model-b"},{"name":"model-a"}]}]}}`)
		case "/api/agent/v3/llm_utils_chat":
			prompts++
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["config_name"] != "model-a" {
				t.Error(body)
			}
			fmt.Fprint(w, textFrames("hello"))
		default:
			t.Errorf("unexpected upstream %s", r.URL.Path)
		}
	}))
	defer up.Close()
	cfg := fixtureConfig()
	cfg.BaseURL = up.URL
	s, e := newServer(cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer s.shutdown()
	s.auth.cred = credential{Token: "test-token", UserID: "account"}
	req, c, e := s.execute(context.Background(), chatRequest{Messages: []message{{Role: "user", Content: "hi"}}}, nil)
	if e != nil || c.Message.Content != "hello" || req.Model != "model-a" || prompts != 1 {
		t.Fatalf("%+v %+v %v", req, c, e)
	}
}
func TestEmptyArgumentsStreamMatchesFinal(t *testing.T) {
	frames := frame("output", map[string]any{"tool_calls": []any{map[string]any{"id": "empty", "function_call": map[string]string{"name": "read_file"}}}}) + frame("done", map[string]string{})
	args := ""
	c, e := readIDEStream(strings.NewReader(frames), chatRequest{Choice: "auto", Tools: []tool{fileTool()}}, func(d delta) error {
		if d.Call != nil {
			args += d.Call.Function.Arguments
		}
		return nil
	})
	if e != nil || args != "{}" || c.Message.ToolCalls[0].Function.Arguments != args {
		t.Fatalf("%+v %s %v", c, args, e)
	}

}
