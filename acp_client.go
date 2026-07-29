package main

import (
	"context"
	"errors"
	"fmt"

	acp "github.com/coder/acp-go-sdk"
)

type update struct {
	Text      string
	Reasoning bool
}

type traeClient struct{ updates chan update }

func (c *traeClient) ReadTextFile(
	context.Context,
	acp.ReadTextFileRequest,
) (acp.ReadTextFileResponse, error) {
	return acp.ReadTextFileResponse{}, errors.New("file reads are not supported")
}
func (c *traeClient) WriteTextFile(
	context.Context,
	acp.WriteTextFileRequest,
) (acp.WriteTextFileResponse, error) {
	return acp.WriteTextFileResponse{}, errors.New("file writes are not supported")
}
func (c *traeClient) RequestPermission(
	context.Context,
	acp.RequestPermissionRequest,
) (acp.RequestPermissionResponse, error) {
	return acp.RequestPermissionResponse{}, errors.New("permission requests are disabled; use --yolo")
}
func (c *traeClient) CreateTerminal(
	context.Context,
	acp.CreateTerminalRequest,
) (acp.CreateTerminalResponse, error) {
	return acp.CreateTerminalResponse{}, errors.New("terminals are not supported")
}
func (c *traeClient) KillTerminal(
	context.Context,
	acp.KillTerminalRequest,
) (acp.KillTerminalResponse, error) {
	return acp.KillTerminalResponse{}, errors.New("terminals are not supported")
}
func (c *traeClient) TerminalOutput(
	context.Context,
	acp.TerminalOutputRequest,
) (acp.TerminalOutputResponse, error) {
	return acp.TerminalOutputResponse{}, errors.New("terminals are not supported")
}
func (c *traeClient) ReleaseTerminal(
	context.Context,
	acp.ReleaseTerminalRequest,
) (acp.ReleaseTerminalResponse, error) {
	return acp.ReleaseTerminalResponse{}, errors.New("terminals are not supported")
}
func (c *traeClient) WaitForTerminalExit(
	context.Context,
	acp.WaitForTerminalExitRequest,
) (acp.WaitForTerminalExitResponse, error) {
	return acp.WaitForTerminalExitResponse{}, errors.New("terminals are not supported")
}

func (c *traeClient) SessionUpdate(ctx context.Context, n acp.SessionNotification) error {
	if n.Update.AgentMessageChunk != nil && n.Update.AgentMessageChunk.Content.Text != nil {
		return c.sendUpdate(ctx, update{Text: n.Update.AgentMessageChunk.Content.Text.Text})
	}
	if n.Update.AgentThoughtChunk != nil && n.Update.AgentThoughtChunk.Content.Text != nil {
		return c.sendUpdate(ctx, update{
			Text:      n.Update.AgentThoughtChunk.Content.Text.Text,
			Reasoning: true,
		})
	}
	return nil
}

func (c *traeClient) sendUpdate(ctx context.Context, item update) error {
	select {
	case c.updates <- item:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("deliver session update: %w", ctx.Err())
	}
}
