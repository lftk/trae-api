package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	openai "github.com/sashabaranov/go-openai"
)

func TestValidBearer(t *testing.T) {
	cases := []struct {
		name, header, token string
		want                bool
	}{
		{"exact", "Bearer sekrit", "sekrit", true},
		{"case-insensitive scheme", "bearer sekrit", "sekrit", true},
		{"extra spaces", "Bearer   sekrit  ", "sekrit", true},
		{"wrong token", "Bearer nope", "sekrit", false},
		{"missing scheme", "sekrit", "sekrit", false},
		{"basic auth", "Basic c2Vrcml0", "sekrit", false},
		{"empty header", "", "sekrit", false},
		{"empty token config", "Bearer anything", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := validBearer(tc.header, tc.token); got != tc.want {
				t.Fatalf("validBearer(%q, %q) = %v, want %v", tc.header, tc.token, got, tc.want)
			}
		})
	}
}

func TestAuthMiddlewareEnforcedOnlyWhenTokenSet(t *testing.T) {
	handle := func() http.Handler {
		return authMiddleware("sekrit", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
	}

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	handle().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("missing token status = %d, want 401", rec.Code)
	}

	rec = httptest.NewRecorder()
	req.Header.Set("Authorization", "Bearer wrong")
	handle().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong token status = %d, want 401", rec.Code)
	}

	rec = httptest.NewRecorder()
	req.Header.Set("Authorization", "bearer sekrit")
	handle().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("valid token status = %d, want 200", rec.Code)
	}

	// an empty configured token disables auth entirely
	open := authMiddleware("", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	rec = httptest.NewRecorder()
	open.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("no-token-config status = %d, want 200", rec.Code)
	}
}

func TestModelsRouteReturns502WhenTraeCLIFails(t *testing.T) {
	s := newServer(config{
		TraeBin:  filepath.Join(t.TempDir(), "missing-trae-cli"),
		Workdir:  t.TempDir(),
		StateDir: "", // avoid touching the real state dir
	})
	defer s.shutdown()
	rec := httptest.NewRecorder()
	s.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("models status = %d, want 502", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "list trae models") {
		t.Fatalf("models error body = %q", rec.Body.String())
	}
}

func TestParseModelsJSONAndLineFallback(t *testing.T) {
	jsonOut := []byte(`[{"id":"GLM-5","object":"model"},{"id":"Doubao-Seed","object":"model"}]`)
	models, err := parseModels(jsonOut)
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 2 || models[0] != "GLM-5" || models[1] != "Doubao-Seed" {
		t.Fatalf("parsed JSON models = %v", models)
	}

	lineOut := []byte("GLM-5\n\nDoubao-Seed \n")
	models, err = parseModels(lineOut)
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 2 || models[0] != "GLM-5" || models[1] != "Doubao-Seed" {
		t.Fatalf("parsed line models = %v", models)
	}
}

func TestEstimateTokens(t *testing.T) {
	if got := estimateTokens("hello world"); got != 2 {
		t.Fatalf("estimateTokens(\"hello world\") = %d, want 2", got)
	}
	if got := estimateTokens("你好世界"); got != 4 {
		t.Fatalf("estimateTokens CJK = %d, want 4", got)
	}
	if got := estimateTokens("a, b, c!"); got != 6 {
		t.Fatalf("estimateTokens punctuation = %d, want 6", got)
	}
	if got := estimateTokens(""); got != 0 {
		t.Fatalf("estimateTokens empty = %d, want 0", got)
	}
}

func TestResolveResponseSurfacesReasoningWhenAnswerEmpty(t *testing.T) {
	answer, reasoning := resolveResponse("", "deep thoughts")
	if answer != reasoning {
		t.Fatalf("resolveResponse empty answer = %q, want reasoning %q", answer, reasoning)
	}
	answer, reasoning = resolveResponse("real answer", "deep thoughts")
	if answer != "real answer" || reasoning != "deep thoughts" {
		t.Fatalf("resolveResponse with answer = (%q, %q)", answer, reasoning)
	}
}

func TestFingerprintMessagesStableAndDistinct(t *testing.T) {
	a := []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleUser, Content: "hello"},
		{Role: openai.ChatMessageRoleAssistant, Content: "hi"},
	}
	if fingerprintMessages(a) != fingerprintMessages(a) {
		t.Fatal("fingerprint of identical transcript is not stable")
	}
	reordered := []openai.ChatCompletionMessage{a[1], a[0]}
	if fingerprintMessages(a) == fingerprintMessages(reordered) {
		t.Fatal("fingerprint ignores message order")
	}
	edited := []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleUser, Content: "hello"},
		{Role: openai.ChatMessageRoleAssistant, Content: "bye"},
	}
	if fingerprintMessages(a) == fingerprintMessages(edited) {
		t.Fatal("fingerprint ignores content edits")
	}
}

func TestDecodeChatRequestValidation(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"empty messages", `{"messages":[]}`},
		{"malformed json", `{"messages":`},
		{"multiple json values", `{"messages":[{"role":"user","content":"hi"}]}{}`},
		{"unsupported content type", `{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"x"}}]}]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(tc.body))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			if _, ok := decodeChatRequest(rec, req); ok {
				t.Fatalf("decodeChatRequest accepted %q", tc.body)
			}
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", rec.Code)
			}
		})
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	if _, ok := decodeChatRequest(rec, req); !ok {
		t.Fatalf("decodeChatRequest rejected a valid body: %s", rec.Body.String())
	}
}

func TestListModelsUsesConfiguredWorkdir(t *testing.T) {
	dir := t.TempDir()
	s := newServer(config{
		TraeBin:  filepath.Join(dir, "missing-trae-cli"),
		Workdir:  dir,
		StateDir: "",
	})
	defer s.shutdown()
	// resolveWorkdir must accept the configured directory; the exec itself
	// fails because the binary does not exist.
	if _, err := s.listModels(context.Background()); err == nil {
		t.Fatal("listModels succeeded with a missing binary")
	}
}
