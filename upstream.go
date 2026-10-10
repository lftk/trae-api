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
	"sort"
	"strings"
	"sync"
	"time"
)

type traeClient struct {
	cfg              config
	http             *http.Client
	auth             *authManager
	mu               sync.Mutex
	models           []string
	modelsAt         time.Time
	modelsGeneration uint64
}

func (t *traeClient) headers(c credential) http.Header {
	h := http.Header{}
	values := map[string]string{"Content-Type": "application/json", "Accept": "text/event-stream", "Authorization": "Cloud-IDE-JWT " + c.Token, "X-Cloudide-Token": c.Token, "x-ide-token": c.Token, "x-uid": c.UserID, "x-app-id": "6eefa01c-1036-4c7e-9ca5-d891f63bfcd8", "x-device-id": c.DeviceID, "x-machine-id": c.MachineID, "x-request-id": newID(""), "x-ide-version": t.cfg.IDEVersion, "x-ide-version-code": t.cfg.IDEVersionCode, "x-ide-version-type": "stable", "x-app-version": "default", "x-app-version-code": t.cfg.IDEVersionCode, "x-version-code": t.cfg.IDEVersionCode, "x-device-brand": "83DG", "x-device-type": "windows", "x-device-platform": "windows", "x-os-version": "Windows 11 Pro", "package-type": "stable_cn", "x-lscbd-aid": "787976", "x-lscbd-platform": "windows", "x-ss-dp": "787976", "x-plugin-channel": "icube-ai", "app-version": t.cfg.IDEVersion, "x-bridge-transport": "aha", "x-ahanet-timeout": "86400", "x-lgw-req-sdk-type": "3", "x-net-sdk-domain-dispatch": "1", "x-ttnet-bypass-decompression": "1", "x-ttnet-bypass-cookie": "0", "request-traffic-type": "prod", "User-Agent": "Trae/" + t.cfg.IDEVersion}
	for k, v := range values {
		h.Set(k, v)
	}
	return h
}
func (t *traeClient) modelNamesAttempt(ctx context.Context, c credential, g uint64) ([]string, error) {
	t.mu.Lock()
	if t.modelsGeneration == g && time.Since(t.modelsAt) < 5*time.Minute {
		names := append([]string(nil), t.models...)
		t.mu.Unlock()
		return names, nil
	}
	t.mu.Unlock()
	endpoint := strings.TrimRight(t.cfg.BaseURL, "/") + "/api/remote/v1/models?functions=solo_agent_remote%2Csolo_work_remote%2Csolo_design_remote&show_custom_model=true"
	req, e := http.NewRequestWithContext(ctx, "GET", endpoint, nil)
	if e != nil {
		return nil, e
	}
	req.Header = t.headers(c)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Origin", "https://solo.trae.cn")
	resp, e := t.http.Do(req)
	if e != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("Trae model list connection failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		if resp.StatusCode == 401 || resp.StatusCode == 403 {
			return nil, errCredentialRejected
		}
		return nil, fmt.Errorf("Trae model list returned HTTP %d", resp.StatusCode)
	}
	var root struct {
		Code int `json:"code"`
		Data struct {
			List []struct {
				Models []struct {
					Name string `json:"name"`
				} `json:"models"`
			} `json:"list"`
		} `json:"data"`
	}
	if e = json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&root); e != nil || root.Code != 0 {
		return nil, fmt.Errorf("invalid Trae model list response")
	}
	seen := map[string]bool{}
	var names []string
	for _, group := range root.Data.List {
		for _, m := range group.Models {
			if m.Name != "" && !seen[m.Name] {
				names = append(names, m.Name)
				seen[m.Name] = true
			}
		}
	}
	sort.Strings(names)
	if len(names) == 0 {
		return nil, fmt.Errorf("Trae returned no available models")
	}
	t.mu.Lock()
	t.models = append([]string(nil), names...)
	t.modelsAt = time.Now()
	t.modelsGeneration = g
	t.mu.Unlock()
	return names, nil
}
func buildIDEBody(r chatRequest, sessionID string) map[string]any {
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
				calls = append(calls, map[string]any{"id": c.ID, "type": "function", "function_call": c.Function})
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
	body := map[string]any{"messages": messages, "config_name": r.Model, "model": r.Model, "function": "solo_work_lite", "stream": true, "request_id": sessionID, "session_id": sessionID}
	limit := r.MaxCompletionTokens
	if limit == 0 {
		limit = r.MaxTokens
	}
	if limit > 0 {
		body["max_tokens"] = limit
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
			fn := map[string]any{"name": t.Function.Name, "description": t.Function.Description, "parameters": string(t.Function.Parameters)}
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
func (t *traeClient) promptAttempt(ctx context.Context, c credential, g uint64, r chatRequest, id string, onDelta func(delta) error) (completion, error) {
	endpoint := strings.TrimRight(t.cfg.BaseURL, "/") + "/api/agent/v3/llm_utils_chat"
	req, e := http.NewRequestWithContext(ctx, "POST", endpoint, bytes.NewBufferString(jsonText(buildIDEBody(r, id))))
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
	a := ideAccumulator{result: completion{Message: message{Role: "assistant"}, Finish: "stop"}, byID: map[string]int{}, byIndex: map[int]int{}, r: r, emit: emit}
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
			if e := a.send(delta{Heartbeat: true}); e != nil {
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
			if e := a.send(delta{Index: len(a.result.Message.ToolCalls), Call: &toolCall{Function: function{Arguments: "{}"}}}); e != nil {
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
func mergeSnapshot(old, next string) (string, string, error) {
	if next == "" || strings.HasPrefix(old, next) {
		return old, "", nil
	}
	if strings.HasPrefix(next, old) {
		return next, strings.TrimPrefix(next, old), nil
	}
	// IDE also emits standalone text fragments. Native tool arguments use a separate delta path.
	return old + next, next, nil
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
		event = firstString(obj, "event", "event_type", "eventType", "type")
	}
	kind := normalizeEvent(event)
	if kind == "error" || obj["error"] != nil {
		return fmt.Errorf("Trae returned an error event")
	}
	if code, ok := obj["code"].(float64); ok && code != 0 {
		return fmt.Errorf("Trae returned business error %d", int(code))
	}
	if kind == "heartbeat" || kind == "keepalive" || kind == "ping" {
		return a.send(delta{Heartbeat: true})
	}
	if kind == "tokenusage" {
		v := objectValue(obj["usage"])
		if v == nil {
			v = obj
		}
		a.result.Usage = parseUsage(v)
		return nil
	}
	if v := objectValue(obj["usage"]); v != nil {
		a.result.Usage = parseUsage(v)
	}
	if f := firstString(obj, "finish_reason", "finishReason", "stop_reason", "stopReason"); f != "" {
		switch f {
		case "length", "max_tokens":
			a.result.Finish = "length"
		case "content_filter":
			a.result.Finish = "content_filter"
		}
	}
	text := firstString(obj, "response", "text", "content")
	reason := firstString(obj, "reasoning_content", "reasoningContent", "thought", "thinking")
	if m := objectValue(obj["message"]); m != nil {
		if text == "" {
			text = firstString(m, "content", "text")
		}
		if reason == "" {
			reason = firstString(m, "reasoning_content", "reasoningContent", "thought", "thinking")
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
	a.result.Message.Content, d, _ = mergeSnapshot(a.result.Message.Content, text)
	if d != "" {
		if e := a.send(delta{Text: d}); e != nil {
			return e
		}
	}
	a.result.Reasoning, d, _ = mergeSnapshot(a.result.Reasoning, reason)
	if d != "" {
		if e := a.send(delta{Reasoning: d}); e != nil {
			return e
		}
	}
	if len(a.result.Message.Content)+len(a.result.Reasoning) > 16<<20 {
		return fmt.Errorf("Trae response exceeds size limit")
	}
	if e := a.extractCalls(obj); e != nil {
		return e
	}
	if m := objectValue(obj["message"]); m != nil {
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
			m := objectValue(raw)
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
	fn := objectValue(m["function_call"])
	native := fn != nil
	if fn == nil {
		fn = objectValue(m["functionCall"])
		native = fn != nil
	}
	if fn == nil {
		fn = objectValue(m["function"])
	}
	if fn == nil {
		fn = m
	}
	name := firstString(fn, "name")
	if name == "" {
		name = firstString(m, "name", "tool_name")
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
	id := firstString(m, "id", "tool_call_id", "toolCallId")
	if id == "" {
		id = firstString(fn, "id", "toolCallId")
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
		a.calls = append(a.calls, &accumulatedCall{call: toolCall{ID: id, Type: "function"}})
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
		state.call.Function.Arguments, _, _ = mergeSnapshot(old, args)
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
	callDelta := toolCall{Function: function{Arguments: strings.TrimPrefix(state.call.Function.Arguments, state.sentArgs)}}
	if !strings.HasPrefix(state.call.Function.Arguments, state.sentArgs) {
		return fmt.Errorf("Trae rewrote streamed tool arguments")
	}
	if !state.sent {
		callDelta.ID = state.call.ID
		callDelta.Type = "function"
		callDelta.Function.Name = state.call.Function.Name
	}
	if !state.sent || callDelta.Function.Arguments != "" {
		if e := a.send(delta{Call: &callDelta, Index: slot}); e != nil {
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
	u := &usage{Input: n("prompt_tokens", "input_tokens", "input_token", "inputTokens"), Output: n("completion_tokens", "output_tokens", "output_token", "outputTokens"), Total: n("total_tokens", "total_token", "totalTokens")}
	if u.Total == 0 {
		u.Total = u.Input + u.Output
	}
	if v := n("cache_read_tokens", "cached_tokens", "cacheReadTokens"); v > 0 {
		u.InputDetails = &inputTokenDetails{CachedTokens: v}
	}
	if v := n("reasoning_tokens", "reasoningTokens"); v > 0 {
		u.OutputDetails = &outputTokenDetails{ReasoningTokens: v}
	}
	return u
}

var errCredentialRejected = errors.New("upstream rejected credential")

func (t *traeClient) modelNames(ctx context.Context, c credential, g uint64) ([]string, error) {
	names, e := t.modelNamesAttempt(ctx, c, g)
	if errors.Is(e, errCredentialRejected) {
		fresh, newGeneration, err := t.auth.refreshRejected(ctx, c.Token, g)
		if err != nil {
			return nil, err
		}
		if newGeneration != g || !t.auth.validGeneration(g) {
			return nil, errLoginRequired
		}
		c, g = fresh, newGeneration
		names, e = t.modelNamesAttempt(ctx, c, g)
	}
	if errors.Is(e, errCredentialRejected) {
		t.auth.invalidate(g, c.Token)
		return nil, errLoginRequired
	}
	return names, e
}
func (t *traeClient) prompt(ctx context.Context, c credential, g uint64, r chatRequest, id string, onDelta func(delta) error) (completion, error) {
	result, e := t.promptAttempt(ctx, c, g, r, id, onDelta)
	if errors.Is(e, errCredentialRejected) {
		fresh, newGeneration, err := t.auth.refreshRejected(ctx, c.Token, g)
		if err != nil {
			return result, err
		}
		if newGeneration != g || !t.auth.validGeneration(g) {
			return result, errLoginRequired
		}
		c, g = fresh, newGeneration
		result, e = t.promptAttempt(ctx, c, g, r, id, onDelta)
	}
	if errors.Is(e, errCredentialRejected) {
		t.auth.invalidate(g, c.Token)
		return result, errLoginRequired
	}
	return result, e
}
