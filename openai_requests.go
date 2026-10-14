package main

import (
	"encoding/json"
	"fmt"

	openai "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/packages/param"
)

func optionalPointer[T comparable](v param.Opt[T]) *T {
	if v.Valid() {
		return &v.Value
	}
	return nil
}

// SDK request parameters omit stream (the SDK selects it via NewStreaming).
// Raw content preserves the gateway's accepted text/input_text/output_text
// aliases and lets us reject unsupported modalities instead of losing them in
// a permissive SDK union. The field index is only for presence/unknown checks.
type chatMessageInputFields struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content"`
	ToolCalls  []toolCall      `json:"tool_calls"`
	Name       string          `json:"name"`
	ToolCallID string          `json:"tool_call_id"`
}
type chatToolInputFields struct {
	Type     string `json:"type"`
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Strict      *bool           `json:"strict"`
		Parameters  json.RawMessage `json:"parameters"`
	} `json:"function"`
}

func decodeChatRequest(body json.RawMessage) (chatRequest, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return chatRequest{}, err
	}
	if stop, ok := raw["stop"]; ok && string(stop) != "null" {
		return chatRequest{}, fmt.Errorf("stop is unsupported by the IDE upstream")
	}
	var sdk openai.ChatCompletionNewParams
	if err := json.Unmarshal(body, &sdk); err != nil {
		return chatRequest{}, err
	}
	var fields struct {
		Model               string   `json:"model"`
		MaxTokens           int64    `json:"max_tokens"`
		MaxCompletionTokens int64    `json:"max_completion_tokens"`
		Parallel            *bool    `json:"parallel_tool_calls"`
		Temperature         *float64 `json:"temperature"`
		TopP                *float64 `json:"top_p"`
		StreamOptions       struct {
			IncludeUsage bool `json:"include_usage"`
		} `json:"stream_options"`
		Stream     bool                     `json:"stream"`
		Messages   []chatMessageInputFields `json:"messages"`
		Tools      []chatToolInputFields    `json:"tools"`
		ToolChoice json.RawMessage          `json:"tool_choice"`
	}
	if err := json.Unmarshal(body, &fields); err != nil {
		return chatRequest{}, err
	}
	req := chatRequest{Model: string(sdk.Model), Stream: fields.Stream, Parallel: optionalPointer(sdk.ParallelToolCalls), MaxTokens: int(sdk.MaxTokens.Value), MaxCompletionTokens: int(sdk.MaxCompletionTokens.Value), Temperature: optionalPointer(sdk.Temperature), TopP: optionalPointer(sdk.TopP), ToolChoice: fields.ToolChoice}
	req.StreamOptions.IncludeUsage = sdk.StreamOptions.IncludeUsage.Value
	if len(fields.Messages) != len(sdk.Messages) || len(fields.Tools) != len(sdk.Tools) {
		return req, fmt.Errorf("invalid request arrays")
	}
	for i, m := range sdk.Messages {
		f := fields.Messages[i]
		content, err := textContent(f.Content)
		if err != nil {
			return req, err
		}
		role := m.GetRole()
		if role == nil || *role != f.Role {
			return req, fmt.Errorf("unsupported role %q", f.Role)
		}
		normalized := message{Role: *role, Content: content, Name: f.Name, ToolCallID: f.ToolCallID}
		// Preserve call validation on every role. Only assistant calls may survive
		// validateRequest; a missing type retains the historical function default.
		for _, c := range f.ToolCalls {
			if c.Type == "" {
				c.Type = "function"
			}
			normalized.ToolCalls = append(normalized.ToolCalls, c)
		}
		req.Messages = append(req.Messages, normalized)
	}
	for i, t := range sdk.Tools {
		f := fields.Tools[i]
		if f.Type != "function" || t.OfFunction == nil {
			return req, fmt.Errorf("only named function tools are supported")
		}
		fn := t.OfFunction.Function
		v := tool{Type: "function"}
		v.Function.Name = fn.Name
		v.Function.Description = fn.Description.Value
		v.Function.Parameters = f.Function.Parameters
		v.Function.Strict = optionalPointer(fn.Strict)
		req.Tools = append(req.Tools, v)
	}
	return req, nil
}
