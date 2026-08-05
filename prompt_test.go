package main

import (
	"strings"
	"testing"

	openai "github.com/sashabaranov/go-openai"
)

func TestTemporaryWorkspaceNoticeIsInjectedOncePerSession(t *testing.T) {
	p := &process{workdirTemp: true}
	one := &session{process: p}
	two := &session{process: p}
	original := "[user]\nHelp with this code."

	first := one.preparePrompt(original)
	if !strings.HasPrefix(first, "[system]\n"+temporaryWorkspaceNotice+"\n\n") {
		t.Fatalf("first prompt did not contain the workspace notice: %q", first)
	}
	if !strings.HasSuffix(first, original) {
		t.Fatalf("first prompt did not preserve the original prompt: %q", first)
	}
	if second := one.preparePrompt(original); second != original {
		t.Fatalf("workspace notice was injected more than once: %q", second)
	}
	if firstForTwo := two.preparePrompt(original); firstForTwo == original {
		t.Fatal("workspace notice state was shared between sessions")
	}
}

func TestTemporaryWorkspaceNoticePreservesMessageOrder(t *testing.T) {
	messages := []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleSystem, Content: "Project rules"},
		{Role: openai.ChatMessageRoleUser, Content: "Question"},
		{Role: openai.ChatMessageRoleAssistant, Content: "Earlier answer"},
	}
	original := formatPrompt(messages)
	s := &session{process: &process{workdirTemp: true}}
	prepared := s.preparePrompt(original)

	if !strings.HasSuffix(prepared, original) {
		t.Fatalf("prepared prompt changed the original messages: %q", prepared)
	}
	if strings.Count(prepared, original) != 1 {
		t.Fatalf("prepared prompt duplicated the original messages: %q", prepared)
	}
}

func TestExplicitWorkspaceDoesNotInjectNotice(t *testing.T) {
	s := &session{process: &process{workdirTemp: false}}
	original := "[user]\nInspect the repository."

	if got := s.preparePrompt(original); got != original {
		t.Fatalf("explicit workspace prompt changed: %q", got)
	}
}

func TestEstimatedUsageIncludesTemporaryWorkspaceNotice(t *testing.T) {
	s := &session{process: &process{workdirTemp: true}}
	original := "[user]\nHello"
	prepared := s.preparePrompt(original)
	usage := openAIUsage(nil, prepared, "answer", "")

	if usage.PromptTokens != estimateTokens(prepared) {
		t.Fatalf("prompt tokens = %d, want %d", usage.PromptTokens, estimateTokens(prepared))
	}
	if usage.PromptTokens <= estimateTokens(original) {
		t.Fatalf("prompt tokens = %d, want more than original prompt's %d", usage.PromptTokens, estimateTokens(original))
	}
}
