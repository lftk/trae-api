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
	routes           map[string]modelRoute
}
type modelRoute struct {
	function, name, base string
	most, window         int
}

var chatFunctions = []string{"chat_v3", "solo_work_lite", "solo_agent", "solo_agent_lite"}

func (t *traeClient) headers(c credential) http.Header {
	h := http.Header{}
	values := map[string]string{"Content-Type": "application/json", "Accept": "text/event-stream", "Authorization": "Cloud-IDE-JWT " + c.Token, "X-Cloudide-Token": c.Token, "x-ide-token": c.Token, "x-uid": c.UserID, "x-app-id": "6eefa01c-1036-4c7e-9ca5-d891f63bfcd8", "x-device-id": c.DeviceID, "x-machine-id": c.MachineID, "x-request-id": newID(""), "x-ide-version": "0.1.69", "x-ide-version-code": "20260917", "x-ide-version-type": "stable", "x-app-version": "0.1.69", "x-app-version-code": "20260917", "x-version-code": "20260917", "x-device-brand": "ASUS TUF Gaming A15 FA507RM_FA507RM", "x-device-type": "windows", "x-device-platform": "windows", "x-os-version": "Windows 10 Pro", "package-type": "stable_cn", "x-lscbd-aid": "787976", "x-lscbd-platform": "windows", "x-ss-dp": "787976", "x-plugin-channel": "icube-ai", "app-version": "0.1.69", "x-bridge-transport": "aha", "x-ahanet-timeout": "86400", "x-lgw-req-sdk-type": "3", "x-net-sdk-domain-dispatch": "1", "x-ttnet-bypass-decompression": "1", "x-ttnet-bypass-cookie": "0", "request-traffic-type": "prod", "User-Agent": "Trae/0.1.69"}
	for k, v := range values {
		h.Set(k, v)
	}
	return h
}
func (t *traeClient) modelNamesAttempt(ctx context.Context, c credential, g uint64) ([]string, error) {
	t.mu.Lock()
	if len(t.models) > 0 && t.modelsGeneration == g && time.Since(t.modelsAt) < 5*time.Minute {
		names := append([]string(nil), t.models...)
		t.mu.Unlock()
		return names, nil
	}
	t.mu.Unlock()
	lists := map[string][]map[string]any{}
	root, err := t.modelJSON(ctx, c, "/api/ide/v1/batch_get_detail_param", map[string]any{"functions": chatFunctions, "agent_type": "", "current_config_info": map[string]any{"config_name": "", "is_custom_model": false}, "mode_type": 0, "access_type": 0, "ab_force_vids": "", "ab_autotest_advanced_mode": 0, "show_custom_model": true})
	if errors.Is(err, errCredentialRejected) {
		return nil, err
	}
	if err == nil {
		for _, group := range records(field(root, "function_configs")) {
			fn := firstString(group, "function")
			if chatFunction(fn) {
				lists[fn] = append(lists[fn], records(group["config_info_list"])...)
			}
		}
	}
	hasBatchModels := false
	for _, list := range lists {
		for _, m := range list {
			if validChatModel(m) {
				hasBatchModels = true
				break
			}
		}
	}
	if !hasBatchModels {
		var firstErr error
		for _, fn := range chatFunctions {
			one, e := t.modelJSON(ctx, c, "/api/ide/v1/get_detail_param", map[string]any{"function": fn, "config_names": nil, "need_prompt": false, "current_config_info": nil, "poly_prompt": true, "mode_type": nil, "agent_type": nil})
			if errors.Is(e, errCredentialRejected) {
				return nil, e
			}
			if e != nil {
				if firstErr == nil {
					firstErr = e
				}
				continue
			}
			lists[fn] = records(field(one, "config_info_list"))
		}
		if len(lists) == 0 && firstErr != nil {
			return nil, firstErr
		}
	}
	routes := map[string]modelRoute{}
	entries := map[string][]struct {
		fn string
		m  map[string]any
	}{}
	for _, fn := range chatFunctions {
		for _, m := range lists[fn] {
			if validChatModel(m) {
				id := firstString(m, "config_name")
				entries[id] = append(entries[id], struct {
					fn string
					m  map[string]any
				}{fn, m})
			}
		}
	}
	var names []string
	for id, group := range entries {
		chosen := group[0]
		for _, candidate := range group {
			if devDetail(candidate.m) != nil {
				chosen = candidate
				break
			}
		}
		route := modelRoute{function: chosen.fn, base: id}
		if dev := devDetail(chosen.m); dev != nil {
			route.name = firstString(dev, "model_name")
			route.most = positiveInt(dev["max_tokens"])
		}
		routes[id] = route
		names = append(names, id)
		if _, separateModel := entries[id+"-max"]; separateModel {
			continue
		}
		for _, entry := range group {
			if max := detailWithSuffix(entry.m, "__max"); max != nil {
				window := 0
				devWindow := 0
				for _, source := range append([]struct {
					fn string
					m  map[string]any
				}{entry}, group...) {
					windowInfo := objectValue(source.m["context_window_tokens"])
					if window == 0 {
						window = positiveInt(windowInfo["max"])
					}
					if devWindow == 0 {
						devWindow = positiveInt(windowInfo["dev"])
					}
				}
				if window > devWindow && window > 0 {
					routes[id+"-max"] = modelRoute{function: entry.fn, base: id, name: firstString(max, "model_name"), most: positiveInt(max["max_tokens"]), window: window}
					names = append(names, id+"-max")
					break
				}
			}
		}
	}
	sort.Strings(names)
	if len(names) == 0 {
		return nil, fmt.Errorf("Trae returned no available models")
	}
	// Keep the account lock through publication so a late A response cannot
	// replace B's route table after B has logged in.
	t.auth.mu.Lock()
	if t.auth.generation != g || t.auth.cred.Token == "" {
		t.auth.mu.Unlock()
		return nil, errLoginRequired
	}
	t.mu.Lock()
	t.models = append([]string(nil), names...)
	t.modelsAt = time.Now()
	t.modelsGeneration = g
	t.routes = routes
	t.mu.Unlock()
	t.auth.mu.Unlock()
	return names, nil
}
func field(m map[string]any, key string) any {
	if v, ok := m[key]; ok {
		return v
	}
	return objectValue(m["data"])[key]
}
func records(v any) []map[string]any {
	xs, _ := v.([]any)
	out := make([]map[string]any, 0, len(xs))
	for _, x := range xs {
		if m := objectValue(x); m != nil {
			out = append(out, m)
		}
	}
	return out
}
func positiveInt(v any) int {
	n, _ := v.(float64)
	if n > 0 && n < 1<<30 {
		return int(n)
	}
	return 0
}
func chatFunction(fn string) bool {
	for _, allowed := range chatFunctions {
		if fn == allowed {
			return true
		}
	}
	return false
}
func validChatModel(m map[string]any) bool {
	id := firstString(m, "config_name")
	if id == "" || strings.HasPrefix(strings.ToLower(id), "custom_model") || m["config_switch"] == false || m["is_custom_model"] == true || objectValue(m["display_config"])["is_custom_model"] == true {
		return false
	}
	usage := firstString(m, "usage")
	return usage == "" || usage == "chat_completion"
}
func detailWithSuffix(m map[string]any, suffix string) map[string]any {
	for _, d := range records(m["model_detail_list"]) {
		if strings.HasSuffix(firstString(d, "model_name"), suffix) {
			return d
		}
	}
	return nil
}
func devDetail(m map[string]any) map[string]any { return detailWithSuffix(m, "__dev") }
func (t *traeClient) modelJSON(ctx context.Context, c credential, path string, body any) (map[string]any, error) {
	req, e := http.NewRequestWithContext(ctx, "POST", strings.TrimRight(t.cfg.BaseURL, "/")+path, bytes.NewBufferString(jsonText(body)))
	if e != nil {
		return nil, e
	}
	req.Header = t.headers(c)
	req.Header.Set("Accept", "application/json")
	resp, e := t.http.Do(req)
	if e != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("Trae model list connection failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode == 401 || resp.StatusCode == 403 {
		return nil, errCredentialRejected
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("Trae model list returned HTTP %d", resp.StatusCode)
	}
	var root map[string]any
	if e = json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&root); e != nil {
		return nil, fmt.Errorf("invalid Trae model list response")
	}
	if code := firstString(root, "code"); code != "" && code != "0" {
		if code == "1001" || code == "401" {
			return nil, errCredentialRejected
		}
		return nil, fmt.Errorf("Trae model list business error")
	}
	return root, nil
}
func buildIDEBody(r chatRequest, sessionID string) map[string]any {
	return buildRoutedIDEBody(r, sessionID, modelRoute{function: "solo_work_lite", base: r.Model})
}
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
func (t *traeClient) resolveRoute(ctx context.Context, c credential, g uint64, model string) (modelRoute, error) {
	t.mu.Lock()
	route, ok := t.routes[model]
	valid := t.modelsGeneration == g && time.Since(t.modelsAt) < 5*time.Minute
	t.mu.Unlock()
	if !ok || !valid {
		// Explicit models need their function and native model name too. Discovery
		// failures are surfaced rather than silently routing through another mode.
		if _, err := t.modelNames(ctx, c, g); err != nil {
			return modelRoute{}, err
		}
		t.mu.Lock()
		route, ok = t.routes[model]
		t.mu.Unlock()
		if !ok {
			return modelRoute{}, badRequest("model is not available for this account")
		}
	}
	if !t.auth.validGeneration(g) {
		return modelRoute{}, errLoginRequired
	}
	return route, nil
}
func (t *traeClient) promptAttempt(ctx context.Context, c credential, g uint64, r chatRequest, id string, onDelta func(delta) error) (completion, error) {
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
