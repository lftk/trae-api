package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	acp "github.com/coder/acp-go-sdk"
)

type traeSession struct {
	mu           sync.Mutex
	cmd          *exec.Cmd
	stdin        io.WriteCloser
	conn         *acp.ClientSideConnection
	client       *traeClient
	session      acp.SessionId
	models       []string
	modelID      acp.SessionConfigId
	defaultModel string
	currentModel string
	lastUsed     time.Time
	done         chan struct{}
	onDone       func()
}

func newSession(ctx context.Context, cfg config) (*traeSession, error) {
	started := time.Now()
	workdir, err := resolveWorkdir(cfg.Workdir)
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(cfg.TraeBin, cfg.TraeArgs...)
	cmd.Dir = workdir
	configureProcess(cmd)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("create trae stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return nil, fmt.Errorf("create trae stdout pipe: %w", err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		return nil, fmt.Errorf("start trae cli: %w", err)
	}
	slog.Info("trae session process started", "elapsed", time.Since(started))
	c := &traeClient{updates: make(chan update, 128)}
	conn := acp.NewClientSideConnection(c, stdin, stdout)
	conn.SetLogger(slog.Default())
	s := &traeSession{cmd: cmd, stdin: stdin, conn: conn, client: c, done: make(chan struct{}), lastUsed: time.Now(), defaultModel: cfg.DefaultModel}
	go func() {
		if err := cmd.Wait(); err != nil {
			slog.Error("trae cli exited with error", "error", err)
		}
		close(s.done)
		s.mu.Lock()
		onDone := s.onDone
		s.mu.Unlock()
		if onDone != nil {
			onDone()
		}
	}()
	initCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	if _, err = conn.Initialize(initCtx, acp.InitializeRequest{
		ProtocolVersion: acp.ProtocolVersionNumber,
		ClientInfo: &acp.Implementation{
			Name:    "trae-api",
			Version: "0.1.0",
		},
		ClientCapabilities: acp.ClientCapabilities{
			Fs: acp.FileSystemCapabilities{
				ReadTextFile:  false,
				WriteTextFile: false,
			},
		},
	}); err != nil {
		_ = s.Close()
		return nil, fmt.Errorf("acp initialize: %w", err)
	}
	created, err := conn.NewSession(initCtx, acp.NewSessionRequest{Cwd: workdir, McpServers: []acp.McpServer{}})
	if err != nil {
		_ = s.Close()
		return nil, fmt.Errorf("acp new session: %w", err)
	}
	s.session = created.SessionId
	for _, option := range created.ConfigOptions {
		if option.Select == nil || option.Select.Options.Ungrouped == nil {
			continue
		}
		if option.Select.Category != nil && *option.Select.Category == acp.SessionConfigOptionCategoryModel {
			s.modelID = option.Select.Id
			for _, model := range *option.Select.Options.Ungrouped {
				s.models = append(s.models, string(model.Value))
			}
		}
	}
	return s, nil
}

func (s *traeSession) setModel(ctx context.Context, model string) (string, error) {
	if model == "" {
		if len(s.models) == 0 {
			return model, nil
		}
		model = s.models[0]
	}
	if s.currentModel == model {
		return model, nil
	}
	for _, available := range s.models {
		if available == model {
			if s.modelID == "" {
				s.currentModel = model
				return model, nil
			}
			_, err := s.conn.SetSessionConfigOption(ctx, acp.SetSessionConfigOptionRequest{ValueId: &acp.SetSessionConfigOptionValueId{SessionId: s.session, ConfigId: s.modelID, Value: acp.SessionConfigValueId(model)}})
			if err != nil {
				return "", fmt.Errorf("set session model %q: %w", model, err)
			}
			s.currentModel = model
			return model, nil
		}
	}
	if len(s.models) == 0 {
		s.currentModel = model
		return model, nil
	}
	if model == s.defaultModel && len(s.models) > 0 {
		return s.setModel(ctx, "")
	}
	return "", fmt.Errorf("model %q is not advertised by trae", model)
}

func (s *traeSession) prompt(ctx context.Context, text string, stream func(update)) (string, string, error) {
	s.lastUsed = time.Now()
	done := make(chan error, 1)
	go func() {
		_, err := s.conn.Prompt(ctx, acp.PromptRequest{SessionId: s.session, Prompt: []acp.ContentBlock{acp.TextBlock(text)}})
		done <- err
	}()
	var answer, reasoning strings.Builder
	consume := func(item update) {
		if item.Reasoning {
			reasoning.WriteString(item.Text)
		} else {
			answer.WriteString(item.Text)
		}
		if stream != nil {
			stream(item)
		}
	}
	for {
		select {
		case item := <-s.client.updates:
			consume(item)
		case err := <-done:
			if err != nil {
				return answer.String(), reasoning.String(), fmt.Errorf("prompt trae session: %w", err)
			}
			// SessionUpdate handlers enqueue notifications before Prompt can
			// return, but both the final update and the completion signal may
			// be ready in this select at the same time. Drain the queued
			// updates so the final agent_message_chunk is not lost.
			for {
				select {
				case item := <-s.client.updates:
					consume(item)
				case <-ctx.Done():
					answerText, reasoningText := responseText(answer.String(), reasoning.String())
					return answerText, reasoningText, ctx.Err()
				default:
					answerText, reasoningText := responseText(answer.String(), reasoning.String())
					return answerText, reasoningText, nil
				}
			}
		case <-ctx.Done():
			return answer.String(), reasoning.String(), ctx.Err()
		}
	}
}

func (s *traeSession) touchLocked() {
	s.lastUsed = time.Now()
}

func (s *traeSession) setOnDone(fn func()) {
	s.mu.Lock()
	s.onDone = fn
	dead := false
	select {
	case <-s.done:
		dead = true
	default:
	}
	s.mu.Unlock()
	if dead {
		fn()
	}
}

// responseText keeps clients from receiving an empty answer when an ACP agent
// emits the user-facing response as an agent_thought_chunk and never follows
// it with an agent_message_chunk. This has been observed with some Trae/model
// combinations and is especially visible through OpenAI-to-Anthropic proxies.
func responseText(answer, reasoning string) (string, string) {
	if strings.TrimSpace(answer) == "" && strings.TrimSpace(reasoning) != "" {
		return reasoning, reasoning
	}
	return answer, reasoning
}

func (s *traeSession) Close() error {
	var closeErr error
	if s.stdin != nil {
		closeErr = errors.Join(closeErr, s.stdin.Close())
	}
	if s.cmd != nil && s.cmd.Process != nil {
		if err := killProcess(s.cmd); err != nil && !errors.Is(err, os.ErrProcessDone) {
			closeErr = errors.Join(closeErr, err)
		}
	}
	return closeErr
}
