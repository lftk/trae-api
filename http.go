package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
	"unicode"

	acp "github.com/coder/acp-go-sdk"
	"github.com/google/uuid"
	openai "github.com/sashabaranov/go-openai"
)

const (
	legacySessionIDHeader     = "X-Session-ID"
	claudeCodeSessionIDHeader = "X-Claude-Code-Session-Id"
)

func (s *server) chat(w http.ResponseWriter, r *http.Request) {
	started := time.Now()
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var req openai.ChatCompletionRequest
	decoder := json.NewDecoder(r.Body)
	if err := decoder.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid chat completion request: "+err.Error())
		return
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		writeError(w, http.StatusBadRequest, "invalid chat completion request: multiple JSON values")
		return
	}
	if len(req.Messages) == 0 {
		writeError(w, http.StatusBadRequest, "invalid chat completion request: messages is required")
		return
	}
	if err := validateMessages(req.Messages); err != nil {
		writeError(w, http.StatusBadRequest, "invalid chat completion request: "+err.Error())
		return
	}
	externalID := requestSessionID(r)
	lease, err := s.acquireSession(r.Context(), externalID)
	if err != nil {
		status := http.StatusBadGateway
		if errors.Is(err, errProcessLimit) || errors.Is(err, errSessionLimit) {
			status = http.StatusServiceUnavailable
		}
		writeError(w, status, err.Error())
		return
	}
	defer lease.release()
	session := lease.session
	session.mu.Lock()
	defer session.mu.Unlock()
	selectedModel, err := session.setModel(r.Context(), req.Model)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if selectedModel != "" {
		req.Model = selectedModel
	}
	prompt := formatPrompt(req.Messages)
	completionID := "chatcmpl-" + uuid.NewString()
	slog.Debug("chat request", "method", r.Method, "path", r.URL.Path, "headers", debugHeaders(r.Header), "external_sessionid", externalID, "acp_sessionid", session.sessionID(), "completionid", completionID, "model", req.Model, "stream", req.Stream, "prompt", prompt)
	if req.Stream {
		s.streamChat(w, r, session, completionID, externalID, req.Model, prompt)
		return
	}
	answer, reasoning, usage, sentPrompt, err := session.prompt(r.Context(), prompt, nil)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	if usage == nil {
		w.Header().Set("X-Usage-Estimated", "true")
	}
	if externalID != "" {
		w.Header().Set("X-Session-ID", externalID)
	}
	response := openai.ChatCompletionResponse{
		ID:      completionID,
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   req.Model,
		Choices: []openai.ChatCompletionChoice{{
			Index: 0,
			Message: openai.ChatCompletionMessage{
				Role:             openai.ChatMessageRoleAssistant,
				Content:          answer,
				ReasoningContent: reasoning,
			},
			FinishReason: openai.FinishReasonStop,
		}},
		Usage: openAIUsage(usage, sentPrompt, answer, reasoning),
	}
	writeJSONOrLog(w, http.StatusOK, response)
	slog.Debug("chat response", "completionid", completionID, "external_sessionid", externalID, "acp_sessionid", session.sessionID(), "answer", answer, "reasoning", reasoning, "usage", response.Usage)
	slog.Info("chat completed", "completionid", completionID, "sessionid", externalID, "acpsessionid", session.sessionID(), "elapsed", time.Since(started))
}

func requestSessionID(r *http.Request) string {
	if id := r.Header.Get(legacySessionIDHeader); id != "" {
		return id
	}
	return r.Header.Get(claudeCodeSessionIDHeader)
}

func (s *server) streamChat(
	w http.ResponseWriter,
	r *http.Request,
	session *session,
	completionID, externalID, model, prompt string,
) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	if externalID != "" {
		w.Header().Set("X-Session-ID", externalID)
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming is not supported")
		return
	}
	var streamErr error
	var sawAnswer bool
	tryWrite := func(value any) bool {
		if streamErr != nil {
			return false
		}
		if err := writeSSE(w, value); err != nil {
			streamErr = err
			return false
		}
		return true
	}
	initial := openai.ChatCompletionStreamResponse{
		ID:      completionID,
		Object:  "chat.completion.chunk",
		Created: time.Now().Unix(),
		Model:   model,
		Choices: []openai.ChatCompletionStreamChoice{{
			Index: 0,
			Delta: openai.ChatCompletionStreamChoiceDelta{
				Role: openai.ChatMessageRoleAssistant,
			},
			FinishReason: openai.FinishReasonNull,
		}},
	}
	if !tryWrite(initial) {
		slog.Error("write chat completion stream", "completionid", completionID, "sessionid", externalID, "acpsessionid", session.sessionID(), "error", streamErr)
		return
	}
	flusher.Flush()
	answer, reasoning, usage, sentPrompt, err := session.prompt(r.Context(), prompt, func(item update) {
		if streamErr != nil {
			return
		}
		if !item.Reasoning {
			sawAnswer = sawAnswer || item.Text != ""
		}
		chunk := openai.ChatCompletionStreamResponse{
			ID:      completionID,
			Object:  "chat.completion.chunk",
			Created: time.Now().Unix(),
			Model:   model,
			Choices: []openai.ChatCompletionStreamChoice{{
				Index:        0,
				Delta:        openai.ChatCompletionStreamChoiceDelta{},
				FinishReason: openai.FinishReasonNull,
			}},
		}
		if item.Reasoning {
			chunk.Choices[0].Delta.ReasoningContent = item.Text
		} else {
			chunk.Choices[0].Delta.Content = item.Text
		}
		if tryWrite(chunk) {
			flusher.Flush()
		}
	})
	slog.Debug("stream response", "completionid", completionID, "external_sessionid", externalID, "acp_sessionid", session.sessionID(), "answer", answer, "reasoning", reasoning, "usage", usage, "error", err)
	if streamErr != nil {
		slog.Error("write chat completion stream", "completionid", completionID, "sessionid", externalID, "acpsessionid", session.sessionID(), "error", streamErr)
		return
	}
	if err != nil {
		tryWrite(openai.ErrorResponse{
			Error: &openai.APIError{
				Message: err.Error(),
				Type:    "server_error",
			},
		})
	} else {
		if usage == nil {
			w.Header().Set("X-Usage-Estimated", "true")
		}
		// If ACP supplied only thought chunks, prompt() converts them to the
		// answer after the stream callback has already run. Send that fallback
		// answer now so a proxy/client does not finish with an empty response.
		if !sawAnswer && answer != "" {
			tryWrite(openai.ChatCompletionStreamResponse{
				ID:      completionID,
				Object:  "chat.completion.chunk",
				Created: time.Now().Unix(),
				Model:   model,
				Choices: []openai.ChatCompletionStreamChoice{{
					Index: 0,
					Delta: openai.ChatCompletionStreamChoiceDelta{
						Content: answer,
					},
					FinishReason: openai.FinishReasonNull,
				}},
			})
		}
		final := openai.ChatCompletionStreamResponse{
			ID:      completionID,
			Object:  "chat.completion.chunk",
			Created: time.Now().Unix(),
			Model:   model,
			Choices: []openai.ChatCompletionStreamChoice{{
				Index:        0,
				Delta:        openai.ChatCompletionStreamChoiceDelta{},
				FinishReason: openai.FinishReasonStop,
			}},
		}
		if usage != nil {
			final.Usage = openAIUsagePtr(usage, sentPrompt, answer, reasoning)
		} else {
			estimated := openAIUsage(usage, sentPrompt, answer, reasoning)
			final.Usage = &estimated
		}
		if tryWrite(final) {
			_, streamErr = io.WriteString(w, "data: [DONE]\n\n")
		}
	}
	if streamErr != nil {
		slog.Error("write chat completion stream", "completionid", completionID, "sessionid", externalID, "acpsessionid", session.sessionID(), "error", streamErr)
		return
	}
	flusher.Flush()
}

func debugHeaders(headers http.Header) map[string][]string {
	result := make(map[string][]string, len(headers))
	for name, values := range headers {
		if strings.EqualFold(name, "Authorization") ||
			strings.EqualFold(name, "Cookie") ||
			strings.EqualFold(name, "Set-Cookie") ||
			strings.EqualFold(name, "X-API-Key") {
			result[name] = []string{"<redacted>"}
			continue
		}
		result[name] = append([]string(nil), values...)
	}
	return result
}

func openAIUsage(usage *acp.Usage, prompt, answer, reasoning string) openai.Usage {
	if usage == nil {
		promptTokens := estimateTokens(prompt)
		completionTokens := estimateTokens(answer + reasoning)
		return openai.Usage{
			PromptTokens:     promptTokens,
			CompletionTokens: completionTokens,
			TotalTokens:      promptTokens + completionTokens,
		}
	}
	result := openai.Usage{
		PromptTokens:     usage.InputTokens,
		CompletionTokens: usage.OutputTokens,
		TotalTokens:      usage.TotalTokens,
	}
	if usage.CachedReadTokens != nil || usage.CachedWriteTokens != nil {
		result.PromptTokensDetails = &openai.PromptTokensDetails{}
		if usage.CachedReadTokens != nil {
			result.PromptTokensDetails.CachedTokens = *usage.CachedReadTokens
		}
	}
	if usage.ThoughtTokens != nil {
		result.CompletionTokensDetails = &openai.CompletionTokensDetails{
			ReasoningTokens: *usage.ThoughtTokens,
		}
	}
	return result
}

func openAIUsagePtr(usage *acp.Usage, prompt, answer, reasoning string) *openai.Usage {
	if usage == nil {
		return nil
	}
	result := openAIUsage(usage, prompt, answer, reasoning)
	return &result
}

// estimateTokens is a provider-independent fallback for ACP agents that do
// not return usage. It is intentionally conservative and is marked on the
// response with X-Usage-Estimated; exact counts require the model tokenizer.
func estimateTokens(text string) int {
	count := 0
	inWord := false
	for _, r := range text {
		switch {
		case unicode.IsSpace(r):
			inWord = false
		case unicode.Is(unicode.Han, r) || unicode.IsPunct(r) || unicode.IsSymbol(r):
			if inWord {
				count++
				inWord = false
			}
			count++
		default:
			inWord = true
		}
	}
	if inWord {
		count++
	}
	return count
}

func (s *server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSONOrLog(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /v1/models", s.models)
	mux.HandleFunc("POST /v1/chat/completions", s.chat)
	return authMiddleware(s.cfg.APIToken, mux)
}
func authMiddleware(token string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if token != "" && r.Header.Get("Authorization") != "Bearer "+token {
			writeError(w, http.StatusUnauthorized, "invalid bearer token")
			return
		}
		next.ServeHTTP(w, r)
	})
}
func writeSSE(w io.Writer, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("marshal server-sent event: %w", err)
	}
	if _, err := fmt.Fprintf(w, "data: %s\n\n", data); err != nil {
		return fmt.Errorf("write server-sent event: %w", err)
	}
	return nil
}
func writeJSON(w http.ResponseWriter, status int, value any) error {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(value); err != nil {
		return fmt.Errorf("encode JSON response: %w", err)
	}
	return nil
}
func writeError(w http.ResponseWriter, status int, message string) {
	response := openai.ErrorResponse{
		Error: &openai.APIError{
			Message: message,
			Type:    "invalid_request_error",
		},
	}
	if err := writeJSON(w, status, response); err != nil {
		slog.Error("write error response", "error", err)
	}
}
func writeJSONOrLog(w http.ResponseWriter, status int, value any) {
	if err := writeJSON(w, status, value); err != nil {
		slog.Error("write JSON response", "error", err)
	}
}
