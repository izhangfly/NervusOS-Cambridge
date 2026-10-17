package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/nervus-os/nervud/internal/audit"
	"github.com/nervus-os/nervud/internal/authority"
	"github.com/nervus-os/nervud/internal/kernel"
	"github.com/nervus-os/nervud/internal/scheduler"
)

func main() {
	// Startup flags (none in production)
	// Control-plane IPC entry point. Production images always use /run/nervus/nervud.sock;
	// the flag is for development only
	sockPath := flag.String("sock", "/run/nervus/nervud.sock", "IPC socket path")
	logLevel := flag.String("log-level", "info", "Log level: debug/info/warn/error")
	// Development machines only (Linux without CAP_SYS_NICE): keep running at normal priority when setting
	// real-time priority fails. Disabled in production, where Linux without CAP_SYS_NICE exits at once
	allowSchedDegrade := flag.Bool("dev-allow-sched-degrade", false,
		"[DEV] Allow real-time priority setting failure to downgrade to normal priority")
	flag.Parse()

	logger := newLogger(*logLevel)
	slog.SetDefault(logger)

	// Root context, bound to SIGINT/SIGTERM. A signal cancels ctx, and the Kernel shuts down in reverse order.
	// systemd runs nervud's process lifecycle; shutdown starts with the signal it sends
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// The logic lives in run() so it can return an error: once main calls os.Exit, defers do not run
	if err := run(ctx, *sockPath, *allowSchedDegrade, logger); err != nil {
		logger.Error("nervud exited", "err", err)
		os.Exit(1)
	}
	logger.Info("nervud stopped")
}

// Log level parser
// Function: newLogger
// Parameter: level, a string
// Returns: *slog.Logger
func newLogger(level string) *slog.Logger {
	var lv slog.Level
	if err := lv.UnmarshalText([]byte(level)); err != nil {
		lv = slog.LevelInfo
	}
	h := slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lv})
	return slog.New(h)
}

// laneStopTimeout is how long to wait for every real-time Lane to exit.
// After that, stop waiting and force the process to exit.
// Exiting with an error relies on systemd to restart us; in production, five failed starts should force a reboot.
// The MCU's (microcontroller's) safety mechanism is the last line of defence after the kernel exits.
const laneStopTimeout = 2 * time.Second

// run assembles the kernel and blocks until ctx is cancelled or a module fails to start.
// The assembly steps live in assemble() below, keeping "assemble" and "run and clean up" in separate layers.
//
// Shutdown order:
//
// SIGTERM
//
//	-> Kernel.stopAll stops the modules in reverse (IPC first, audit last)
//	-> k.Run returns
//	-> sched.Shutdown cancels the lane ctx and joins (the defer below)
//	-> run returns and the process exits
//
// Lanes do not listen to the signal ctx; they are controlled by the Scheduler's own ctx.
// Otherwise a Lane and the Kernel would be woken by the same signal in parallel, with no telling which exits first,
// and the Lane's cleanup (revoking authority, confirming the brake, flushing the audit) could be cut short as the process ends.
// Lanes are the lowest layer of infrastructure, so they are reclaimed after every module has stopped.
func run(ctx context.Context, sockPath string, allowSchedDegrade bool, logger *slog.Logger) error {
	sched := scheduler.New(logger, allowSchedDegrade)

	// The defer guarantees that, whether assembly fails or shutdown is normal, Lanes are cancelled and joined
	defer func() {
		sctx, cancel := context.WithTimeout(context.Background(), laneStopTimeout)
		defer cancel()
		if err := sched.Shutdown(sctx); err != nil {
			logger.Error("scheduler: lane not exited within timeout", "err", err)
		}
	}()

	k, err := assemble(ctx, sched, sockPath, logger)
	if err != nil {
		return err
	}

	// Run blocks: it Starts every module in turn; if one fails, it Stops the started ones in reverse and returns the error.
	// Once all are up it waits for ctx.Done(), then Stops them in reverse
	return k.Run(ctx)
}

// assemble builds the foundations and modules of the startup sequence and registers them with the Kernel.
//
// New modules go here:
// k.Register(...), without touching the Kernel or any other module.
// An error means assembly itself failed, and the kernel will not start.
func assemble(ctx context.Context, sched *scheduler.Scheduler, sockPath string, logger *slog.Logger) (*kernel.Kernel, error) {
	// Facility layer: usable as soon as it is constructed
	//
	// Assembly order = dependency order, which has nothing to do with the Kernel's start/stop order
	//
	// audit is currently a facility (Record writes to slog). If it ever needs a writer that starts and stops,
	// register it as a Module as well, in last place: closed last, so shutdown is audited too.
	aud := audit.New(logger)

	auth, err := authority.New(authority.Config{
		Auditor: aud,
		Log:     logger,
		// Leaving Invariants empty means DefaultInvariants(), the production-safe values.
		// To change DataRoot on a development machine, do NOT add a flag: the production binary must not contain
		// any switch that loosens the safety floor (see allowSchedDegrade for where that line was learned)
	})
	if err != nil {
		return nil, err
	}

	// Scheduler: created and owned by run() (it cancels and joins the Lanes); only used here
	//
	// Multi-threading, and binding Linux threads to set their priority
	//
	// How it works:
	//   runtime.LockOSThread() pins a goroutine to a dedicated thread, so Go never moves it and never puts other goroutines on that thread;
	//   then sched_setscheduler(2) sets that thread's policy and priority.
	//
	// scheduler.SpawnDedicated() wraps both steps (see internal/scheduler).
	// Real-time scheduling is Linux only, and the kernel does not start elsewhere; allowSchedDegrade makes an exception for debugging.

	// TODO(rewrite): replace with the real safety.Supervisor loop, and hand ownership of this Lane
	//   to the safety module (it spawns it in Start and stops it in Stop).
	//   Also start a PrioEmergencyStop(99) Stop Lane per sec 6 (zero-allocation hot path).
	if err := sched.SpawnDedicated("safety-supervisor",
		scheduler.PolicyFIFO, scheduler.PrioSafety,
		func(ctx context.Context) {
			<-ctx.Done() // placeholder: exit when the shutdown signal arrives
		}); err != nil {
		return nil, err
	}

	// Modules. Registration order = start order; the Kernel shuts them down in reverse.
	// Each module receives the narrow interface dependencies it needs in New(...) (never global singletons).

	k := kernel.New(logger)

	// auth is injected as a NARROW interface (defined by the consuming package, implicitly satisfied by *Gate) rather than
	// passing the whole *authority.Gate down; the fewer modules hold every method, the more the Gate actually concentrates power.
	//   Example: pkgregistry only needs to install packages, so it defines and receives only that
	//     type PackageInstaller interface {
	//         InstallVerifiedPackage(context.Context, authority.Subject, ...) error
	//
	//
	// TODO(rewrite): wire these in one by one per sec 2, from the bottom up. IPC goes last: before the door opens to the outside,
	// Identity/Permission/Safety must be ready, so there is never a window of access without permission checks.
	//   k.Register(identity.New(...))       // SO_PEERCRED, UID -> Package mapping
	//   k.Register(permission.New(...))     // capability enforcement
	//   k.Register(pkgregistry.New(auth, ...)) // Package Registry + install decisions
	//   k.Register(service.New(auth, ...))  // App/Service component lifecycle
	//   k.Register(endpoint.New(...))       // Endpoint registration/resolution/routing
	//   k.Register(resource.New(...))       // Resource Registry + Provider binding
	//   k.Register(control.New(...))        // HUMAN/AI ControlLease + motion epoch
	//   k.Register(safety.New(sched, ...))  // Safety Gate + StopProgress (starts its Lane with sched above)
	//   k.Register(ipc.New(sockPath, ...))  // control-plane UDS, depends on everything above being ready
	_ = sockPath
	_ = auth

	return k, nil
}
