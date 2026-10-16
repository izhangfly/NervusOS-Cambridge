// Package kernel manages the ordered start and reverse-order stop of kernel modules
// It depends only on the Module interface and imports no concrete module, keeping dependencies one-way
// Kernel only knows the interface and leaves wiring details to main, which makes the lifecycle easy to unit-test
package kernel

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"
)

// Module is the common lifecycle contract of every kernel module
//
// # Start should return quickly: long-running loops start their own goroutines
//
// A loop only exits on Stop, not on Start(ctx)'s ctx. If the loop also listened to the signal ctx,
// it and stopAll would be woken by the same signal in parallel, which exits first would be undefined, and the stop order would no longer be
// decided solely by reverse-order Stop; a module's wind-down (revoking authority, confirming the stop, writing the audit) could be cut short
//
// # The ctx Stop receives has a timeout (see defaultStopTimeout); implementations should honour it and return quickly
//
// But Kernel doesn't trust modules to comply: even if Stop ignores ctx and hangs forever, Kernel
// gives up waiting after the timeout and carries on stopping the remaining modules (see stopModule). One stuck module mustn't drag down
// the whole shutdown sequence, especially the last one, which writes the audit
type Module interface {
	Name() string
	Start(ctx context.Context) error
	Stop(ctx context.Context) error
}

// FatalReporter is implemented by modules that have a background loop that may fail irrecoverably
//
// Modules with background loops must implement it. Without it, a module whose accept loop has died
// silently becomes an empty shell: Start returned successfully long ago, Stop was never called, Kernel can't see a thing,
// and from outside it only looks like "the service is running but doing nothing", the hardest kind of fault to diagnose
//
// A non-nil error on the channel means the module can't recover. Kernel then
// stops every module in reverse order and makes Run return the error, so main exits non-zero and systemd restarts it
//
// The channel should be buffered and written at most once; a module must never block writing to it because nobody reads
type FatalReporter interface {
	Fatal() <-chan error
}

// defaultStopTimeout is the time limit for one module's Stop
//
// Timed per module rather than one budget for the whole sequence: a total budget would let the modules at the end
//
//	(the lowest-level ones, whose wind-down matters most) be robbed of time because earlier modules were slow
const defaultStopTimeout = 5 * time.Second

type Kernel struct {
	log     *slog.Logger
	modules []Module

	// stopTimeout is a separate field so tests can drop it to milliseconds
	// instead of waiting out the production timeout
	stopTimeout time.Duration
}

func New(log *slog.Logger) *Kernel {
	return &Kernel{log: log, stopTimeout: defaultStopTimeout}
}

// Register adds modules in call order; start order = registration order, stop order = the reverse
// Only called before Run (during wiring); nothing is added or removed at run time
func (k *Kernel) Register(m Module) {
	k.modules = append(k.modules, m)
}

type moduleFailure struct {
	name string
	err  error
}

// Run starts every module in turn; if any start fails, it stops the started modules in reverse order and returns the error
// *Note: after a start failure, the module that failed to start isn't stopped
//
// Both during start-up and while running it listens to two stop sources:
//
//	ctx cancelled - SIGINT/SIGTERM, a planned stop; stop in reverse order and return nil
//	a module reports a fatal error - stop in reverse order and return that error, so main exits non-zero
//
// Key point: fatal watchers are installed module by module, and both sources are re-checked before starting each module.
// So if an already-ready module goes fatal during start-up, or SIGTERM arrives right then, Kernel
// stops starting on the spot and rolls back, instead of dutifully starting the rest
func (k *Kernel) Run(ctx context.Context) error {
	// watchDone makes the watcher goroutines exit with Run, so they don't hang on module channels
	// nobody reads any more. Both return paths are covered by defer
	watchDone := make(chan struct{})
	defer close(watchDone)

	// Capacity = number of modules: each module writes at most once and writers never block, even when Kernel is already
	// shutting down and no longer reads this channel
	fatal := make(chan moduleFailure, len(k.modules))

	started := make([]Module, 0, len(k.modules))

	for _, m := range k.modules {
		// Before starting the next one, check whether it's already time to stop. At the top of the loop, so that "SIGTERM during start-up"
		// and "an already-ready module going fatal during start-up" take effect at once, rather than after everything has started
		select {
		case <-ctx.Done():
			k.log.Info("shutdown requested during startup; rolling back", "started", len(started))
			if err := k.stopAll(started); err != nil {
				return fmt.Errorf("shutdown during startup completed with errors: %w", err)
			}
			return nil
		case f := <-fatal:
			k.log.Error("module failed during startup; rolling back",
				"module", f.name, "err", f.err, "started", len(started))
			return joinShutdown(fmt.Errorf("module %s failed at runtime: %w", f.name, f.err), k.stopAll(started))
		default:
		}

		k.log.Info("starting module", "module", m.Name())
		if err := m.Start(ctx); err != nil {
			k.log.Error("module failed to start; rolling back", "module", m.Name(), "err", err)
			return joinShutdown(fmt.Errorf("start %s: %w", m.Name(), err), k.stopAll(started))
		}
		started = append(started, m)

		// Install the watcher immediately, without waiting for everything to start. Installing one module's watcher late means ignoring
		// its fatal for the whole time the remaining modules are starting
		k.watchModuleFatal(m, fatal, watchDone)
	}
	k.log.Info("kernel ready", "modules", len(started))

	select {
	case <-ctx.Done():
		k.log.Info("shutdown signal received; shutting down")
		// Even in a planned stop, a failed wind-down must show in the exit code: otherwise, when revoking authority, confirming the stop
		// or writing the audit fails, the process still exits 0 and systemd can't see it, and that's exactly the kind of failure
		// that most needs to be seen
		if err := k.stopAll(started); err != nil {
			return fmt.Errorf("shutdown completed with errors: %w", err)
		}
		return nil

	case f := <-fatal:
		// No partial degradation: a half-dead TCB is more dangerous than a restart. Stop everything and let systemd restart it,
		// with StartLimitAction=reboot as the backstop for repeated failures
		k.log.Error("module reported a fatal error; shutting down kernel",
			"module", f.name, "err", f.err)
		// The fatal error is the main cause, but wind-down failures shouldn't be lost either; join them for offline analysis
		return joinShutdown(fmt.Errorf("module %s failed at runtime: %w", f.name, f.err), k.stopAll(started))
	}
}

// joinShutdown merges the main error with wind-down errors: the main error is always there, wind-down errors are appended if any
func joinShutdown(primary, stopErr error) error {
	if stopErr == nil {
		return primary
	}
	return errors.Join(primary, stopErr)
}

// watchModuleFatal installs a fatal-error watcher goroutine for one module
//
// Modules that don't implement FatalReporter, or whose Fatal returns nil, are skipped: the interface is optional,
// and infrastructure modules without background loops don't need it
func (k *Kernel) watchModuleFatal(m Module, out chan<- moduleFailure, done <-chan struct{}) {
	fr, ok := m.(FatalReporter)
	if !ok {
		return
	}
	ch := fr.Fatal()
	if ch == nil {
		return
	}
	go func(name string) {
		select {
		case err, open := <-ch:
			// The channel being closed (open=false) isn't a fault: the module may just have closed it while winding down.
			// Only a real non-nil error counts as fatal
			if open && err != nil {
				select {
				case out <- moduleFailure{name: name, err: err}:
				case <-done:
				}
			}
		case <-done:
		}
	}(m.Name())
}

// stopAll stops every module in reverse order and aggregates their stop errors
//
// One module failing or timing out doesn't break the sequence; carry on with the next (the audit at the end in particular
// must get its chance to wind down), but errors are collected and joined for the layer above to reflect in the exit code
func (k *Kernel) stopAll(started []Module) error {
	var errs []error
	for i := len(started) - 1; i >= 0; i-- {
		if err := k.stopModule(started[i]); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// stopModule stops one module, enforcing a time limit, and returns the stop error (including timeouts)
//
// Stop runs in its own goroutine and this function only waits stopTimeout for it. On timeout it gives up waiting
// and returns so the caller can stop the next module; the stuck goroutine leaks, but compared with the whole
// shutdown sequence (including the lowest-level audit) hanging forever, leaking one goroutine is clearly the smaller cost:
// the process exits soon after and the leaked goroutine goes with it, while safety is backed by systemd restarts and the MCU
// heartbeat stop
//
// A ctx independent of Run: the signal that triggered the stop has usually already cancelled the root ctx, and reusing it would make every
// module's Stop see a cancelled context the moment it starts and skip its wind-down entirely
func (k *Kernel) stopModule(m Module) error {
	k.log.Info("stopping module", "module", m.Name())

	ctx, cancel := context.WithTimeout(context.Background(), k.stopTimeout)
	defer cancel()

	// Capacity 1: even after this function has timed out and stopped receiving, the Stop goroutine's write when it finishes
	// won't block and leak on this channel
	done := make(chan error, 1)
	go func() { done <- m.Stop(ctx) }()

	select {
	case err := <-done:
		if err != nil {
			// Log immediately so ops see it during the stop, and also return it so the exit code reflects it
			k.log.Error("module failed to stop", "module", m.Name(), "err", err)
			return fmt.Errorf("stop %s: %w", m.Name(), err)
		}
		return nil
	case <-ctx.Done():
		k.log.Error("module stop timed out; abandoning and continuing shutdown",
			"module", m.Name(), "timeout", k.stopTimeout)
		return fmt.Errorf("stop %s: timed out after %s", m.Name(), k.stopTimeout)
	}
}
