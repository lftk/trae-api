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
	c.sessions[s.id] = s
	c.mu.Unlock()
}
func (c *client) removeSession(id acp.SessionId) {
	c.mu.Lock()
	delete(c.sessions, id)
	c.mu.Unlock()
}

var (
	errFileAccessUnsupported = errors.New("file access is not supported")
	errPermissionsDisabled   = errors.New("permission requests are disabled; use --yolo")
	errTerminalsUnsupported  = errors.New("terminals are not supported")
)

func (c *client) ReadTextFile(context.Context, acp.ReadTextFileRequest) (acp.ReadTextFileResponse, error) {
	return acp.ReadTextFileResponse{}, errFileAccessUnsupported
}
func (c *client) WriteTextFile(context.Context, acp.WriteTextFileRequest) (acp.WriteTextFileResponse, error) {
	return acp.WriteTextFileResponse{}, errFileAccessUnsupported
}
func (c *client) RequestPermission(context.Context, acp.RequestPermissionRequest) (acp.RequestPermissionResponse, error) {
	return acp.RequestPermissionResponse{}, errPermissionsDisabled
}
func (c *client) CreateTerminal(context.Context, acp.CreateTerminalRequest) (acp.CreateTerminalResponse, error) {
	return acp.CreateTerminalResponse{}, errTerminalsUnsupported
}
func (c *client) KillTerminal(context.Context, acp.KillTerminalRequest) (acp.KillTerminalResponse, error) {
	return acp.KillTerminalResponse{}, errTerminalsUnsupported
}
func (c *client) TerminalOutput(context.Context, acp.TerminalOutputRequest) (acp.TerminalOutputResponse, error) {
	return acp.TerminalOutputResponse{}, errTerminalsUnsupported
}
func (c *client) ReleaseTerminal(context.Context, acp.ReleaseTerminalRequest) (acp.ReleaseTerminalResponse, error) {
	return acp.ReleaseTerminalResponse{}, errTerminalsUnsupported
}
func (c *client) WaitForTerminalExit(context.Context, acp.WaitForTerminalExitRequest) (acp.WaitForTerminalExitResponse, error) {
	return acp.WaitForTerminalExitResponse{}, errTerminalsUnsupported
}

func (c *client) SessionUpdate(ctx context.Context, n acp.SessionNotification) error {
	c.mu.RLock()
	s := c.sessions[n.SessionId]
	c.mu.RUnlock()
	if s == nil {
		return fmt.Errorf("unknown ACP session %s", n.SessionId)
	}
	s.updatesMu.RLock()
	updates := s.updates
	s.updatesMu.RUnlock()
	if updates == nil {
		return nil
	}
	if chunk := n.Update.AgentMessageChunk; chunk != nil && chunk.Content.Text != nil {
		slog.Debug("ACP agent message chunk", "acp_sessionid", n.SessionId)
		return c.sendUpdate(ctx, updates, update{Text: chunk.Content.Text.Text})
	}
	if chunk := n.Update.AgentThoughtChunk; chunk != nil && chunk.Content.Text != nil {
		slog.Debug("ACP agent thought chunk", "acp_sessionid", n.SessionId)
		return c.sendUpdate(ctx, updates, update{Text: chunk.Content.Text.Text, Reasoning: true})
	}
	return nil
}

func (c *client) sendUpdate(ctx context.Context, updates chan update, item update) error {
	select {
	case updates <- item:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("deliver session update: %w", ctx.Err())
	default:
		// ACP notifications must not block the connection reader when the
		// HTTP client has gone away or the prompt queue is being discarded.
		return nil
	}
}
