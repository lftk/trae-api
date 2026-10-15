package main

import (
	"encoding/json"

	openai "github.com/openai/openai-go/v3"
	"github.com/tidwall/sjson"
)

// Chat responses use the SDK's default JSON encoding. The only addition is
// reasoning_content, a Trae extension absent from the official SDK model.
func marshalChatReasoning[T openai.ChatCompletion | openai.ChatCompletionChunk](value T, reasoning string) (json.RawMessage, error) {
	data, err := json.Marshal(value)
	if err != nil || reasoning == "" {
		return data, err
	}
	path := "choices.0.message.reasoning_content"
	if _, stream := any(value).(openai.ChatCompletionChunk); stream {
		path = "choices.0.delta.reasoning_content"
	}
	data, err = sjson.SetBytes(data, path, reasoning)
	return data, err
}

func chatUsage(u *usage) openai.CompletionUsage {
	if u == nil {
		return openai.CompletionUsage{}
	}
	v := openai.CompletionUsage{
		PromptTokens:     int64(u.Input),
		CompletionTokens: int64(u.Output),
		TotalTokens:      int64(u.Total),
	}
	if u.InputDetails != nil {
		v.PromptTokensDetails.CachedTokens = int64(u.InputDetails.CachedTokens)
	}
	if u.OutputDetails != nil {
		v.CompletionTokensDetails.ReasoningTokens = int64(u.OutputDetails.ReasoningTokens)
	}
	return v
}

// rawJSONFields is transient serialized JSON, never the request/response model.
// It lets the adapters fix null/omission semantics without re-declaring SDK
// fields. Encoding errors propagate before any stream headers are sent.
type rawJSONFields map[string]json.RawMessage

func jsonFields(v any) (rawJSONFields, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var fields rawJSONFields
	if err = json.Unmarshal(b, &fields); err != nil {
		return nil, err
	}
	return fields, nil
}
func (f rawJSONFields) set(key string, v any) error {
	b, err := json.Marshal(v)
	if err == nil {
		f[key] = b
	}
	return err
}
func (f rawJSONFields) drop(keys ...string) {
	for _, k := range keys {
		delete(f, k)
	}
}

type openAIErrorJSON struct {
	detail      openai.Error
	includeCode bool
}

func (v openAIErrorJSON) MarshalJSON() ([]byte, error) {
	fields, err := jsonFields(v.detail)
	if err != nil {
		return nil, err
	}
	fields.drop("StatusCode", "Request", "Response", "param")
	if v.includeCode {
		fields["code"] = json.RawMessage("null")
	} else {
		fields.drop("code")
	}
	return json.Marshal(struct {
		Error rawJSONFields `json:"error"`
	}{
		fields,
	})
}
