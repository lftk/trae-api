package main

import (
	"context"
	"errors"
	"log/slog"
	"sync"
)

var errSessionLimit = errors.New("maximum trae ACP session limit reached")

// processPool owns initialized, not-yet-bound ACP processes. A process is
// removed from the pool when it is assigned to a session and is never reused
// after that session closes.
type processPool struct {
	mu       sync.Mutex
	cfg      config
	factory  func(context.Context, config) (*process, error)
	idle     []*process
	pending  int
	total    int
	notify   chan struct{}
	ctx      context.Context
	cancel   context.CancelFunc
	stopping bool
	wg       sync.WaitGroup
}

func newProcessPool(cfg config, factory func(context.Context, config) (*process, error)) *processPool {
	ctx, cancel := context.WithCancel(context.Background())
	p := &processPool{
		cfg:     cfg,
		factory: factory,
		notify:  make(chan struct{}),
		ctx:     ctx,
		cancel:  cancel,
	}
	p.fill()
	return p
}

func (p *processPool) acquire(ctx context.Context) (*process, error) {
	for {
		p.mu.Lock()
		if len(p.idle) > 0 {
			process := p.idle[len(p.idle)-1]
			p.idle = p.idle[:len(p.idle)-1]
			p.mu.Unlock()
			p.fill()
			select {
			case <-process.done:
				_ = process.Close()
				continue
			default:
			}
			return process, nil
		}
		if p.stopping {
			p.mu.Unlock()
			return nil, errors.New("process pool is shutting down")
		}
		if p.pending == 0 {
			p.pending++
			p.mu.Unlock()
			return p.create(ctx)
		}
		notify := p.notify
		p.mu.Unlock()
		select {
		case <-notify:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

func (p *processPool) create(ctx context.Context) (*process, error) {
	process, err := p.factory(ctx, p.cfg)
	p.mu.Lock()
	p.pending--
	if err == nil && process != nil && !p.stopping {
		p.total++
		p.mu.Unlock()
		p.observe(process)
		p.signal()
		p.fill()
		return process, nil
	}
	p.mu.Unlock()
	if process != nil {
		_ = process.Close()
	}
	p.signal()
	if err != nil {
		return nil, err
	}
	return nil, errors.New("process pool is shutting down")
}

func (p *processPool) fill() {
	p.mu.Lock()
	for len(p.idle)+p.pending < p.cfg.WarmProcesses &&
		!p.stopping {
		p.pending++
		p.wg.Add(1)
		go p.warm()
	}
	p.mu.Unlock()
}

func (p *processPool) warm() {
	defer p.wg.Done()
	process, err := p.factory(p.ctx, p.cfg)
	p.mu.Lock()
	p.pending--
	if err == nil && process != nil && !p.stopping {
		p.total++
		p.idle = append(p.idle, process)
		p.mu.Unlock()
		p.observe(process)
		p.signal()
		return
	}
	p.mu.Unlock()
	if process != nil {
		_ = process.Close()
	}
	p.signal()
	if err != nil && !errors.Is(err, context.Canceled) {
		slog.Warn("warm trae ACP process failed", "error", err)
	}
}

func (p *processPool) observe(process *process) {
	process.addOnDone(func() {
		p.mu.Lock()
		if p.total > 0 {
			p.total--
		}
		for i, idle := range p.idle {
			if idle == process {
				p.idle = append(p.idle[:i], p.idle[i+1:]...)
				break
			}
		}
		p.mu.Unlock()
		p.signal()
		p.fill()
	})
}

func (p *processPool) signal() {
	p.mu.Lock()
	old := p.notify
	p.notify = make(chan struct{})
	close(old)
	p.mu.Unlock()
}

func (p *processPool) close() {
	p.mu.Lock()
	p.stopping = true
	p.cancel()
	p.mu.Unlock()
	p.wg.Wait()
	p.mu.Lock()
	idle := p.idle
	p.idle = nil
	p.mu.Unlock()
	for _, process := range idle {
		_ = process.Close()
	}
}
