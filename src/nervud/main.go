package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"os/user"
	"strings"
	"syscall"
	"time"

	"github.com/nervus-os/nervud/internal/admin"
	"github.com/nervus-os/nervud/internal/adminwire"
	"github.com/nervus-os/nervud/internal/audit"
	"github.com/nervus-os/nervud/internal/authority"
	"github.com/nervus-os/nervud/internal/authority/systemd"
	"github.com/nervus-os/nervud/internal/catalog"
	"github.com/nervus-os/nervud/internal/control"
	"github.com/nervus-os/nervud/internal/endpoint"
	"github.com/nervus-os/nervud/internal/health"
	"github.com/nervus-os/nervud/internal/identity"
	"github.com/nervus-os/nervud/internal/ipc"
	"github.com/nervus-os/nervud/internal/kernel"
	"github.com/nervus-os/nervud/internal/logging"
	"github.com/nervus-os/nervud/internal/motiongate"
	"github.com/nervus-os/nervud/internal/operation"
	"github.com/nervus-os/nervud/internal/permission"
	"github.com/nervus-os/nervud/internal/permissionadmin"
	"github.com/nervus-os/nervud/internal/pkgregistry"
	"github.com/nervus-os/nervud/internal/power"
	"github.com/nervus-os/nervud/internal/preflight"
	"github.com/nervus-os/nervud/internal/resource"
	"github.com/nervus-os/nervud/internal/resourcedir"
	"github.com/nervus-os/nervud/internal/safety"
	"github.com/nervus-os/nervud/internal/scheduler"
	"github.com/nervus-os/nervud/internal/service"
	"github.com/nervus-os/nervud/internal/transfer"
)

// safetyTripAdapter connects service.SafetyEscalator to safety.Module: when a Vital component's circuit breaker trips,
// it triggers one Safety latch. It uses ReasonSupervisorEscalation, because a supervisor escalation is exactly
// a critical component crashing repeatedly and the service supervision chain escalating it to a stop.
// An adapter rather than having service import safety, so safety's ReasonCode semantics don't leak into
// the service package.
type safetyTripAdapter struct{ s *safety.Module }

func (a safetyTripAdapter) Trip() { a.s.Trip(safety.ReasonSupervisorEscalation) }

func main() {
	// Startup flags (none in production)
	// The control-plane IPC entry point. Always /run/nervus/nervud.sock in the production image;
	// the flag is only for development.
	sockPath := flag.String("sock", "/run/nervus/nervud.sock", "IPC socket path")
	transferSockPath := flag.String("transfer-sock", transfer.DefaultSockPath,
		"high-throughput transfer socket path")
	// The admin channel UDS (root-only), for nervusctl to trigger install / uninstall / permission grant and revoke. It must be separate from
	// the App control plane, because the App control plane only accepts App-range UIDs and root ops tools can't connect to it.
	adminSockPath := flag.String("admin-sock", adminwire.DefaultSockPath, "privileged admin channel socket path")
	logLevel := flag.String("log-level", "info", "Log level: debug/info/warn/error")
	// Development machines only (Linux without CAP_SYS_NICE): allow running degraded when setting real-time priority fails.
	// Disabled in production, where Linux without CAP_SYS_NICE exits straight away.
	allowSchedDegrade := flag.Bool("dev-allow-sched-degrade", false,
		"[DEV] Allow real-time priority setting failure to downgrade to normal priority")
	// Development machines only: skip the filesystem preflight. The production image must run it, and a missing path or wrong mode is fatal;
	// development machines lack read-only image paths such as /usr/libexec/nervus and must skip it explicitly to start.
	skipPreflight := flag.Bool("dev-skip-preflight", false,
		"[DEV] Skip the filesystem preflight self-check (production must run it)")
	// Development machines only: with no embedded platform root, use the public key embedded in system packages' manifest.sig as the trust anchor.
	// Signature checks, key_id and digests still all run; the only thing relaxed is "was this key authorised by the platform root".
	// Without it, every system package in a development build fails closed to Ordinary, and
	// perm.service.register needs OEM, so no system service exporting a public interface could register.
	devTrustSystemPackages := flag.Bool("dev-trust-system-packages", false,
		"[DEV] Anchor system-image packages on their own embedded signer keys (no platform root)")
	// Audit directory. Always /var/lib/nervus/audit in production (created by preflight with mode 0700).
	//
	// On a development machine that path may not be writable, and "can't open the audit, can't start" doesn't relax into a silent
	// downgrade: a system that runs but records no audit is worse than one that won't start. So there's an explicit switch,
	// rather than letting it quietly become slog-only in some environments.
	auditDir := flag.String("audit-dir", audit.DefaultDir, "audit chain directory")
	flag.Parse()

	logger, closeLog := newLogger(*logLevel)
	slog.SetDefault(logger)

	// Root context: bound to SIGINT/SIGTERM. Signal -> ctx cancelled -> Kernel shuts down in reverse order.
	// systemd runs nervud's process lifecycle; shutdown is triggered by its signals.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// After the first signal triggers a graceful shutdown, restore the default signal behaviour so a second signal can force the process
	// to end. Otherwise, if the graceful shutdown hangs, later SIGTERM/SIGINT are still captured by NotifyContext and
	// do nothing, leaving ops only SIGKILL. This gives an escape hatch when shutdown gets stuck.
	go func() {
		<-ctx.Done()
		stop()
	}()

	// The logic lives in run so it can return an error: once main calls os.Exit, defers don't run.
	err := run(ctx, *sockPath, *transferSockPath, *adminSockPath, *auditDir,
		*allowSchedDegrade, *skipPreflight, *devTrustSystemPackages, logger)
	if err != nil {
		logger.Error("nervud exited", "err", err)
	} else {
		logger.Info("nervud stopped")
	}

	// Drain the async log before os.Exit. closeLog has its own flush timeout: even if stderr is
	// completely stuck, it returns within the limit, so the exit path can't get stuck on logging a second time.
	// Both of these log lines are written (queued asynchronously) before closeLog and drained when it closes.
	if n := closeLog(); n > 0 {
		// Record the drop count through a synchronous bare handler: the async writer has stopped by now,
		// so this line must take a path that doesn't depend on it, or it would be dropped itself.
		slog.New(slog.NewTextHandler(os.Stderr, nil)).
			Warn("nervud: some log lines were dropped under back-pressure", "dropped", n)
	}

	if err != nil {
		os.Exit(1)
	}
}

// logQueueDepth is how many entries the async log queue can hold.
//
// 512 is enough to absorb bursts (every module logging at startup, dense records at a fatal error)
// without using much memory. Beyond that, entries are dropped and counted; the writing side is never blocked.
const logQueueDepth = 512

// newLogger builds an asynchronous, non-blocking logger.
//
// The second return value is the close function: it drains and stops the background writer and returns the number of dropped entries.
// It must be called once before the process exits. The writer under TextHandler is an AsyncWriter, so
// RT Lanes, fatal and shutdown paths are never blocked by a slow stderr when logging.
func newLogger(level string) (*slog.Logger, func() uint64) {
	var lv slog.Level
	if err := lv.UnmarshalText([]byte(level)); err != nil {
		lv = slog.LevelInfo
	}
	aw := logging.NewAsyncWriter(os.Stderr, logQueueDepth)
	h := slog.NewTextHandler(aw, &slog.HandlerOptions{Level: lv})
	closeFn := func() uint64 {
		// Close first, then read the count: once Close closes the stop channel, Write no longer takes the drop branch
		//  (it writes synchronously instead), so the count after closing is the final value.
		_ = aw.Close()
		return aw.Dropped()
	}
	return slog.New(h), closeFn
}

// laneStopTimeout caps the wait for every real-time Lane to exit.
// On timeout it stops waiting and forces the process to exit.
// An error exit relies on systemd to restart; in production, after 5 failed starts the system should be forced to reboot,
// and the MCU (microcontroller) safety mechanism is the backstop once the kernel has exited.
const laneStopTimeout = 2 * time.Second

// run assembles the kernel and blocks until ctx is cancelled or a module fails to start.
// The assembly steps are split out into assemble below, keeping assembly and run/cleanup in separate layers.
//
// # Shutdown order
//
// # SIGTERM
//
// -> Kernel.stopAll stops modules in reverse (IPC closes first, audit last)
// -> k.Run returns
// -> sched.Shutdown cancels the lane ctx and joins (the defer below)
// -> run returns and the process exits
//
// Lanes don't listen to the signal ctx; they're controlled by the Scheduler's own ctx.
// Otherwise Lanes and the Kernel would be woken in parallel by the same signal, with no telling which exits first, and the Lanes' cleanup
// (revoking, brake confirmation, flushing the audit) could be cut off when the process ends.
// Lanes are the lowest layer of infrastructure, so they're reclaimed only after every module has stopped.
func run(
	ctx context.Context,
	sockPath, transferSockPath, adminSockPath, auditDir string,
	allowSchedDegrade, skipPreflight, devTrustSystemPackages bool,
	logger *slog.Logger,
) (err error) {
	sched := scheduler.New(logger, allowSchedDegrade)

	// The defer guarantees Lanes are cancelled and waited for, whether assembly fails or shutdown is normal.
	// A failed Lane reclaim (revoking / brake confirmation / audit flush didn't finish) must show in the exit code,
	// otherwise systemd sees exit 0 and thinks everything was clean; the named return value err carries it out.
	defer func() {
		sctx, cancel := context.WithTimeout(context.Background(), laneStopTimeout)
		defer cancel()
		if serr := sched.Shutdown(sctx); serr != nil {
			logger.Error("scheduler: lane not exited within timeout", "err", serr)
			if err == nil {
				err = fmt.Errorf("scheduler shutdown: %w", serr)
			}
		}
	}()

	k, cleanup, aerr := assemble(
		ctx, sched, sockPath, transferSockPath, adminSockPath, auditDir,
		skipPreflight, devTrustSystemPackages, logger)
	if aerr != nil {
		return aerr
	}
	// cleanup releases infrastructure resources such as the systemd D-Bus connection. It runs after k.Run returns (every module stopped,
	// including the service module that uses the spawner), before the scheduler defer above (last in, first out).
	defer cleanup()

	// Run blocks: Start every module in turn; if any fails, Stop the started ones in reverse and return the error.
	// Once all have started, wait for ctx.Done, then Stop in reverse.
	return k.Run(ctx)
}

// assemble creates the foundations and modules for the start sequence and registers them with the Kernel.
//
// New modules go here:
// k.Register(...), with no change to the Kernel or other modules.
// A returned error means assembly already failed, and kernel startup stops.
func assemble(
	ctx context.Context,
	sched *scheduler.Scheduler,
	sockPath, transferSockPath, adminSockPath, auditDir string,
	skipPreflight, devTrustSystemPackages bool,
	logger *slog.Logger,
) (*kernel.Kernel, func(), error) {
	// cleanup gathers infrastructure resources that need releasing at shutdown (currently only the systemd D-Bus connection).
	// Never nil, and safe to call even when a step of assembly failed.
	var closers []func()
	cleanup := func() {
		for i := len(closers) - 1; i >= 0; i-- {
			closers[i]()
		}
	}

	// Filesystem preflight: runs before any infrastructure or module is built. A mismatch in the read-only image area is fatal;
	// in the writable area (/var/lib/nervus/*, /run/nervus) missing paths are created and wrong modes repaired. It's the kernel's
	// power-on self-test of its own foundations, and must happen before any disk write by audit/authority/pkgregistry.
	if !skipPreflight {
		if err := preflight.Run(preflight.DefaultConfig(logger)); err != nil {
			return nil, cleanup, err
		}
	} else {
		logger.Warn("preflight: SKIPPED (dev-skip-preflight) - never in production")
	}

	// Infrastructure layer: usable as soon as it's built.
	//
	// Construction order = dependency order, unrelated to the Kernel's start/stop order.
	//
	// Audit: an append-only hash-chained file + a slog mirror.
	//
	// If it can't open, nervud doesn't start. A system that runs but records no audit is worse than one that won't start:
	// the former makes people think there's an audit, and afterwards there's nothing to look at.
	//
	// It isn't registered as a Module: a Module's Stop is in the reverse shutdown chain, and the audit must live until
	// after the last shutdown record is written. It's closed with a defer, which runs after cleanup.
	aud, err := audit.NewFileRecorder(audit.FileConfig{
		Dir: auditDir,
		Log: logger,
	})
	if err != nil {
		return nil, cleanup, err
	}
	closers = append(closers, func() {
		if n := aud.Dropped(); n > 0 {
			// Drops mean there's a gap in the chain. It's already written to the file as a ChainGap; reporting it again
			// here lets ops see it directly in the journal without reading the audit file.
			logger.Warn("audit: records were dropped under back-pressure", "dropped", n)
		}
		if cerr := aud.Close(); cerr != nil {
			logger.Error("audit: close", "err", cerr)
		}
		// Read after Close: Close writes the final record and does the last fsync, and both should be counted.
		//
		// The records/syncs ratio shows how well batching works. If the two are close, nearly every record was
		// Denied. That isn't a configuration problem; something keeps being refused and is worth looking at.
		records, syncs := aud.Stats()
		logger.Info("audit: closed", "records", records, "syncs", syncs)
	})

	// systemd D-Bus connection: the backend for starting processes (StartSandboxedProcess).
	// If it can't connect (no D-Bus on a development machine / insufficient privilege) it fails closed: spawner=nil, and authority's
	// process-starting operations return ErrUnsupportedPlatform, while non-process operations (install, tree deletion, setting owners) still work.
	// The same trade-off as the trust store: a missing capability degrades, and never pretends to succeed.
	var spawner authority.UnitManager
	dialCtx, dialCancel := context.WithTimeout(ctx, 5*time.Second)
	sdConn, sderr := systemd.Dial(dialCtx)
	dialCancel()
	if sderr != nil {
		logger.Warn("systemd: dbus unavailable; process launch disabled", "err", sderr)
	} else {
		spawner = sdConn
		closers = append(closers, func() { _ = sdConn.Close() })
	}

	auth, err := authority.New(authority.Config{
		Auditor:      aud,
		Log:          logger,
		Spawner:      spawner,
		DeviceGroups: deviceGroupsFromEnv(logger),
		// Leaving Invariants empty = DefaultInvariants, the production-safe values.
		// Even if a development machine wants a different DataRoot, don't add a flag: a production binary must not expose
		// a switch that changes the baseline for directory ownership and permission checks.
	})
	if err != nil {
		return nil, cleanup, err
	}

	// Scheduler: created and owned by run (it cancels and joins the Lanes); it's only used here.
	//
	// The multi-threading + binding Linux threads with priorities part.
	//
	// How it works:
	//  runtime.LockOSThread pins a goroutine to a dedicated thread, so Go doesn't move it or put other goroutines on that thread;
	//  then sched_setscheduler(2) sets the policy and priority on that thread.
	//
	// scheduler.SpawnDedicated puts thread binding and scheduling setup inside the same lifecycle boundary.
	// Real-time scheduling is Linux-only. The kernel can't start on non-Linux; allowSchedDegrade allows it as an exception for debugging.

	// The motion epoch and the Safety latch must live in the same atomic core, so state and generation number can't tear.
	// main builds it once and injects the same instance into safety and control, which share one revocation generation number:
	// safety latches and increments it when it trips, control increments it on lease lifecycle events, and neither owns it.
	// The three RT Lanes (Stop FIFO 95 / Supervisor FIFO 90 / Control RR 40) are owned by their modules
	//  (spawned in Start, stopped in Stop); no placeholder Lanes are hung here.
	gate := motiongate.New()

	// Modules. Registration order = start order; the Kernel shuts down in reverse.
	// Each module receives the narrow interface dependencies it needs in New(...) (rather than global singletons).

	k := kernel.New(logger)

	// auth is injected through narrow interfaces (defined by the consumer package, satisfied implicitly by *Gate), rather than passing
	// the whole *authority.Gate down. The fewer modules get every method, the more the Gate concentrates power.
	//  For example, pkgregistry only needs install capability, so it defines and receives only
	//  type PackageInstaller interface {
	//  InstallVerifiedPackage(context.Context, authority.Subject,...) error
	//  }
	//
	// identity.Registry has no Module shell yet (no Name/Start/Stop), so for now it's
	// simply created as a library with New and handed to pkgregistry as the receiver of its full projection.
	// Once identity has its own Module shell, this should take the same instance from the Kernel instead.
	idReg := identity.NewRegistry()

	// pkgregistry's own authoritative Registry: the full state of install / uninstall / boot scan exists only here,
	// not held separately from the Module, otherwise the two instances would fork into invisible state differences.
	pkgReg := pkgregistry.NewRegistry()

	// One immutable catalog snapshot is shared by package loading, endpoint
	// resolution, permissions, resources, and the IPC method gate. New Provider
	// interfaces and methods enter through signed artifacts; none of these
	// consumers keeps a capability-specific fallback table.
	definitions, err := catalog.NewDefaultRegistry()
	if err != nil {
		return nil, cleanup, fmt.Errorf("create definition catalog: %w", err)
	}
	permReg := permission.NewRegistry(definitions)

	transferMgr, err := transfer.New(transfer.Config{
		SockPath: transferSockPath,
		Log:      logger,
	})
	if err != nil {
		return nil, cleanup, err
	}
	// Runtime grant state needs persistence and linking to revocation of dangerous GrantUser permissions,
	// which needs control as the LeaseRevoker, and ctl is only built further down. So SetGrantStore moves to
	// after ctl is built (see there); it isn't wired here, to avoid loading once with a nil revoker and then resetting.

	// IPC registers last: Identity/Permission/Safety must be ready before the doors open,
	// so there's no window of access without permission checks.
	// Root of trust: the embedded platform root verifies the bundle under /usr/share/nervus/trust.
	// A load failure (a development build missing the embedded root, a missing bundle, or one that fails verification) doesn't block startup; it fails closed:
	// a zero TrustStore is passed in, so developer self-signatures still verify, platform/oem are always refused, and dynamic installs
	// can only get Ordinary. That's consistent with "fail closed when verification fails, never pretend it passed".
	trustStore, terr := pkgregistry.LoadTrustStore(pkgregistry.DefaultTrustDir)
	if terr != nil {
		logger.Warn("pkgregistry: trust store unavailable; non-Ordinary trust disabled", "err", terr)
		// Development downgrade: use the embedded public key in system packages as the anchor. Only tried when there's really no usable trust store:
		// once the production trust store loads, this switch has no effect at all, so there's no path for "a flag overriding
		// a verified root of trust".
		if devTrustSystemPackages {
			devStore, derr := pkgregistry.LoadDevTrustStore(
				pkgregistry.DefaultSystemPackagesDir, logger)
			if derr != nil {
				logger.Warn("pkgregistry: dev trust anchors unavailable; staying fail-closed",
					"err", derr)
			} else {
				trustStore = devStore
				logger.Warn("pkgregistry: DEV TRUST ACTIVE (dev-trust-system-packages) - never in production")
				// Audit rather than just a log line: this changes the premise of the whole machine's trust decisions, so it must stay in
				// a channel the log level can't filter.
				aud.Record(ctx, audit.Event{
					Action:  "pkgregistry.DevTrustAnchors",
					Subject: "kernel",
					Detail:  "system-image packages anchored on embedded signer keys (no platform root)",
				})
			}
		}
	}
	pkgMod := pkgregistry.New(auth, idReg, permReg, pkgReg, definitions, trustStore, aud, logger,
		pkgregistry.DefaultRegistryStateDir, pkgregistry.DefaultSystemPackagesDir,
		authority.DefaultInvariants().PackageRoot, authority.DefaultInvariants().DataRoot,
	)
	k.Register(pkgMod)                  // Package Registry + install decisions
	k.Register(permission.New(permReg)) // capability enforcement: the Grant projection is pushed by pkgregistry

	// Resource lookup reads the same catalog revision used by endpoint routing.
	resMod := resource.New(definitions)
	k.Register(resMod)

	// HUMAN/AI ControlLease + deadman + Control Lane (RR 40). Reads/increments the same gate as safety.
	// Registered before safety = starts earlier, stops later: at shutdown safety finishes first and control revokes leases last.
	//
	// Construction order is also dependency order: safety needs it as the LeaseRevoker, and control doesn't depend on safety
	//  (a deadman failure only revokes leases + increments the epoch, without latching Safety), so the wiring is one-way and acyclic.
	ctl := control.New(sched, gate, aud, logger, control.DefaultPolicy())
	k.Register(ctl)

	// With ctl ready, wire up runtime grant state: stateDir for persistence + revoker=ctl. When a motion-group permission is revoked,
	// permission calls ctl.RevokeByPackage through that revoker to revoke the package's actuator leases (control increments the
	// motion epoch). SetGrantStore is here rather than where permReg is built because it
	// needs ctl: permReg is built first (pkgregistry's install decisions need it) and ctl later, hence the two-stage wiring.
	// AcquireControl/ReleaseControl are already connected to the same ctl by IPC, so a revocation immediately invalidates existing
	// wire leases, rather than only affecting the next request.
	permReg.SetGrantStore(pkgregistry.DefaultRegistryStateDir, ctl, aud)

	// Safety Gate + Stop Lane (FIFO 95) + Supervisor (FIFO 90): the module owns two RT Lanes.
	// It must be ready before IPC (Safety must be armed before the doors open). v1: there's no real Provider, so delivery uses
	// NopPath and reporting uses NopReports; the LeaseRevoker is control. Incrementing the motion epoch is still the main
	// revocation mechanism; ctl.RevokeAll only clears the lease objects themselves (it no longer increments the epoch on top).
	safetyMod := safety.New(
		sched, gate, aud, logger,
		safety.DefaultContract(), safety.NopPath(), safety.NopReports(), ctl,
	)
	k.Register(safetyMod)

	// Service Manager: starts enabled components from the Registry as sandboxed processes through authority -> systemd,
	// watches for crashes and handles them by criticality, and maintains the unit -> component reverse index that unlocks
	// ipc.verifyComponent. Registered after safety and before ipc: on the way up,
	// Safety is armed before external processes may run; shutdown goes in reverse ipc -> service -> safety: stop taking callers, then stop
	// external processes (no more sources of motion commands), and stop safety last.
	svcMgr := service.New(auth, pkgReg, permReg, safetyTripAdapter{safetyMod}, aud, logger,
		authority.DefaultInvariants())
	// The install service can additionally write to the staging root: nervud creates stage-* directories there for it to unpack into,
	// while the sandbox's ProtectSystem=strict makes the whole filesystem read-only. This is the only exception of its kind,
	// written out explicitly at wiring, and there's no second place that can hand out a writable path.
	//
	// Who gets it is decided by perm.pkg.admin, not by package name. The kernel knows no specific
	// Package ID, and the criterion is the same as admission to the admin channel.
	svcMgr.GrantStagingAccess(admin.DefaultStagingDir)
	// Shared area between services: every package has a subdirectory under each of two roots, owned by itself with mode 0755.
	// preflight creates the roots, pkgregistry's boot scan creates the per-package subdirectories, and service puts this package's
	// subdirectory in ReadWritePaths. All three use the same Invariants, so the paths can't diverge.
	pkgMod.SetSharedRoots(
		authority.DefaultInvariants().SharedRuntimeRoot,
		authority.DefaultInvariants().SharedPersistRoot,
	)
	k.Register(svcMgr)

	// Health aggregator: reads the three authoritative sources (safety/control/service) live and combines them into one health level for the machine.
	// Registered after safety/control/service are built and before endpoint/ipc: it needs instances of all three
	// (it doesn't depend on their Start; Report computes on read).
	//
	// Gap (for whoever finishes this): *service.Manager doesn't yet implement the
	// Instances []service.Instance that health.ServiceObserver requires, so it can't be injected as the third observer. The third argument here is nil,
	// to avoid widening service's interface just for wiring. health is fail-safe for a nil observer (that dimension takes part
	// with zero values, and deriveStatus's fail-closed rule means it never wrongly reports Healthy without seeing Safety).
	// Once *service.Manager has Instances, replace the nil with svcMgr.
	healthMod := health.New(safetyMod, ctl, nil)
	k.Register(healthMod)

	// Inject the external collaborators uninstall/disable need into pkgregistry: service stops components; control revokes leases.
	// Uninstalling a Package revokes the actuator leases under that package through ctl.RevokeByPackage (if motion is involved,
	// control increments the motion epoch). IPC leases are wired, so existing leases are revoked synchronously during uninstall.
	pkgMod.SetLifecycleHooks(svcMgr, ctl)

	// Endpoint registration/resolution/routing must be registered after service and before IPC,
	// because when Resolve starts an on-demand component it depends on svcMgr.EnsureStarted.
	epMod := endpoint.New(definitions, pkgReg, permReg, svcMgr, aud, logger)

	// Register built-in endpoints: implemented by nervud itself, not Interfaces of external Services.
	//
	// Safety observation and re-arm naturally live in the kernel and can't be provided by a Provider; they act directly
	// on kernel state. But Apps and the recovery service need them, and the only call shape on the control plane is
	// ResolveEndpoint -> Request -> Response.
	//
	// envelope.proto shuts the door on "add a new body" ("anything that isn't one of those eight things should be
	// a method on some Interface"), so it goes through built-in endpoints: callers use completely standard
	// Resolve+Request and neither know nor need to know whether the other side is the kernel or a Provider.
	//
	// A registration failure at wiring is a hard error: a system missing its Safety recovery channel shouldn't be trusted even if it starts,
	// because once the machine stops it can never be released, short of restarting the whole kernel.
	if err := epMod.RegisterBuiltin(
		safety.BuiltinInterfaceID, 1, 0, safetyMod.BuiltinHandler(),
	); err != nil {
		return nil, cleanup, fmt.Errorf("register builtin %s: %w", safety.BuiltinInterfaceID, err)
	}
	logger.Info("endpoint: builtin registered", "interface", safety.BuiltinInterfaceID)

	// Whole-machine power (orderly reboot/shutdown). Also built in: what really executes it is the Authority Gate,
	// only nervud has that privilege, and no external Provider could offer it.
	//
	// A registration failure is a hard error here too: a system that's up but can't be shut down leaves the user only pulling the plug,
	// and avoiding pulled plugs is exactly why this channel exists (to avoid filesystem damage from abnormal power loss).
	//
	// Hosted mode (NervusOS as a session on an existing desktop such as Ubuntu): power off / reboot are routed
	// to an external hook and mean "exit / restart NervusOS"; never the whole host machine. See power/hosted.go
	var powerGate power.Gate = auth
	if hook := os.Getenv(power.HostedHookEnv); hook != "" {
		powerGate = power.NewHostedGate(hook, aud, logger)
		logger.Warn("power: hosted mode, power actions end/restart the NervusOS session only", "hook", hook)
	}
	powerMod := power.New(powerGate, logger)
	if err := epMod.RegisterBuiltin(
		power.BuiltinInterfaceID, 1, 0, powerMod.BuiltinHandler(),
	); err != nil {
		return nil, cleanup, fmt.Errorf("register builtin %s: %w", power.BuiltinInterfaceID, err)
	}
	logger.Info("endpoint: builtin registered", "interface", power.BuiltinInterfaceID)

	// Resource directory: the Catalog's own read-only view, filling the gap of enumerating "what devices are there".
	// Without it, Apps could only probe with repeated Resolve calls, and a failed Resolve can't tell
	// "no such device" from "there is one but I'm not allowed".
	//
	// A registration failure is a hard error here as well, but for a different reason than the two above: it isn't a safety channel,
	// it's a sign that the bootstrap contract and this binary disagree. The interface is in the Catalog but its handler
	// won't install, so the two have drifted, and no later Resolve result can be trusted.
	resourceDirMod := resourcedir.New(definitions, logger)
	if err := epMod.RegisterBuiltin(
		resourcedir.BuiltinInterfaceID, 1, 0, resourceDirMod.BuiltinHandler(),
	); err != nil {
		return nil, cleanup, fmt.Errorf(
			"register builtin %s: %w", resourcedir.BuiltinInterfaceID, err)
	}
	logger.Info("endpoint: builtin registered", "interface", resourcedir.BuiltinInterfaceID)

	// Grant surface: reading and writing USER_CONSENT grant state. It fills the "installed but can never get it"
	// gap: before it, the only thing in the system that could change runtime grant state was
	// SetRuntimeState on the admin channel, which in practice only nervusctl could use, so sensitive permissions requested by ordinary apps
	// had no UI that could approve them.
	//
	// The reason the kernel implements it is the same as the resource directory: grant state is permission.Registry's own
	// state, and delegating it to a replaceable service would bypass the platform-release bar of
	// perm.permission.admin. A registration failure is also a hard error (contract and binary have drifted).
	permissionAdminMod := permissionadmin.New(definitions, pkgReg, permReg, logger)
	if err := epMod.RegisterBuiltin(
		permissionadmin.BuiltinInterfaceID, 1, 0, permissionAdminMod.BuiltinHandler(),
	); err != nil {
		return nil, cleanup, fmt.Errorf(
			"register builtin %s: %w", permissionadmin.BuiltinInterfaceID, err)
	}
	logger.Info("endpoint: builtin registered", "interface", permissionadmin.BuiltinInterfaceID)

	// Self-check surface: the second interface of the same Module. Before calling a method that needs a sensitive permission, an app asks
	// "do I have it?" (like Android's checkSelfPermission), and requests it if not.
	//
	// It shares the Module with the one above, but the bars are completely different: .admin needs perm.permission.admin
	// (SYSTEM_ONLY + platform-release), while the self-check has none. The target is always the caller itself, decided by
	// BuiltinCall.Caller, and the request has no package_id. The bars are written in the two bootstrapInterface calls
	// in the catalog bootstrap, not here.
	if err := epMod.RegisterBuiltin(
		permissionadmin.SelfBuiltinInterfaceID, 1, 0, permissionAdminMod.SelfBuiltinHandler(),
	); err != nil {
		return nil, cleanup, fmt.Errorf(
			"register builtin %s: %w", permissionadmin.SelfBuiltinInterfaceID, err)
	}
	logger.Info("endpoint: builtin registered", "interface", permissionadmin.SelfBuiltinInterfaceID)

	k.Register(epMod)

	// Operation Manager: gives system-coordinated long tasks such as arm trajectories / homing / moving to a pose a state machine and handle owned by
	// nervud. Registered after endpoint and before ipc: operations only make sense to create by dispatch once Resolve/Route
	// are ready, and the state machine must be ready before IPC opens. resource (resMod.Valid checks resource_handle) and audit
	// are injected through narrow interfaces.
	//
	// The LeaseValidator is injected after ipc is wired (see the SetLeaseValidator section below):
	// unwrapping wire lease handles needs the conn mapping table, which lives in ipc.
	//
	// Passing nil here isn't a leftover; it's construction order: operation must register after endpoint
	// and before ipc (operations only make sense to create by dispatch once Resolve/Route are ready,
	// and the state machine must be ready before IPC opens), while ipc can only be built after operation.
	// In between, the lease validator is nil and motion operations fail closed.
	opMod := operation.New(resMod, nil, aud, logger)
	k.Register(opMod)

	// The transfer manager owns the separate high-throughput Unix socket. It
	// starts before IPC accepts control calls and stops after IPC, so no control
	// route can issue a handle without a live data plane.
	k.Register(transferMgr)

	// IPC control-plane UDS: opens last. It depends on everything above being ready (Identity/Permission/Safety/Service/
	// Endpoint). Components connects svcMgr to unlock verifyComponent; Endpoints connects epMod to unlock
	// ResolveEndpoint/RegisterEndpoint/UnregisterEndpoint and the Route lookup for Requests.
	// Only from here can Apps/Services really complete a handshake.
	ipcSrv, err := ipc.New(ipc.Config{
		SockPath:   sockPath,
		Log:        logger,
		Auditor:    aud,
		Invariants: authority.DefaultInvariants(),
		Identity:   idReg,
		Permission: permReg,
		Components: svcMgr,
		Endpoints:  epMod,
		// Leases connects AcquireControl/ReleaseControl (envelope 70-73). Before this,
		// the control module was complete but had no entry point, so Apps couldn't get a motion lease.
		Leases: ctl,
		// Resources lets AcquireControl's selector resolve to a resource_handle,
		// using the same table and the same matching rules as ResolveEndpoint. An empty selector in v2
		// no longer has an implicit default, so both paths fail closed together.
		Resources: resMod,
		// Operations connects long tasks: methods declaring returns_operation are only accepted
		// from here on. When it's nil they're all refused, rather than degrading into ordinary calls, which would give the caller
		// an OK while the machine is still moving.
		Operations: opMod,
		Transfer:   transferMgr,
		// Launcher connects LaunchComponent (envelope 80/81): the Launcher opening an App and
		// the session service bringing up the desktop at boot both go through it. Before this, the only way to start a component
		// was endpoint.Resolve starting an on-demand provider, so "launch an app" had to disguise itself as
		// "resolve an interface": audit couldn't tell the two apart, and pure UI apps without an interface couldn't start at all.
		Launcher: svcMgr,
	})
	if err != nil {
		return nil, cleanup, err
	}
	// Runtime revocation must close matching Dispatch route tokens before it
	// scans Transfer records. IPC owns that coordination boundary; wiring the
	// raw Transfer manager here would leave a revoke-then-Begin race.
	permReg.SetPermissionRevoker(ipcSrv)
	// Install-grant removal may run inside a package transaction. Service only
	// records a supervisor stop intent on this hook; it never waits on systemd or
	// looks the package up while the transaction lock is held.
	permReg.SetInstallGrantRevoker(svcMgr)
	// USER_CONSENT changes also alter process sandbox mounts. This separate hook
	// is never called by permission.Replace, so a package transaction cannot wait
	// on systemd or accidentally restart a package while rolling back.
	permReg.SetRuntimePermissionProjector(svcMgr)
	ctl.SetLeaseObserver(ipcSrv)
	pkgMod.SetTransferRevoker(ipcSrv)
	if err := epMod.RegisterBuiltin(
		catalog.InterfaceTransferControl, 1, 0, ipcSrv.TransferBuiltinHandler(),
	); err != nil {
		return nil, cleanup, fmt.Errorf(
			"register builtin %s: %w", catalog.InterfaceTransferControl, err)
	}
	logger.Info("endpoint: builtin registered", "interface", catalog.InterfaceTransferControl)

	// The three operation back-fills must come after ipc is built: the validator and built-in handlers both live in
	// ipc, while operation must register before ipc (the state machine must be ready before IPC opens).
	// Construction order turns the dependencies into a cycle, and a back-fill at wiring breaks it.
	//
	// The order matters: install the handler first (it sets up operationWire), then register the endpoint
	// to get the handle, and only then connect the event bypass, which uses that handle as its fan-out key.
	opMod.SetLeaseValidator(ipcSrv.OperationLeaseValidator())
	if err := epMod.RegisterBuiltin(
		catalog.InterfaceOperationControl, 1, 0, ipcSrv.OperationBuiltinHandler(),
	); err != nil {
		return nil, cleanup, fmt.Errorf(
			"register builtin %s: %w", catalog.InterfaceOperationControl, err)
	}
	if err := epMod.RegisterBuiltinSubscriber(
		catalog.InterfaceOperationControl, 1, ipcSrv.OperationSubscribeAdmitter(),
	); err != nil {
		return nil, cleanup, fmt.Errorf(
			"register builtin subscriber %s: %w", catalog.InterfaceOperationControl, err)
	}
	operationEndpointID, ok := epMod.BuiltinEndpointID(catalog.InterfaceOperationControl, 1)
	if !ok {
		return nil, cleanup, fmt.Errorf(
			"builtin %s has no endpoint id after registration",
			catalog.InterfaceOperationControl)
	}
	ipcSrv.SetOperationEndpointID(operationEndpointID)
	opMod.SetEventObserver(ipcSrv.OperationEventObserver())
	logger.Info("endpoint: builtin registered", "interface", catalog.InterfaceOperationControl)

	k.Register(ipcSrv)

	// Privileged admin channel (root-only UDS): for nervusctl to trigger install / uninstall / disable and enable / permission grant and revoke.
	// Registered after ipc = starts later, stops earlier: at shutdown the admin plane stops first (no new install / permission
	// commands), then the App control plane. It drives the same in-process pkgregistry.Module / permission
	// .Registry and never opens a second writer, so authoritative state can't fork. AdminUID is nervud's own
	// euid (0/root in production): only the ops identity running nervud can send commands; the socket mode and group are set in Start.
	// Leaving StagingRoot empty = admin.DefaultStagingDir (/var/lib/nervus/staging, created by preflight
	// on the same filesystem as PackageRoot, so the install-time renameat2 doesn't cross disks).
	// The install service needs to connect to the admin channel to install for Apps (an App can't be root, and system services
	// run in the App UID range).
	//
	// No Package ID appears here. Who can connect is decided by admin.PermissionPackageAdmin
	//  (perm.pkg.admin), which admin resolves by permission in its own Start. It's registered after
	// pkgregistry, when the boot scan has finished, UIDs are allocated and permissions decided.
	// At wiring none of those three exist yet, so a check written here would be dead code.
	adminSrv, err := admin.New(admin.Config{
		SockPath:    adminSockPath,
		AdminUID:    uint32(os.Geteuid()),
		Packages:    pkgMod,
		Registry:    pkgReg,
		Permissions: permReg,
		Auditor:     aud,
		Log:         logger,
	})
	if err != nil {
		return nil, cleanup, err
	}
	k.Register(adminSrv)

	return k, cleanup, nil
}

// deviceGroupsFromEnv reads NERVUS_DEVICE_GROUPS (comma separated), the host groups given to components
// with device access (see authority.Config.DeviceGroups). Hosted mode sets it to "video" in the nervud
// drop-in. A group the host doesn't have is dropped with a warning: systemd refuses to start a unit that
// names an unknown group, and one missing group must not take down every device Provider.
func deviceGroupsFromEnv(logger *slog.Logger) []string {
	raw := os.Getenv("NERVUS_DEVICE_GROUPS")
	if raw == "" {
		return nil
	}
	var out []string
	for _, g := range strings.Split(raw, ",") {
		g = strings.TrimSpace(g)
		if g == "" {
			continue
		}
		if _, err := user.LookupGroup(g); err != nil {
			logger.Warn("device group not found on this host; skipped", "group", g, "err", err)
			continue
		}
		out = append(out, g)
	}
	if len(out) > 0 {
		logger.Info("device providers get supplementary groups", "groups", out)
	}
	return out
}
