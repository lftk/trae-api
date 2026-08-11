package main

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"io"
	"strconv"
	"strings"

	openai "github.com/sashabaranov/go-openai"
)

const temporaryWorkspaceNotice = `Important workspace notice:

The current ACP working directory is an isolated temporary placeholder created by trae-api. It is not the user's actual project directory and must not be used to infer the project's structure, files, repository state, or instructions.

Use only the project context and file contents supplied in the conversation. Do not create, edit, search, or execute project files in this temporary directory unless the user explicitly asks you to do so. If the task requires access to files that were not supplied, state that the actual project workspace is unavailable.`

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

func (s *session) preparePrompt(text string) string {
	if s.process == nil || !s.process.workdirTemp || s.workspaceNoticeSent {
		return text
	}
	s.workspaceNoticeSent = true
	return "[system]\n" + temporaryWorkspaceNotice + "\n\n" + text
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

// fingerprintMessages hashes a message sequence (role + text content) for
// implicit-session continuity detection. Clients without a session ID (for
// example VS Code chat) resend the full transcript on every request; a request
// whose prefix matches the transcript of a live implicit session continues it.
func fingerprintMessages(messages []openai.ChatCompletionMessage) uint64 {
	h := sha256.New()
	for _, message := range messages {
		_, _ = io.WriteString(h, message.Role)
		h.Write([]byte{0})
		_, _ = io.WriteString(h, messageText(message))
		h.Write([]byte{0})
	}
	return binary.BigEndian.Uint64(h.Sum(nil)[:8])
}

func formatFingerprint(fp uint64) string {
	return strconv.FormatUint(fp, 16)
}
