package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

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
			first := callAPI(t, s, "/v1/chat/completions", map[string]any{"stream": stream, "tools": []tool{fileTool()}, "messages": []message{{Role: "user", Content: "original"}}}, "")
			if first.Code != 200 || !strings.Contains(first.Body.String(), "reasoning_content") || !strings.Contains(first.Body.String(), "tool_calls") {
				t.Fatal(first.Body.String())
			}
			history := []message{{Role: "user", Content: "original"}, {Role: "assistant", ToolCalls: []toolCall{{ID: "a", Type: "function", Function: function{Name: "read_file", Arguments: "{}"}}, {ID: "b", Type: "function", Function: function{Name: "read_file", Arguments: "{}"}}}}, {Role: "tool", ToolCallID: "a", Content: "result A"}, {Role: "tool", ToolCallID: "b", Content: "result B"}}
			second := callAPI(t, s, "/v1/chat/completions", map[string]any{"model": "m", "stream": stream, "tools": []tool{fileTool()}, "messages": history}, "")
			if second.Code != 200 || !strings.Contains(second.Body.String(), "verified") || turns != 2 {
				t.Fatalf("turns=%d body=%s", turns, second.Body.String())
			}
			incomplete := callAPI(t, s, "/v1/chat/completions", map[string]any{"model": "m", "messages": []message{{Role: "tool", ToolCallID: "a", Content: "only result"}}}, "same")
			if incomplete.Code != 400 || turns != 2 {
				t.Fatal("incremental tool history accepted")
			}
		})
	}
}
