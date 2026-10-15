package main

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
	"unicode"

	openai "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/packages/pagination"
	"github.com/tidwall/sjson"
)

const maxRequestBody = 4 << 20

func decodeBody(w http.ResponseWriter, r *http.Request) (json.RawMessage, map[string]json.RawMessage, error) {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBody)
	d := json.NewDecoder(r.Body)
	var body json.RawMessage
	if e := d.Decode(&body); e != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(e, &tooLarge) {
			return nil, nil, &apiError{
				413,
				"request body exceeds 4 MiB limit",
			}
		}
		return nil, nil, badRequest("invalid JSON request")
	}
	var raw map[string]json.RawMessage
	if e := json.Unmarshal(body, &raw); e != nil || raw == nil {
		return nil, nil, badRequest("request must be a JSON object")
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return nil, nil, badRequest("multiple JSON values")
	}
	for _, k := range []string{"reasoning_effort", "reasoning", "thinking"} {
		if v, ok := raw[k]; ok && string(v) != "null" && string(v) != "{}" {
			return nil, nil, badRequest(k + " is unsupported by the IDE upstream")
		}
	}
	if v, ok := raw["n"]; ok && string(v) != "1" {
		return nil, nil, badRequest("only n=1 is supported")
	}
	if v, ok := raw["response_format"]; ok && string(v) != "null" {
		var f struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(v, &f) != nil || f.Type != "text" {
			return nil, nil, badRequest("only text response_format is supported")
		}
	}
	return body, raw, nil
}
func (s *server) chat(w http.ResponseWriter, r *http.Request) {
	body, fields, e := decodeBody(w, r)
	if e != nil {
		writeError(w, statusForError(e), e.Error())
		return
	}
	req, e := decodeChatRequest(body, fields)
	if e != nil {
		writeError(w, 400, "invalid chat completion request: "+e.Error())
		return
	}
	id := newID("chatcmpl-")
	created := time.Now().Unix()
	var sink *streamSink
	model := req.Model
	var emit func(delta) error
	if req.Stream {
		sink = newStreamSink(w, r)
		defer sink.close()
		emit = func(d delta) error {
			if d.Start {
				model = d.Model
				return sink.send("", chatChunk(id, created, model, openai.ChatCompletionChunkChoiceDelta{
					Role: "assistant",
				}, "", nil))
			}
			if d.Heartbeat {
				return sink.heartbeat()
			}
			value := openai.ChatCompletionChunkChoiceDelta{
				Content: d.Text,
			}
			if d.Call != nil {
				call := openai.ChatCompletionChunkChoiceDeltaToolCall{
					Index: int64(d.Index),
					ID:    d.Call.ID,
					Function: openai.ChatCompletionChunkChoiceDeltaToolCallFunction{
						Name:      d.Call.Function.Name,
						Arguments: d.Call.Function.Arguments,
					},
				}
				if call.ID != "" {
					call.Type = "function"
				}
				value.ToolCalls = []openai.ChatCompletionChunkChoiceDeltaToolCall{call}
			}
			chunk := chatChunk(id, created, model, value, "", nil)
			if d.Reasoning == "" {
				return sink.send("", chunk)
			}
			body, err := marshalChatReasoning(chunk, d.Reasoning)
			if err != nil {
				return err
			}
			return sink.send("", body)
		}
	}
	effective, result, e := s.execute(r.Context(), req, emit)
	if e != nil {
		s.writeExecutionError(w, sink, e)
		return
	}
	if result.Usage == nil {
		result.Usage = estimatedUsage(effective, result)
		if sink == nil {
			w.Header().Set("X-Usage-Estimated", "true")
		}
	}
	if sink != nil {
		if e = sink.send("", chatChunk(id, created, effective.Model, openai.ChatCompletionChunkChoiceDelta{}, result.Finish, nil)); e != nil {
			return
		}
		if req.StreamOptions.IncludeUsage {
			if e = sink.send("", openai.ChatCompletionChunk{
				ID:      id,
				Object:  "chat.completion.chunk",
				Created: created,
				Model:   effective.Model,
				Choices: []openai.ChatCompletionChunkChoice{},
				Usage:   chatUsage(result.Usage),
			}); e != nil {
				return
			}
		}
		_ = sink.done()
		return
	}
	msg := openai.ChatCompletionMessage{
		Role:    "assistant",
		Content: result.Message.Content,
	}
	for _, call := range result.Message.ToolCalls {
		msg.ToolCalls = append(msg.ToolCalls, openai.ChatCompletionMessageToolCallUnion{
			ID:   call.ID,
			Type: "function",
			Function: openai.ChatCompletionMessageFunctionToolCallFunction{
				Name:      call.Function.Name,
				Arguments: call.Function.Arguments,
			},
		})
	}
	resultBody := openai.ChatCompletion{
		ID:      id,
		Object:  "chat.completion",
		Created: created,
		Model:   effective.Model,
		Choices: []openai.ChatCompletionChoice{{
			Index:        0,
			Message:      msg,
			FinishReason: result.Finish,
		}},
		Usage: chatUsage(result.Usage),
	}
	if result.Reasoning == "" {
		writeJSON(w, 200, resultBody)
		return
	}
	body, err := marshalChatReasoning(resultBody, result.Reasoning)
	if err != nil {
		writeError(w, 500, "could not encode chat completion")
		return
	}
	writeJSON(w, 200, body)
}
func chatChunk(id string, created int64, model string, d openai.ChatCompletionChunkChoiceDelta, finish string, u *usage) openai.ChatCompletionChunk {
	return openai.ChatCompletionChunk{
		ID:      id,
		Object:  "chat.completion.chunk",
		Created: created,
		Model:   model,
		Choices: []openai.ChatCompletionChunkChoice{{
			Index:        0,
			Delta:        d,
			FinishReason: finish,
		}},
		Usage: chatUsage(u),
	}
}
func (s *server) models(w http.ResponseWriter, r *http.Request) {
	c, e := readAPICredential()
	if e != nil {
		writeError(w, statusForError(e), e.Error())
		return
	}
	names, e := s.upstream.modelNames(r.Context(), c, snapshotIdentity(c))
	if e != nil {
		writeError(w, statusForError(e), e.Error())
		return
	}
	models := make([]openai.Model, 0, len(names))
	for _, name := range names {
		models = append(models, openai.Model{
			ID:      name,
			Object:  "model",
			Created: 0,
			OwnedBy: "trae",
		})
	}
	writeJSON(w, 200, pagination.Page[openai.Model]{
		Object: "list",
		Data:   models,
	})
}
func (s *server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		_, err := readAPICredential()
		writeJSON(w, 200, map[string]any{"status": "ok", "logged_in": err == nil})
	})
	mux.HandleFunc("GET /v1/models", s.models)
	mux.HandleFunc("POST /v1/chat/completions", s.chat)
	mux.HandleFunc("/v1/responses", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, 501, "Responses API is unsupported; use Chat Completions with full history")
	})
	return authMiddleware(s.cfg.APIToken, mux)
}
func authMiddleware(token string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if token == "" && (!localManagementHost(r.Host) || !sameOrigin(r)) {
			writeError(w, 403, "non-local or cross-origin management request denied")
			return
		}
		valid := token == "" || subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+token)) == 1
		if !valid {
			writeError(w, 401, "invalid API token")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// Never resolve a hostname here: a rebinding domain can resolve to loopback.
// Forwarded headers are untrusted when the local gateway has no API token.
func localManagementHost(host string) bool {
	u, err := url.Parse("http://" + host)
	if err != nil || u.Host != host || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return false
	}
	h := u.Hostname()
	if strings.EqualFold(h, "localhost") {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}
func sameOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return r.Header.Get("Sec-Fetch-Site") != "cross-site"
	}
	return origin == "http://"+r.Host || origin == "https://"+r.Host
}
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if e := json.NewEncoder(w).Encode(v); e != nil {
		slog.Debug("write response failed", "error", e)
	}
}
func writeError(w http.ResponseWriter, status int, detail string) {
	typ := "server_error"
	if status == 400 {
		typ = "invalid_request_error"
	}
	if status == 401 {
		typ = "authentication_error"
	}
	writeJSON(w, status, openAIErrorJSON{
		detail: openai.Error{
			Message: detail,
			Type:    typ,
		},
		includeCode: true,
	})
}
func (s *server) writeExecutionError(w http.ResponseWriter, sink *streamSink, e error) {
	if sink != nil && sink.hasStarted() {
		typ := "server_error"
		if statusForError(e) == http.StatusUnauthorized {
			typ = "authentication_error"
		}
		_ = sink.send("", openAIErrorJSON{
			detail: openai.Error{
				Message: e.Error(),
				Type:    typ,
			},
		})
		if typ == "authentication_error" {
			sink.finish()
			return
		}
		_ = sink.done()
		return
	}
	writeError(w, statusForError(e), e.Error())
}
func debugHeaders(h http.Header) map[string][]string {
	out := map[string][]string{}
	for k, v := range h {
		lower := strings.ToLower(k)
		if strings.Contains(lower, "token") || strings.Contains(lower, "authorization") || strings.Contains(lower, "cookie") || strings.Contains(lower, "api-key") {
			out[k] = []string{"<redacted>"}
		} else {
			out[k] = append([]string(nil), v...)
		}
	}
	return out
}
func estimateTokens(text string) int {
	count := 0
	word := false
	for _, r := range text {
		if unicode.IsSpace(r) {
			word = false
		} else if unicode.Is(unicode.Han, r) || unicode.IsPunct(r) {
			count++
			word = false
		} else if !word {
			count++
			word = true
		}
	}
	return count
}
func estimatedUsage(r chatRequest, c completion) *usage {
	in := estimateTokens(jsonText(r.Messages) + jsonText(r.Tools))
	out := estimateTokens(c.Message.Content + c.Reasoning + jsonText(c.Message.ToolCalls))
	return &usage{
		Input:  in,
		Output: out,
		Total:  in + out,
	}
}

// All writes, including timed heartbeats, use the same mutex. The heartbeat
// goroutine is joined before the handler returns or writes a terminal frame.
type streamSink struct {
	mu       sync.Mutex
	w        http.ResponseWriter
	r        *http.Request
	started  bool
	finished bool
	err      error
	stop     chan struct{}
	doneCh   chan struct{}
	once     sync.Once
}

func newStreamSink(w http.ResponseWriter, r *http.Request) *streamSink {
	s := &streamSink{
		w:      w,
		r:      r,
		stop:   make(chan struct{}),
		doneCh: make(chan struct{}),
	}
	go func() {
		defer close(s.doneCh)
		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-s.stop:
				return
			case <-r.Context().Done():
				return
			case <-ticker.C:
				_ = s.heartbeat()
			}
		}
	}()
	return s
}
func (s *streamSink) hasStarted() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.started
}
func (s *streamSink) writeLocked(event string, v any) error {
	if s.err != nil {
		return s.err
	}
	if s.finished {
		return fmt.Errorf("stream already finished")
	}
	if e := s.r.Context().Err(); e != nil {
		return e
	}
	flusher, ok := s.w.(http.Flusher)
	if !ok {
		return fmt.Errorf("streaming unsupported")
	}
	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("encode stream event: %w", err)
	}
	if !s.started {
		s.w.Header().Set("Content-Type", "text/event-stream")
		s.w.Header().Set("Cache-Control", "no-cache")
		s.w.Header().Set("X-Accel-Buffering", "no")
		s.started = true
	}
	_ = http.NewResponseController(s.w).SetWriteDeadline(time.Now().Add(30 * time.Second))
	if event != "" {
		_, s.err = fmt.Fprintf(s.w, "event: %s\n", event)
	}
	if s.err == nil {
		_, s.err = fmt.Fprintf(s.w, "data: %s\n\n", data)
	}
	if s.err == nil {
		flusher.Flush()
	}
	return s.err
}
func (s *streamSink) send(event string, v any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.writeLocked(event, v)
}
func (s *streamSink) heartbeat() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.started || s.finished || s.err != nil {
		return s.err
	}
	_ = http.NewResponseController(s.w).SetWriteDeadline(time.Now().Add(30 * time.Second))
	_, s.err = io.WriteString(s.w, ": keepalive\n\n")
	if s.err == nil {
		s.w.(http.Flusher).Flush()
	}
	return s.err
}
func (s *streamSink) done() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return s.err
	}
	s.finished = true
	_ = http.NewResponseController(s.w).SetWriteDeadline(time.Now().Add(30 * time.Second))
	_, s.err = io.WriteString(s.w, "data: [DONE]\n\n")
	if s.err == nil {
		s.w.(http.Flusher).Flush()
	}
	return s.err
}
func (s *streamSink) finish() {
	s.mu.Lock()
	s.finished = true
	s.mu.Unlock()
}
func (s *streamSink) close() {
	s.once.Do(func() {
		close(s.stop)
		<-s.doneCh
	})
}

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
