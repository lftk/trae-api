package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
)

type function struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}
type toolCall struct {
	ID       string   `json:"id"`
	Type     string   `json:"type"`
	Function function `json:"function"`
}
type message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content"`
	ToolCalls  []toolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	Name       string     `json:"name,omitempty"`
}

func (m *message) UnmarshalJSON(b []byte) error {
	var v struct {
		Role       string          `json:"role"`
		Content    json.RawMessage `json:"content"`
		ToolCalls  []toolCall      `json:"tool_calls"`
		ToolCallID string          `json:"tool_call_id"`
		Name       string          `json:"name"`
	}
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	text, err := textContent(v.Content)
	if err != nil {
		return err
	}
	for i := range v.ToolCalls {
		if v.ToolCalls[i].Type == "" {
			v.ToolCalls[i].Type = "function"
		}
	}
	*m = message{Role: v.Role, Content: text, ToolCalls: v.ToolCalls, ToolCallID: v.ToolCallID, Name: v.Name}
	return nil
}
func textContent(raw json.RawMessage) (string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", nil
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s, nil
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err != nil {
		return "", fmt.Errorf("content must be text or text blocks")
	}
	var b strings.Builder
	for _, p := range parts {
		if p.Type != "text" && p.Type != "input_text" && p.Type != "output_text" {
			return "", fmt.Errorf("content type %q is unsupported; only text is supported", p.Type)
		}
		b.WriteString(p.Text)
	}
	return b.String(), nil
}

type tool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description,omitempty"`
		Parameters  json.RawMessage `json:"parameters"`
		Strict      *bool           `json:"strict,omitempty"`
	} `json:"function"`
}
type chatRequest struct {
	Model               string          `json:"model"`
	Messages            []message       `json:"messages"`
	Stream              bool            `json:"stream"`
	Tools               []tool          `json:"tools,omitempty"`
	ToolChoice          json.RawMessage `json:"tool_choice,omitempty"`
	Parallel            *bool           `json:"parallel_tool_calls,omitempty"`
	MaxTokens           int             `json:"max_tokens,omitempty"`
	MaxCompletionTokens int             `json:"max_completion_tokens,omitempty"`
	Temperature         *float64        `json:"temperature,omitempty"`
	TopP                *float64        `json:"top_p,omitempty"`
	StreamOptions       struct {
		IncludeUsage bool `json:"include_usage"`
	} `json:"stream_options"`
	Choice string `json:"-"`
}
type usage struct {
	Input         int                 `json:"prompt_tokens"`
	Output        int                 `json:"completion_tokens"`
	Total         int                 `json:"total_tokens"`
	InputDetails  *inputTokenDetails  `json:"prompt_tokens_details,omitempty"`
	OutputDetails *outputTokenDetails `json:"completion_tokens_details,omitempty"`
}
type inputTokenDetails struct {
	CachedTokens int `json:"cached_tokens"`
}
type outputTokenDetails struct {
	ReasoningTokens int `json:"reasoning_tokens"`
}
type completion struct {
	Message   message
	Reasoning string
	Usage     *usage
	Finish    string
}
type delta struct {
	Text, Reasoning string
	Call            *toolCall
	Index           int
	Heartbeat       bool
	Start           bool
	Model           string
	Tools           []tool
}

func newID(prefix string) string {
	var b [16]byte
	if _, e := rand.Read(b[:]); e != nil {
		panic(e)
	}
	return prefix + hex.EncodeToString(b[:])
}
func jsonText(v any) string { b, _ := json.Marshal(v); return string(b) }
func choiceName(raw json.RawMessage, tools []tool) (string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return "auto", nil
	}
	var s string
	if json.Unmarshal(raw, &s) != nil {
		var obj struct {
			Type     string `json:"type"`
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		}
		if json.Unmarshal(raw, &obj) != nil || obj.Type != "function" || obj.Function.Name == "" {
			return "", fmt.Errorf("tool_choice must be auto, none, required or a named function")
		}
		s = obj.Function.Name
	}
	if s == "auto" || s == "none" {
		return s, nil
	}
	if s == "required" && len(tools) > 0 {
		return s, nil
	}
	for _, t := range tools {
		if t.Function.Name == s {
			return s, nil
		}
	}
	return "", fmt.Errorf("tool_choice must select an available tool")
}
func validateRequest(r *chatRequest) error {
	if len(r.Messages) == 0 {
		return fmt.Errorf("messages/input is required")
	}
	if r.MaxTokens < 0 || r.MaxCompletionTokens < 0 {
		return fmt.Errorf("token limits must be positive")
	}
	if r.Temperature != nil && (*r.Temperature < 0 || *r.Temperature > 2) {
		return fmt.Errorf("temperature must be between 0 and 2")
	}
	if r.TopP != nil && (*r.TopP < 0 || *r.TopP > 1) {
		return fmt.Errorf("top_p must be between 0 and 1")
	}
	for i := range r.Tools {
		t := &r.Tools[i]
		if t.Type != "function" || t.Function.Name == "" {
			return fmt.Errorf("only named function tools are supported")
		}
		for j := 0; j < i; j++ {
			if r.Tools[j].Function.Name == t.Function.Name {
				return fmt.Errorf("duplicate tool name %q", t.Function.Name)
			}
		}
		if len(t.Function.Parameters) == 0 {
			t.Function.Parameters = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		var schema map[string]any
		if e := json.Unmarshal(t.Function.Parameters, &schema); e != nil || schema == nil {
			return fmt.Errorf("tool parameters must be a JSON object")
		}
	}
	for _, m := range r.Messages {
		switch m.Role {
		case "user", "assistant", "system", "developer", "tool":
		default:
			return fmt.Errorf("unsupported role %q", m.Role)
		}
		if m.Role == "tool" && m.ToolCallID == "" {
			return fmt.Errorf("tool message requires tool_call_id")
		}
		for _, c := range m.ToolCalls {
			if m.Role != "assistant" || c.Type != "function" || c.ID == "" || c.Function.Name == "" || !json.Valid([]byte(c.Function.Arguments)) {
				return fmt.Errorf("invalid assistant tool call")
			}
		}
	}
	var e error
	r.Choice, e = choiceName(r.ToolChoice, r.Tools)
	return e
}

// Validate the full client-supplied history; no server-side conversation state.
func validateToolHistory(ms []message) error {
	pending := map[string]bool{}
	seen := map[string]bool{}
	for _, m := range ms {
		if m.Role == "tool" {
			if !pending[m.ToolCallID] {
				return fmt.Errorf("tool result has no pending call %q", m.ToolCallID)
			}
			delete(pending, m.ToolCallID)
			continue
		}
		if len(pending) > 0 {
			return fmt.Errorf("pending tool calls require their results before another message")
		}
		for _, c := range m.ToolCalls {
			if seen[c.ID] {
				return fmt.Errorf("duplicate tool call id %q", c.ID)
			}
			pending[c.ID] = true
			seen[c.ID] = true
		}
	}
	if len(pending) > 0 {
		return fmt.Errorf("all pending tool calls require results")
	}
	return nil
}
