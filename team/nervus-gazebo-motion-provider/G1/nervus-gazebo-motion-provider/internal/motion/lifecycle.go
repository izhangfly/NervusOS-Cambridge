package motion

import (
	"context"
	"errors"
	"sync"
)

type State uint8

const (
	StateUnspecified State = iota
	StateDiscovering
	StateReady
	StateDegraded
	StateUnsupported
	StateClosed
)

var (
	ErrNotReady    = errors.New("motion adapter is not ready")
	ErrInvalidated = errors.New("motion adapter generation was invalidated")
)

type Binding struct {
	ProfileID       string
	Model           string
	CapabilityRole  string
	ResourceRole    string
	WorldGeneration uint64
}

type Command struct {
	Generation uint64
	Payload    any
}

// Adapter is the common lifecycle implemented by every semantic motion path.
// Execute must honor ctx and must never replay a command after Invalidate.
type Adapter interface {
	Bind(context.Context, Binding) error
	State() State
	Execute(context.Context, Command) error
	Stop(context.Context) error
	Invalidate(uint64)
	Close() error
}

// Lifecycle provides the generation and state gate shared by concrete
// adapters. It deliberately contains no transport-specific behavior.
type Lifecycle struct {
	mu         sync.RWMutex
	state      State
	generation uint64
	binding    Binding
	closed     bool
}

func (l *Lifecycle) BeginBind(binding Binding) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return ErrNotReady
	}
	if binding.WorldGeneration == 0 {
		return ErrInvalidated
	}
	l.binding = binding
	l.generation = binding.WorldGeneration
	l.state = StateDiscovering
	return nil
}

func (l *Lifecycle) MarkReady() {
	l.mu.Lock()
	if !l.closed && l.generation != 0 {
		l.state = StateReady
	}
	l.mu.Unlock()
}

func (l *Lifecycle) MarkUnsupported() {
	l.mu.Lock()
	if !l.closed {
		l.state = StateUnsupported
	}
	l.mu.Unlock()
}

func (l *Lifecycle) State() State {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.state
}

func (l *Lifecycle) RequireReady(generation uint64) error {
	l.mu.RLock()
	defer l.mu.RUnlock()
	if l.closed || l.state != StateReady {
		return ErrNotReady
	}
	if generation == 0 || generation != l.generation {
		return ErrInvalidated
	}
	return nil
}

func (l *Lifecycle) Invalidate(nextGeneration uint64) {
	l.mu.Lock()
	if !l.closed {
		l.generation = nextGeneration
		l.state = StateDegraded
	}
	l.mu.Unlock()
}

func (l *Lifecycle) Close() {
	l.mu.Lock()
	l.closed = true
	l.generation = 0
	l.state = StateClosed
	l.mu.Unlock()
}
