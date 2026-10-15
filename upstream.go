package main

// Protocol references and pinned source revisions are recorded in REFERENCES.md.
import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

func buildRoutedIDEBody(r chatRequest, sessionID string, route modelRoute) map[string]any {
	var messages []any
	if len(r.Tools) > 0 {
		messages = append(messages, map[string]any{"role": "system", "content": []any{map[string]string{"type": "text", "text": "Tools belong to the API caller. Request tools using native function calls, then wait for the caller's tool result with the matching call ID. Do not execute tools yourself or claim a file or command succeeded without a successful caller result."}}})
	}
	names := map[string]string{}
	for _, m := range r.Messages {
		for _, c := range m.ToolCalls {
			names[c.ID] = c.Function.Name
		}
	}
	for _, m := range r.Messages {
		role := m.Role
		if role == "developer" {
			role = "system"
		}
		item := map[string]any{"role": role, "content": []any{map[string]string{"type": "text", "text": m.Content}}}
		if len(m.ToolCalls) > 0 {
			var calls []any
			for _, c := range m.ToolCalls {
				calls = append(calls, map[string]any{
					"id":            c.ID,
					"type":          "function",
					"function_call": c.Function,
				})
			}
			item["tool_calls"] = calls
			if m.Content == "" {
				item["content"] = nil
			}
		}
		if m.ToolCallID != "" {
			item["tool_call_id"] = m.ToolCallID
			name := m.Name
			if name == "" {
				name = names[m.ToolCallID]
			}
			if name != "" {
				item["name"] = name
			}
		}
		messages = append(messages, item)
	}
	body := map[string]any{
		"messages":    messages,
		"config_name": r.Model,
		"model":       r.Model,
		"function":    "solo_work_lite",
		"stream":      true,
		"request_id":  sessionID,
		"session_id":  sessionID,
	}
	if route.base != "" {
		body["config_name"] = route.base
		body["model"] = route.base
	}
	if route.function != "" {
		body["function"] = route.function
	}
	if route.name != "" {
		body["model_name"] = route.name
	}
	limit := r.MaxCompletionTokens
	if limit == 0 {
		limit = r.MaxTokens
	}
	if route.window > 0 && limit == 0 && route.most > 0 {
		limit = route.most
	}
	if limit > 0 {
		if route.most > 0 && limit > route.most {
			limit = route.most
		}
		if route.window > 0 && limit > route.window {
			limit = route.window
		}
		body["max_tokens"] = limit
	}
	if route.window > 0 {
		body["user_message_context"] = map[string]any{"model_info": map[string]any{"prompt_max_tokens": route.window - limit}}
	}
	if r.Temperature != nil {
		body["temperature"] = *r.Temperature
	}
	if r.TopP != nil {
		body["top_p"] = *r.TopP
	}
	if len(r.Tools) > 0 && r.Choice != "none" {
		var tools []any
		for _, t := range r.Tools {
			fn := map[string]any{
				"name":        t.Function.Name,
				"description": t.Function.Description,
				"parameters":  string(t.Function.Parameters),
			}
			if t.Function.Strict != nil {
				fn["strict"] = *t.Function.Strict
			}
			tools = append(tools, map[string]any{"type": "function", "function": fn})
		}
		body["tools"] = tools
		body["tool_choice"] = r.Choice
		if r.Parallel != nil {
			body["parallel_tool_calls"] = *r.Parallel
		}
	}
	return body
}

func (t *traeClient) resolveRoute(ctx context.Context, c apiCredential, g uint64, model string) (modelRoute, error) {
	t.mu.Lock()
	cached := t.cache[g]
	route, ok := cached.routes[model]
	valid := time.Since(cached.at) < 5*time.Minute
	t.mu.Unlock()
	if !ok || !valid {
		// Explicit models need their function and native model name too. Discovery
		// failures are surfaced rather than silently routing through another mode.
		if _, err := t.modelNames(ctx, c, g); err != nil {
			return modelRoute{}, err
		}
		t.mu.Lock()
		route, ok = t.cache[g].routes[model]
		t.mu.Unlock()
		if !ok {
			return modelRoute{}, badRequest("model is not available for this account")
		}
	}
	return route, nil
}

func (t *traeClient) promptAttempt(ctx context.Context, c apiCredential, g uint64, r chatRequest, id string, onDelta func(delta) error) (completion, error) {
	route, err := t.resolveRoute(ctx, c, g, r.Model)
	if err != nil {
		return completion{}, err
	}
	endpoint := strings.TrimRight(t.cfg.BaseURL, "/") + "/api/agent/v3/llm_utils_chat"
	req, e := http.NewRequestWithContext(ctx, "POST", endpoint, bytes.NewBufferString(jsonText(buildRoutedIDEBody(r, id, route))))
	if e != nil {
		return completion{}, e
	}
	req.Header = t.headers(c)
	req.Header.Set("Referer", endpoint)
	resp, e := t.http.Do(req)
	if e != nil {
		if ctx.Err() != nil {
			return completion{}, ctx.Err()
		}
		return completion{}, fmt.Errorf("Trae IDE connection failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		if resp.StatusCode == 401 || resp.StatusCode == 403 {
			return completion{}, errCredentialRejected
		}
		return completion{}, fmt.Errorf("Trae IDE returned HTTP %d", resp.StatusCode)
	}
	return readIDEStream(resp.Body, r, onDelta)
}

type accumulatedCall struct {
	call     toolCall
	sent     bool
	sentArgs string
}

type ideAccumulator struct {
	result   completion
	calls    []*accumulatedCall
	byID     map[string]int
	byIndex  map[int]int
	r        chatRequest
	emit     func(delta) error
	terminal bool
	lastCall int // one-based most recently addressed native call
}

func readIDEStream(reader io.Reader, r chatRequest, emit func(delta) error) (completion, error) {
	a := ideAccumulator{
		result: completion{
			Message: message{
				Role: "assistant",
			},
			Finish: "stop",
		},
		byID:    map[string]int{},
		byIndex: map[int]int{},
		r:       r,
		emit:    emit,
	}
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 4096), 4<<20)
	event := ""
	var data []string
	flush := func() error {
		if len(data) == 0 {
			if normalizeEvent(event) == "done" {
				a.terminal = true
			}
			event = ""
			return nil
		}
		e := a.frame(event, strings.Join(data, "\n"))
		event = ""
		data = nil
		return e
	}
	for scanner.Scan() {
		line := scanner.Text()
		if strings.TrimSpace(line) == "" {
			if e := flush(); e != nil {
				return a.result, e
			}
			if a.terminal {
				break
			}
			continue
		}
		if strings.HasPrefix(line, "event:") {
			if len(data) > 0 {
				if e := flush(); e != nil {
					return a.result, e
				}
			}
			event = strings.TrimSpace(line[6:])
		} else if strings.HasPrefix(line, "data:") {
			data = append(data, strings.TrimSpace(line[5:]))
		} else if strings.HasPrefix(line, ":") {
			if e := a.send(delta{
				Heartbeat: true,
			}); e != nil {
				return a.result, e
			}
		}
	}
	if e := scanner.Err(); e != nil {
		return a.result, fmt.Errorf("Trae stream read failed: %w", e)
	}
	if !a.terminal {
		if e := flush(); e != nil {
			return a.result, e
		}
	}
	if !a.terminal {
		return a.result, fmt.Errorf("Trae stream ended without a terminal event")
	}
	for _, state := range a.calls {
		c := state.call
		if !state.sent {
			return a.result, fmt.Errorf("Trae returned an unnamed tool")
		}
		if c.Function.Arguments == "" {
			c.Function.Arguments = "{}"
			if e := a.send(delta{
				Index: len(a.result.Message.ToolCalls),
				Call: &toolCall{
					Function: function{
						Arguments: "{}",
					},
				},
			}); e != nil {
				return a.result, e
			}
		}
		if !json.Valid([]byte(c.Function.Arguments)) {
			return a.result, fmt.Errorf("Trae returned invalid tool arguments")
		}
		a.result.Message.ToolCalls = append(a.result.Message.ToolCalls, c)
	}
	if len(a.calls) > 0 {
		a.result.Finish = "tool_calls"
	} else if r.Choice == "required" || (r.Choice != "auto" && r.Choice != "none") {
		return a.result, fmt.Errorf("Trae did not return the required tool call")
	}
	if a.result.Message.Content == "" && a.result.Reasoning == "" && len(a.calls) == 0 {
		return a.result, fmt.Errorf("Trae returned an empty response")
	}
	return a.result, nil
}

func normalizeEvent(s string) string {
	var b strings.Builder
	for _, c := range strings.ToLower(s) {
		if c >= 'a' && c <= 'z' {
			b.WriteRune(c)
		}
	}
	v := b.String()
	if v == "responsedone" || v == "streamdone" {
		return "done"
	}
	return v
}

func (a *ideAccumulator) send(d delta) error {
	if a.emit == nil {
		return nil
	}
	return a.emit(d)
}

func mergeSnapshot(old, next string) (string, string) {
	if next == "" || strings.HasPrefix(old, next) {
		return old, ""
	}
	if strings.HasPrefix(next, old) {
		return next, strings.TrimPrefix(next, old)
	}
	// IDE also emits standalone text fragments. Native tool arguments use a separate delta path.
	return old + next, next
}

func (a *ideAccumulator) frame(event, raw string) error {
	if raw == "[DONE]" {
		a.terminal = true
		return nil
	}
	var obj map[string]any
	if e := json.Unmarshal([]byte(raw), &obj); e != nil {
		return fmt.Errorf("invalid Trae SSE JSON")
	}
	if event == "" {
		event = apiFirstString(obj, "event", "event_type", "eventType", "type")
	}
	kind := normalizeEvent(event)
	if code := apiFirstString(obj, "code"); code != "" && code != "0" {
		if code == "401" || code == "403" || code == "1001" {
			return errCredentialRejected
		}
		return fmt.Errorf("Trae returned business error")
	}
	if kind == "error" || obj["error"] != nil {
		return fmt.Errorf("Trae returned an error event")
	}
	if kind == "heartbeat" || kind == "keepalive" || kind == "ping" {
		return a.send(delta{
			Heartbeat: true,
		})
	}
	if kind == "tokenusage" {
		v := apiObjectValue(obj["usage"])
		if v == nil {
			v = obj
		}
		a.result.Usage = parseUsage(v)
		return nil
	}
	if v := apiObjectValue(obj["usage"]); v != nil {
		a.result.Usage = parseUsage(v)
	}
	if f := apiFirstString(obj, "finish_reason", "finishReason", "stop_reason", "stopReason"); f != "" {
		switch f {
		case "length", "max_tokens":
			a.result.Finish = "length"
		case "content_filter":
			a.result.Finish = "content_filter"
		}
	}
	text := apiFirstString(obj, "response", "text", "content")
	reason := apiFirstString(obj, "reasoning_content", "reasoningContent", "thought", "thinking")
	if m := apiObjectValue(obj["message"]); m != nil {
		if text == "" {
			text = apiFirstString(m, "content", "text")
		}
		if reason == "" {
			reason = apiFirstString(m, "reasoning_content", "reasoningContent", "thought", "thinking")
		}
	}
	// Thinking embedded in a cumulative response is held until its closing tag.
	if strings.HasPrefix(text, "<think>") {
		if at := strings.Index(text, "</think>"); at >= 0 {
			reason = text[len("<think>"):at]
			text = text[at+len("</think>"):]
		} else {
			reason = strings.TrimPrefix(text, "<think>")
			text = ""
		}
	}
	var d string
	a.result.Message.Content, d = mergeSnapshot(a.result.Message.Content, text)
	if d != "" {
		if e := a.send(delta{
			Text: d,
		}); e != nil {
			return e
		}
	}
	a.result.Reasoning, d = mergeSnapshot(a.result.Reasoning, reason)
	if d != "" {
		if e := a.send(delta{
			Reasoning: d,
		}); e != nil {
			return e
		}
	}
	if len(a.result.Message.Content)+len(a.result.Reasoning) > 16<<20 {
		return fmt.Errorf("Trae response exceeds size limit")
	}
	if e := a.extractCalls(obj); e != nil {
		return e
	}
	if m := apiObjectValue(obj["message"]); m != nil {
		if e := a.extractCalls(m); e != nil {
			return e
		}
	}
	if kind == "done" {
		a.terminal = true
	}
	return nil
}

func (a *ideAccumulator) extractCalls(obj map[string]any) error {
	for _, key := range []string{"tool_calls", "tool_call", "function_call", "tool_call_info", "toolCallInfo"} {
		v, ok := obj[key]
		if !ok {
			continue
		}
		var records []any
		if xs, ok := v.([]any); ok {
			records = xs
		} else {
			records = []any{v}
		}
		for i, raw := range records {
			m := apiObjectValue(raw)
			if m == nil {
				return fmt.Errorf("invalid Trae tool event")
			}
			if key == "function_call" && m["function_call"] == nil {
				m = map[string]any{"function_call": m}
			}
			fallback := -1
			if len(records) > 1 {
				fallback = i
			}
			if e := a.addCall(m, fallback); e != nil {
				return e
			}
		}
	}
	return nil
}

func (a *ideAccumulator) addCall(m map[string]any, fallback int) error {
	fn := apiObjectValue(m["function_call"])
	native := fn != nil
	if fn == nil {
		fn = apiObjectValue(m["functionCall"])
		native = fn != nil
	}
	if fn == nil {
		fn = apiObjectValue(m["function"])
	}
	if fn == nil {
		fn = m
	}
	name := apiFirstString(fn, "name")
	if name == "" {
		name = apiFirstString(m, "name", "tool_name")
	}
	if name == "finish" || name == "done" || name == "final" {
		found := false
		for _, t := range a.r.Tools {
			found = found || t.Function.Name == name
		}
		if !found {
			return nil
		}
	}
	index := fallback
	if v, ok := m["index"].(float64); ok {
		if v < 0 || v > 10000 {
			return fmt.Errorf("invalid Trae tool index")
		}
		index = int(v)
	}
	id := apiFirstString(m, "id", "tool_call_id", "toolCallId")
	if id == "" {
		id = apiFirstString(fn, "id", "toolCallId")
	}
	slot, exists := a.byID[id]
	if indexed, ok := a.byIndex[index]; index >= 0 && ok {
		if exists && indexed != slot {
			return fmt.Errorf("conflicting Trae tool ID and index")
		}
		if !exists {
			slot, exists = indexed, true
			if id != "" && a.calls[slot].call.ID != id {
				return fmt.Errorf("Trae changed tool identity during a stream")
			}
		}
	}
	if !exists && id == "" && index < 0 && a.lastCall > 0 {
		slot, exists = a.lastCall-1, true
	}
	if !exists {
		slot = len(a.calls)
		if slot >= 128 {
			return fmt.Errorf("too many Trae tool calls")
		}
		if id == "" {
			id = newID("call_")
		}
		a.calls = append(a.calls, &accumulatedCall{
			call: toolCall{
				ID:   id,
				Type: "function",
			},
		})
		a.byID[id] = slot
	}
	if index >= 0 {
		a.byIndex[index] = slot
	}
	a.lastCall = slot + 1
	state := a.calls[slot]
	if name != "" {
		if state.call.Function.Name != "" && state.call.Function.Name != name {
			return fmt.Errorf("Trae changed tool name during a stream")
		}
		state.call.Function.Name = name
	}
	var value any
	hasArgs := false
	for _, key := range []string{"arguments", "args", "input", "params", "parameters"} {
		if v, ok := fn[key]; ok {
			value = v
			hasArgs = true
			break
		}
	}
	args := ""
	if hasArgs {
		if s, ok := value.(string); ok {
			args = s
		} else {
			args = jsonText(value)
		}
	}
	old := state.call.Function.Arguments
	if native {
		if _, ok := value.(string); ok {
			state.call.Function.Arguments += args
		} else if hasArgs {
			state.call.Function.Arguments = args
		}
	} else if hasArgs {
		state.call.Function.Arguments, _ = mergeSnapshot(old, args)
	}
	if len(state.call.Function.Arguments) > 4<<20 {
		return fmt.Errorf("tool arguments exceed size limit")
	}
	if state.call.Function.Name == "" {
		return nil
	}
	allowed := false
	for _, t := range a.r.Tools {
		allowed = allowed || t.Function.Name == state.call.Function.Name
	}
	if !allowed || a.r.Choice == "none" || (a.r.Choice != "auto" && a.r.Choice != "required" && a.r.Choice != state.call.Function.Name) {
		return fmt.Errorf("Trae requested an unavailable tool")
	}
	if a.r.Parallel != nil && !*a.r.Parallel && len(a.calls) > 1 {
		return fmt.Errorf("Trae returned parallel calls when disabled")
	}
	callDelta := toolCall{
		Function: function{
			Arguments: strings.TrimPrefix(state.call.Function.Arguments, state.sentArgs),
		},
	}
	if !strings.HasPrefix(state.call.Function.Arguments, state.sentArgs) {
		return fmt.Errorf("Trae rewrote streamed tool arguments")
	}
	if !state.sent {
		callDelta.ID = state.call.ID
		callDelta.Type = "function"
		callDelta.Function.Name = state.call.Function.Name
	}
	if !state.sent || callDelta.Function.Arguments != "" {
		if e := a.send(delta{
			Call:  &callDelta,
			Index: slot,
		}); e != nil {
			return e
		}
		state.sent = true
		state.sentArgs = state.call.Function.Arguments
	}
	return nil
}

func parseUsage(m map[string]any) *usage {
	n := func(keys ...string) int {
		for _, k := range keys {
			if v, ok := m[k].(float64); ok && v >= 0 {
				return int(v)
			}
		}
		return 0
	}
	u := &usage{
		Input:  n("prompt_tokens", "input_tokens", "input_token", "inputTokens"),
		Output: n("completion_tokens", "output_tokens", "output_token", "outputTokens"),
		Total:  n("total_tokens", "total_token", "totalTokens"),
	}
	if u.Total == 0 {
		u.Total = u.Input + u.Output
	}
	if v := n("cache_read_tokens", "cached_tokens", "cacheReadTokens"); v > 0 {
		u.InputDetails = &inputTokenDetails{
			CachedTokens: v,
		}
	}
	if v := n("reasoning_tokens", "reasoningTokens"); v > 0 {
		u.OutputDetails = &outputTokenDetails{
			ReasoningTokens: v,
		}
	}
	return u
}

var errCredentialRejected = errors.New("upstream rejected credential")

func (t *traeClient) prompt(ctx context.Context, c apiCredential, g uint64, r chatRequest, id string, onDelta func(delta) error) (completion, error) {
	result, e := t.promptAttempt(ctx, c, g, r, id, onDelta)
	if errors.Is(e, errCredentialRejected) {
		return result, errLoginRequired
	}
	return result, e
}
