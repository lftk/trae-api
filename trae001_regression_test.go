package main

import (
	"strings"
	"testing"
)

func TestTRAE001ToolIdentity(t *testing.T) {
	for _, scenario := range []struct {
		name    string
		records []map[string]any
	}{
		{
			"separate IDs without indices",
			[]map[string]any{
				{"id": "a", "function_call": map[string]string{"name": "read_file", "arguments": "{}"}},
				{"id": "b", "function_call": map[string]string{"name": "read_file", "arguments": "{}"}},
			},
		},
		{
			"ID acquires index",
			[]map[string]any{
				{"id": "a", "function_call": map[string]string{"name": "read_file", "arguments": "{"}},
				{"id": "a", "index": 7, "function_call": map[string]string{"arguments": ""}},
				{"index": 7, "function_call": map[string]string{"arguments": "}"}},
			},
		},
		{
			"unindexed continuation follows latest",
			[]map[string]any{
				{"index": 0, "id": "a", "function_call": map[string]string{"name": "read_file", "arguments": "{}"}},
				{"index": 1, "id": "b", "function_call": map[string]string{"name": "read_file", "arguments": "{"}},
				{"function_call": map[string]string{"arguments": "}"}},
			},
		},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			wire := ""
			for _, record := range scenario.records {
				wire += frame("output", map[string]any{"tool_call": record})
			}
			wire += frame("done", map[string]any{})
			var streamed []delta
			result, err := readIDEStream(strings.NewReader(wire), chatRequest{
				Tools:  []tool{fileTool()},
				Choice: "auto",
			}, func(d delta) error {
				streamed = append(streamed, d)
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			want := 2
			if scenario.name == "ID acquires index" {
				want = 1
			}
			if len(result.Message.ToolCalls) != want {
				t.Fatalf("calls=%+v", result.Message.ToolCalls)
			}
			args := make([]string, want)
			ids := make([]string, want)
			for _, d := range streamed {
				if d.Call != nil {
					if d.Index >= want {
						t.Fatalf("index %d", d.Index)
					}
					args[d.Index] += d.Call.Function.Arguments
					if d.Call.ID != "" {
						ids[d.Index] = d.Call.ID
					}
				}
			}
			for i, c := range result.Message.ToolCalls {
				if c.Function.Arguments != "{}" || args[i] != c.Function.Arguments || ids[i] != c.ID {
					t.Fatalf("call=%+v stream=%v ids=%v", c, args, ids)
				}
			}
		})
	}
}
