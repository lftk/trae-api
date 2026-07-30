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

// process is the lifetime of one trae-cli ACP connection. It can own
// many independent ACP sessions.
type process struct {
	mu               sync.Mutex
	cmd              *exec.Cmd
	stdin            io.WriteCloser
	conn             *acp.ClientSideConnection
	client           *client
	workdir          string
	closeSupported   bool
	done             chan struct{}
	onDone           func()
	closeOnce        sync.Once
	newSessionFunc   func(context.Context) (*session, error)
	closeSessionFunc func(context.Context, acp.SessionId) error
}

// session is deliberately only session-scoped state. The HTTP session ID
// is mapped to this object by server; ACP notifications use session instead.
type session struct {
	mu           sync.Mutex
	process      *process
	session      acp.SessionId
	models       []string
	modelID      acp.SessionConfigId
	currentModel string
	lastUsed     time.Time
	updates      chan update
}

type promptResult struct {
	usage *acp.Usage
	err   error
}

func startProcess(ctx context.Context, cfg config) (*process, error) {
	started := time.Now()
	workdir, err := resolveWorkdir(cfg.Workdir)
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(cfg.TraeBin, acpArgs(cfg.Yolo)...)
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
	p := &process{
		cmd: cmd, stdin: stdin, workdir: workdir,
		client: &client{sessions: make(map[acp.SessionId]*session)},
		done:   make(chan struct{}),
	}
	p.conn = acp.NewClientSideConnection(p.client, stdin, stdout)
	p.conn.SetLogger(slog.Default())
	go p.wait()

	initCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	initialized, err := p.conn.Initialize(initCtx, acp.InitializeRequest{
		ProtocolVersion: acp.ProtocolVersionNumber,
		ClientInfo:      &acp.Implementation{Name: "trae-api", Version: "0.1.0"},
		ClientCapabilities: acp.ClientCapabilities{Fs: acp.FileSystemCapabilities{
			ReadTextFile: false, WriteTextFile: false,
		}},
	})
	if err != nil {
		_ = p.Close()
		return nil, fmt.Errorf("acp initialize: %w", err)
	}
	p.closeSupported = initialized.AgentCapabilities.SessionCapabilities.Close != nil
	slog.Info("trae ACP process started", "elapsed", time.Since(started), "session_close", p.closeSupported)
	return p, nil
}

func (p *process) wait() {
	if err := p.cmd.Wait(); err != nil {
		slog.Error("trae cli exited with error", "error", err)
	}
	p.closeOnce.Do(func() { close(p.done) })
	p.mu.Lock()
	onDone := p.onDone
	p.mu.Unlock()
	if onDone != nil {
		onDone()
	}
}

func (p *process) setOnDone(fn func()) {
	p.mu.Lock()
	p.onDone = fn
	dead := false
	select {
	case <-p.done:
		dead = true
	default:
	}
	p.mu.Unlock()
	if dead {
		fn()
	}
}

func (p *process) newSession(ctx context.Context) (*session, error) {
	if p.newSessionFunc != nil {
		return p.newSessionFunc(ctx)
	}
	select {
	case <-p.done:
		return nil, errors.New("trae ACP process is not running")
	default:
	}
	created, err := p.conn.NewSession(ctx, acp.NewSessionRequest{Cwd: p.workdir, McpServers: []acp.McpServer{}})
	if err != nil {
		return nil, fmt.Errorf("acp new session: %w", err)
	}
	s := &session{process: p, session: created.SessionId, lastUsed: time.Now(), updates: make(chan update, 128)}
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
	p.client.addSession(s)
	slog.Info("trae ACP session created", "sessionid", s.sessionID())
	return s, nil
}

// newSession remains a useful standalone constructor and is used as the
// default server factory for compatibility with existing callers.
func newSession(ctx context.Context, cfg config) (*session, error) {
	p, err := startProcess(ctx, cfg)
	if err != nil {
		return nil, err
	}
	s, err := p.newSession(ctx)
	if err != nil {
		_ = p.Close()
		return nil, err
	}
	return s, nil
}

func (s *session) sessionID() string { return string(s.session) }

func (s *session) setModel(ctx context.Context, model string) (string, error) {
	if model == "" && len(s.models) > 0 {
		model = s.models[0]
	}
	if s.currentModel == model {
		return model, nil
	}
	for _, available := range s.models {
		if available != model {
			continue
		}
		if s.modelID != "" {
			_, err := s.process.conn.SetSessionConfigOption(ctx, acp.SetSessionConfigOptionRequest{
				ValueId: &acp.SetSessionConfigOptionValueId{SessionId: s.session, ConfigId: s.modelID, Value: acp.SessionConfigValueId(model)},
			})
			if err != nil {
				return "", fmt.Errorf("set session model %q: %w", model, err)
			}
		}
		s.currentModel = model
		return model, nil
	}
	if len(s.models) == 0 {
		s.currentModel = model
		return model, nil
	}
	return "", fmt.Errorf("model %q is not advertised by trae", model)
}

func (s *session) prompt(ctx context.Context, text string, stream func(update)) (string, string, *acp.Usage, error) {
	s.lastUsed = time.Now()
	done := make(chan promptResult, 1)
	go func() {
		response, err := s.process.conn.Prompt(ctx, acp.PromptRequest{SessionId: s.session, Prompt: []acp.ContentBlock{acp.TextBlock(text)}})
		done <- promptResult{usage: response.Usage, err: err}
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
		case item := <-s.updates:
			consume(item)
		case result := <-done:
			if result.err != nil {
				return answer.String(), reasoning.String(), result.usage, fmt.Errorf("prompt trae session: %w", result.err)
			}
			for {
				select {
				case item := <-s.updates:
					consume(item)
				case <-ctx.Done():
					a, r := responseText(answer.String(), reasoning.String())
					return a, r, result.usage, ctx.Err()
				default:
					a, r := responseText(answer.String(), reasoning.String())
					return a, r, result.usage, nil
				}
			}
		case <-ctx.Done():
			return answer.String(), reasoning.String(), nil, ctx.Err()
		}
	}
}

func responseText(answer, reasoning string) (string, string) {
	if strings.TrimSpace(answer) == "" && strings.TrimSpace(reasoning) != "" {
		return reasoning, reasoning
	}
	return answer, reasoning
}

func (s *session) touchLocked() { s.lastUsed = time.Now() }

func (s *session) Close() error {
	s.process.client.removeSession(s.session)
	if !s.process.closeSupported && s.process.closeSessionFunc == nil {
		slog.Warn("trae ACP does not support session/close; removed local session", "sessionid", s.sessionID())
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.process.closeSession(ctx, s.session); err != nil {
		return fmt.Errorf("close ACP session %s: %w", s.sessionID(), err)
	}
	return nil
}

func (p *process) closeSession(ctx context.Context, id acp.SessionId) error {
	if p.closeSessionFunc != nil {
		return p.closeSessionFunc(ctx, id)
	}
	_, err := p.conn.CloseSession(ctx, acp.CloseSessionRequest{SessionId: id})
	return err
}

func (p *process) Close() error {
	var closeErr error
	if p.stdin != nil {
		closeErr = errors.Join(closeErr, p.stdin.Close())
	}
	if p.cmd != nil && p.cmd.Process != nil {
		if err := killProcess(p.cmd); err != nil && !errors.Is(err, os.ErrProcessDone) {
			closeErr = errors.Join(closeErr, err)
		}
	}
	return closeErr
}
