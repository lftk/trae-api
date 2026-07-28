package main

import (
	"fmt"
	"strings"

	openai "github.com/sashabaranov/go-openai"
)

func validateMessages(messages []openai.ChatCompletionMessage) error {
	for _, message := range messages {
		for _, part := range message.MultiContent {
			if part.Type != openai.ChatMessagePartTypeText && part.Type != "" {
				return fmt.Errorf("message content type %q is not supported; only text is supported", part.Type)
			}
		}
	}
	return nil
}

func formatPrompt(messages []openai.ChatCompletionMessage) string {
	var b strings.Builder
	for _, m := range messages {
		b.WriteString("[" + m.Role + "]\n")
		b.WriteString(messageText(m))
		b.WriteString("\n\n")
	}
	return strings.TrimSpace(b.String())
}

func messageText(message openai.ChatCompletionMessage) string {
	if message.Content != "" {
		return message.Content
	}
	var b strings.Builder
	for _, part := range message.MultiContent {
		if part.Type == openai.ChatMessagePartTypeText || part.Type == "" {
			b.WriteString(part.Text)
		}
	}
	return b.String()
}
