// Package kernel starts the kernel modules in order and shuts them down in reverse.
// It depends only on the Module interface and imports no concrete module, so dependencies flow one way.
// The Kernel knows only the interface and leaves assembly to main, which makes its lifecycle easy to unit test.
package kernel

import (
	"context"
	"fmt"
	"log/slog"
)

// Module is the common lifecycle contract of every kernel module.
// Start should return quickly: long-running loops start their own goroutine and exit when ctx is cancelled or Stop is called.
type Module interface {
	Name() string
	Start(ctx context.Context) error
	Stop(ctx context.Context) error
}

type Kernel struct {
	log     *slog.Logger
	modules []Module
}

func New(log *slog.Logger) *Kernel {
	return &Kernel{log: log}
}

// Register records modules in call order; start order = registration order, shutdown order = the reverse.
// Call it only before Run (during assembly); nothing is added or removed at run time.
func (k *Kernel) Register(m Module) {
	k.modules = append(k.modules, m)
}

// Run starts every module in turn; if one fails to start, it stops the started ones in reverse and returns the error.
// *Note: after a failed start, the module that failed to start is not stopped.
// Once all are started it blocks until ctx is cancelled, then shuts them down in reverse.
func (k *Kernel) Run(ctx context.Context) error {
	started := make([]Module, 0, len(k.modules))
	for _, m := range k.modules {
		k.log.Info("starting module", "module", m.Name())
		if err := m.Start(ctx); err != nil {
			k.log.Error("module failed to start; rolling back", "module", m.Name(), "err", err)
			k.stopAll(started)
			return fmt.Errorf("start %s: %w", m.Name(), err)
		}
		started = append(started, m)
	}
	k.log.Info("kernel ready", "modules", len(started))

	<-ctx.Done()
	k.log.Info("shutdown signal received; shutting down")
	k.stopAll(started)
	return nil
}

// stopAll shuts every module down in reverse; an error from one module is only logged.
// It uses a shutdown context independent of ctx, so cleanup still runs after the signal that started the shutdown has cancelled the root ctx.
func (k *Kernel) stopAll(started []Module) {
	ctx := context.Background() // TODO(rewrite): add a shutdown timeout (context.WithTimeout)
	for i := len(started) - 1; i >= 0; i-- {
		m := started[i]
		k.log.Info("stopping module", "module", m.Name())
		if err := m.Stop(ctx); err != nil {
			k.log.Error("module failed to stop", "module", m.Name(), "err", err)
		}
	}
}
