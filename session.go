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

// process is the lifetime of one trae-cli ACP connection and its session.
type process struct {
	mu               sync.Mutex
	cmd              *exec.Cmd
	stdin            io.WriteCloser
	conn             *acp.ClientSideConnection
	client           *client
	workdir          string
	workdirTemp      bool
	closeSupported   bool
	done             chan struct{}
	onDone           []func()
	doneOnce         sync.Once
	stopOnce         sync.Once
	stopErr          error
	stopRequested    bool
	boundSession     acp.SessionId
	sessionCreating  bool
	newSessionFunc   func(context.Context) (*session, error)
	closeSessionFunc func(context.Context, acp.SessionId) error
	exitDone         chan struct{}
	exitOnce         sync.Once
}

// session is deliberately only session-scoped state. The HTTP session ID
// is mapped to this object by server; ACP notifications use session instead.
type session struct {
	mu                  sync.Mutex
	process             *process
	session             acp.SessionId
	models              []string
	modelID             acp.SessionConfigId
	currentModel        string
	workspaceNoticeSent bool
	lastUsed            time.Time
	updates             chan update
	updatesMu           sync.RWMutex
	promptMu            sync.Mutex
	closed              bool
	closeErr            error
	closeDone           chan struct{}
	leases              int
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
		cmd: cmd, stdin: stdin, workdir: workdir, workdirTemp: cfg.WorkdirTemp,
		client: &client{sessions: make(map[acp.SessionId]*session)},
		done:   make(chan struct{}), exitDone: make(chan struct{}),
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
	err := p.cmd.Wait()
	p.mu.Lock()
	stopRequested := p.stopRequested
	p.mu.Unlock()
	if err != nil && !stopRequested {
		slog.Error("trae cli exited with error", "error", err)
	} else if err != nil {
		slog.Debug("trae cli stopped", "error", err)
	}
	p.markExited()
}

func (p *process) markExited() {
	p.exitOnce.Do(func() {
		if p.exitDone == nil {
			p.exitDone = make(chan struct{})
		}
		close(p.exitDone)
		p.notifyDone()
	})
}

func (p *process) addOnDone(fn func()) {
	p.mu.Lock()
	dead := false
	select {
	case <-p.done:
		dead = true
	default:
	}
	if !dead {
		p.onDone = append(p.onDone, fn)
	}
	p.mu.Unlock()
	if dead {
		fn()
	}
}

func (p *process) notifyDone() {
	p.doneOnce.Do(func() {
		close(p.done)
		p.mu.Lock()
		callbacks := append([]func(){}, p.onDone...)
		p.mu.Unlock()
		for _, callback := range callbacks {
			callback()
		}
	})
}

func (p *process) newSession(ctx context.Context) (*session, error) {
	p.mu.Lock()
	if p.boundSession != "" || p.sessionCreating {
		p.mu.Unlock()
		return nil, errors.New("trae ACP process already has a session")
	}
	p.sessionCreating = true
	p.mu.Unlock()
	var (
		s   *session
		err error
	)
	if p.newSessionFunc != nil {
		s, err = p.newSessionFunc(ctx)
		if err == nil {
			if processIsDone(p) {
				p.mu.Lock()
				p.sessionCreating = false
				p.mu.Unlock()
				return nil, errors.New("trae ACP process exited while creating session")
			}
			p.mu.Lock()
			p.boundSession = s.session
			p.sessionCreating = false
			p.mu.Unlock()
		}
		if err != nil {
			p.mu.Lock()
			p.sessionCreating = false
			p.mu.Unlock()
		}
		return s, err
	}
	select {
	case <-p.done:
		p.mu.Lock()
		p.sessionCreating = false
		p.mu.Unlock()
		return nil, errors.New("trae ACP process is not running")
	default:
	}
	created, err := p.conn.NewSession(ctx, acp.NewSessionRequest{Cwd: p.workdir, McpServers: []acp.McpServer{}})
	if err != nil {
		p.mu.Lock()
		p.sessionCreating = false
		p.mu.Unlock()
		return nil, fmt.Errorf("acp new session: %w", err)
	}
	s = &session{process: p, session: created.SessionId, lastUsed: time.Now(), updates: make(chan update, 128)}
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
	p.mu.Lock()
	if processIsDone(p) {
		p.sessionCreating = false
		p.mu.Unlock()
		return nil, errors.New("trae ACP process exited while creating session")
	}
	p.boundSession = s.session
	p.sessionCreating = false
	p.mu.Unlock()
	p.client.addSession(s)
	slog.Info("trae ACP session created", "sessionid", s.sessionID())
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

func (s *session) prompt(ctx context.Context, text string, stream func(update)) (string, string, *acp.Usage, string, error) {
	s.promptMu.Lock()
	defer s.promptMu.Unlock()
	s.lastUsed = time.Now()
	text = s.preparePrompt(text)
	updates := make(chan update, 128)
	s.updatesMu.Lock()
	s.updates = updates
	s.updatesMu.Unlock()
	defer func() {
		s.updatesMu.Lock()
		if s.updates == updates {
			s.updates = nil
		}
		s.updatesMu.Unlock()
	}()
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
		case item := <-updates:
			consume(item)
		case result := <-done:
			if result.err != nil {
				return answer.String(), reasoning.String(), result.usage, text, fmt.Errorf("prompt trae session: %w", result.err)
			}
			for {
				select {
				case item := <-updates:
					consume(item)
				case <-ctx.Done():
					a, r := responseText(answer.String(), reasoning.String())
					return a, r, result.usage, text, ctx.Err()
				default:
					a, r := responseText(answer.String(), reasoning.String())
					return a, r, result.usage, text, nil
				}
			}
		case <-ctx.Done():
			// Wait for ACP to finish before allowing this session to accept a
			// new prompt. This prevents late notifications from crossing the
			// request boundary.
			result := <-done
			return answer.String(), reasoning.String(), result.usage, text, ctx.Err()
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
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return s.closeWithContext(ctx)
}

func (s *session) closeWithContext(ctx context.Context) error {
	s.mu.Lock()
	if s.closed {
		done := s.closeDone
		err := s.closeErr
		s.mu.Unlock()
		<-done
		s.mu.Lock()
		err = s.closeErr
		s.mu.Unlock()
		return err
	}
	s.closed = true
	s.closeDone = make(chan struct{})
	s.mu.Unlock()

	s.process.client.removeSession(s.session)
	var closeErr error
	if !s.process.closeSupported && s.process.closeSessionFunc == nil {
		slog.Warn("trae ACP does not support session/close; removing local session", "sessionid", s.sessionID())
	} else {
		closeErr = s.process.closeSession(ctx, s.session)
		if closeErr != nil {
			closeErr = fmt.Errorf("close ACP session %s: %w", s.sessionID(), closeErr)
		}
	}
	closeErr = errors.Join(closeErr, s.process.closeWithContext(ctx))
	s.mu.Lock()
	s.closeErr = closeErr
	close(s.closeDone)
	s.mu.Unlock()
	return closeErr
}

func (p *process) closeSession(ctx context.Context, id acp.SessionId) error {
	if p.closeSessionFunc != nil {
		return p.closeSessionFunc(ctx, id)
	}
	_, err := p.conn.CloseSession(ctx, acp.CloseSessionRequest{SessionId: id})
	return err
}

func (p *process) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return p.closeWithContext(ctx)
}

func (p *process) closeWithContext(ctx context.Context) error {
	p.stopOnce.Do(func() {
		p.mu.Lock()
		p.stopRequested = true
		p.mu.Unlock()
		var stopErr error
		if p.stdin != nil {
			stopErr = errors.Join(stopErr, p.stdin.Close())
		}
		if p.cmd != nil && p.cmd.Process != nil {
			if err := killProcess(p.cmd); err != nil && !errors.Is(err, os.ErrProcessDone) {
				stopErr = errors.Join(stopErr, err)
			}
		}
		p.mu.Lock()
		p.stopErr = errors.Join(p.stopErr, stopErr)
		p.mu.Unlock()
		if p.cmd == nil {
			p.markExited()
		}
	})
	if p.exitDone == nil {
		p.markExited()
	}
	select {
	case <-p.exitDone:
	case <-ctx.Done():
		p.mu.Lock()
		p.stopErr = errors.Join(p.stopErr, ctx.Err())
		p.mu.Unlock()
	}
	p.mu.Lock()
	err := p.stopErr
	p.mu.Unlock()
	return err
}
