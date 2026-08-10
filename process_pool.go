package main

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

var errSessionLimit = errors.New("maximum trae ACP session limit reached")
var errProcessPoolStopped = errors.New("process pool is shutting down")
var errProcessFactoryNil = errors.New("process factory returned a nil process")

const defaultMaxProcesses = 100

// processPool owns initialized, not-yet-bound ACP processes. A process is
// removed from the pool when it is assigned to a session and is never reused
// after that session closes.
//
// processSlots limits the total number of live or creating processes. warmSlots
// limits the number of ready or creating warm processes. The channels are the
// counters; fill is the only function that starts process creation.
type processPool struct {
	cfg     config
	factory func(context.Context, config) (*process, error)

	ctx    context.Context
	cancel context.CancelFunc
	stop   chan struct{}
	done   chan struct{}

	ready        chan *pooledProcess
	processSlots chan struct{}
	warmSlots    chan struct{}
	waiting      atomic.Int64

	fillMu    sync.Mutex
	wg        sync.WaitGroup
	closeOnce sync.Once
}

type pooledProcess struct {
	process *process
	claimed atomic.Bool
}

func newProcessPool(cfg config, factory func(context.Context, config) (*process, error)) *processPool {
	if cfg.MaxProcesses < 1 {
		cfg.MaxProcesses = defaultMaxProcesses
	}
	if cfg.WarmProcesses > cfg.MaxProcesses {
		cfg.WarmProcesses = cfg.MaxProcesses
	}
	warmCapacity := cfg.WarmProcesses
	if warmCapacity < 1 {
		warmCapacity = 1
	}
	ctx, cancel := context.WithCancel(context.Background())
	p := &processPool{
		cfg:          cfg,
		factory:      factory,
		ctx:          ctx,
		cancel:       cancel,
		stop:         make(chan struct{}),
		done:         make(chan struct{}),
		ready:        make(chan *pooledProcess, cfg.MaxProcesses),
		processSlots: make(chan struct{}, cfg.MaxProcesses),
		warmSlots:    make(chan struct{}, warmCapacity),
	}
	p.fill()
	return p
}

func (p *processPool) acquire(ctx context.Context) (*process, error) {
	p.waiting.Add(1)
	p.fillDemand()
	for {
		select {
		case entry, ok := <-p.ready:
			if !ok {
				return nil, errProcessPoolStopped
			}
			p.claim(entry)
			if processIsDone(entry.process) {
				_ = entry.process.Close()
				p.fillDemand()
				continue
			}
			p.waiting.Add(-1)
			p.fill()
			return entry.process, nil
		case <-ctx.Done():
			p.waiting.Add(-1)
			return nil, ctx.Err()
		case <-p.stop:
			p.waiting.Add(-1)
			return nil, errProcessPoolStopped
		}
	}
}

func (p *processPool) fill() {
	p.fillTarget(p.cfg.WarmProcesses)
}

func (p *processPool) fillDemand() {
	target := p.cfg.WarmProcesses
	if target < 1 {
		target = 1
	}
	p.fillTarget(target)
}

func (p *processPool) fillTarget(target int) {
	p.fillMu.Lock()
	defer p.fillMu.Unlock()

	for !p.isStopping() && len(p.warmSlots) < target {
		select {
		case p.processSlots <- struct{}{}:
		default:
			return
		}
		select {
		case p.warmSlots <- struct{}{}:
			p.wg.Add(1)
			go p.create()
		default:
			p.releaseProcessSlot()
			return
		}
	}
}

func (p *processPool) create() {
	defer p.wg.Done()
	process, err := p.factory(p.ctx, p.cfg)
	if err != nil || process == nil || p.isStopping() {
		if process != nil {
			_ = process.Close()
		}
		p.releaseProcessSlot()
		p.releaseWarmSlot()
		if err != nil && !errors.Is(err, context.Canceled) && !p.isStopping() {
			slog.Warn("create trae ACP process failed", "error", err)
		}
		if !p.isStopping() {
			time.AfterFunc(100*time.Millisecond, p.refill)
		}
		return
	}

	entry := &pooledProcess{process: process}
	p.observe(entry)
	select {
	case p.ready <- entry:
	case <-p.stop:
		p.claim(entry)
		_ = process.Close()
	}
}

func (p *processPool) observe(entry *pooledProcess) {
	entry.process.addOnDone(func() {
		if entry.claimed.Swap(true) {
			p.releaseProcessSlot()
		} else {
			p.releaseWarmSlot()
			p.releaseProcessSlot()
		}
		p.refill()
	})
}

func (p *processPool) refill() {
	if p.waiting.Load() > 0 {
		p.fillDemand()
		return
	}
	p.fill()
}

func (p *processPool) claim(entry *pooledProcess) {
	if !entry.claimed.Swap(true) {
		p.releaseWarmSlot()
	}
}

func (p *processPool) releaseProcessSlot() {
	select {
	case <-p.processSlots:
	default:
	}
}

func (p *processPool) releaseWarmSlot() {
	select {
	case <-p.warmSlots:
	default:
	}
}

func (p *processPool) isStopping() bool {
	select {
	case <-p.stop:
		return true
	default:
		return false
	}
}

func processIsDone(process *process) bool {
	select {
	case <-process.done:
		return true
	default:
		return false
	}
}

func (p *processPool) close() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	p.closeWithContext(ctx)
}

func (p *processPool) closeWithContext(ctx context.Context) {
	p.closeOnce.Do(func() {
		p.fillMu.Lock()
		p.cancel()
		close(p.stop)
		p.fillMu.Unlock()

		waitDone := make(chan struct{})
		go func() {
			p.wg.Wait()
			close(waitDone)
		}()
		select {
		case <-waitDone:
		case <-ctx.Done():
			return
		}
		for {
			select {
			case entry := <-p.ready:
				p.claim(entry)
				_ = entry.process.closeWithContext(ctx)
			default:
				close(p.ready)
				close(p.done)
				return
			}
		}
	})
}
