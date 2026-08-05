package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"

	acp "github.com/coder/acp-go-sdk"
)

type update struct {
	Text      string
	Reasoning bool
}

type client struct {
	mu       sync.RWMutex
	sessions map[acp.SessionId]*session
}

func (c *client) addSession(s *session) {
	c.mu.Lock()
	c.sessions[s.session] = s
	c.mu.Unlock()
}
func (c *client) removeSession(id acp.SessionId) {
	c.mu.Lock()
	delete(c.sessions, id)
	c.mu.Unlock()
}

func (c *client) ReadTextFile(context.Context, acp.ReadTextFileRequest) (acp.ReadTextFileResponse, error) {
	return acp.ReadTextFileResponse{}, errors.New("file reads are not supported")
}
func (c *client) WriteTextFile(context.Context, acp.WriteTextFileRequest) (acp.WriteTextFileResponse, error) {
	return acp.WriteTextFileResponse{}, errors.New("file writes are not supported")
}
func (c *client) RequestPermission(context.Context, acp.RequestPermissionRequest) (acp.RequestPermissionResponse, error) {
	return acp.RequestPermissionResponse{}, errors.New("permission requests are disabled; use --yolo")
}
func (c *client) CreateTerminal(context.Context, acp.CreateTerminalRequest) (acp.CreateTerminalResponse, error) {
	return acp.CreateTerminalResponse{}, errors.New("terminals are not supported")
}
func (c *client) KillTerminal(context.Context, acp.KillTerminalRequest) (acp.KillTerminalResponse, error) {
	return acp.KillTerminalResponse{}, errors.New("terminals are not supported")
}
func (c *client) TerminalOutput(context.Context, acp.TerminalOutputRequest) (acp.TerminalOutputResponse, error) {
	return acp.TerminalOutputResponse{}, errors.New("terminals are not supported")
}
func (c *client) ReleaseTerminal(context.Context, acp.ReleaseTerminalRequest) (acp.ReleaseTerminalResponse, error) {
	return acp.ReleaseTerminalResponse{}, errors.New("terminals are not supported")
}
func (c *client) WaitForTerminalExit(context.Context, acp.WaitForTerminalExitRequest) (acp.WaitForTerminalExitResponse, error) {
	return acp.WaitForTerminalExitResponse{}, errors.New("terminals are not supported")
}

func (c *client) SessionUpdate(ctx context.Context, n acp.SessionNotification) error {
	c.mu.RLock()
	s := c.sessions[n.SessionId]
	c.mu.RUnlock()
	if s == nil {
		return fmt.Errorf("unknown ACP session %s", n.SessionId)
	}
	if n.Update.AgentMessageChunk != nil && n.Update.AgentMessageChunk.Content.Text != nil {
		slog.Debug("ACP agent message chunk", "acp_sessionid", n.SessionId, "text", n.Update.AgentMessageChunk.Content.Text.Text)
		return c.sendUpdate(ctx, s.updates, update{Text: n.Update.AgentMessageChunk.Content.Text.Text})
	}
	if n.Update.AgentThoughtChunk != nil && n.Update.AgentThoughtChunk.Content.Text != nil {
		slog.Debug("ACP agent thought chunk", "acp_sessionid", n.SessionId, "text", n.Update.AgentThoughtChunk.Content.Text.Text)
		return c.sendUpdate(ctx, s.updates, update{Text: n.Update.AgentThoughtChunk.Content.Text.Text, Reasoning: true})
	}
	return nil
}

func (c *client) sendUpdate(ctx context.Context, updates chan update, item update) error {
	select {
	case updates <- item:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("deliver session update: %w", ctx.Err())
	}
}
