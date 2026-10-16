// Package systemd is nervud's D-Bus client wrapper for systemd (Linux PID 1).
//
// Responsibility boundary (D-Bus client dependency):
//   - Only this subpackage may import github.com/godbus/dbus/v5: it's a D-Bus client for systemd
//     and part of the TCB. Pinned to an exact version, go.sum committed, included in the SBOM.
//   - The unit name, paths, UID, env and properties passed to StartTransientUnit get local allowlist
//     checks (see props.go); callers may not pass arbitrary systemd properties.
//   - When D-Bus drops or returns an uncertain result it fails by default, never treating "request sent" as "process safely started".
//
// systemd owns the process lifecycle: this wrapper starts processes as transient .service units (StartTransientUnit)
// and stops them with StopUnit. That departs from the pidfd sketch on purpose: a unit name is a reuse-safe
// stable handle, and systemd doesn't reuse a unit name for another process the way the kernel recycles PIDs, so the
// bare-PID reuse race pidfds are meant to solve doesn't exist under the systemd-managed model.
//
// Why poll rather than confirm job completion through the JobRemoved signal: godbus's shared connection silently drops signals
// when its signal buffer is full, and systemd Manager.Subscribe causes a continuous stream of signals (especially on a degraded system).
// Together, waiting for a job's JobRemoved could either hang until timeout because of a dropped signal, or get the drain logic
// into a livelock. Instead it polls the unit's ActiveState within a ctx-bounded timeout until active/failed. That gives the
// equivalent of "never treat request-sent as started" (even stronger: it directly confirms the unit really is active), with zero dependence on signal
// delivery reliability. The poll interval is far shorter than start/stop latency, and starting/stopping isn't a hot path.
package systemd

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/godbus/dbus/v5"
)

const (
	dbusDest = "org.freedesktop.systemd1"
	dbusPath = dbus.ObjectPath("/org/freedesktop/systemd1")
	mgrIface = "org.freedesktop.systemd1.Manager"

	// startStopPollInterval is the poll interval for confirming starts/stops; it should be fast, since start/stop latency is usually around 100ms
	startStopPollInterval = 50 * time.Millisecond
	// waitPollInterval is the poll interval for waiting for a long-running component to exit; it should be thrifty, since crash detection tolerates
	// seconds of delay, and there's no need to poll every running component's state every 50ms
	waitPollInterval = 1 * time.Second
)

// ErrJobFailed: the unit went into the failed state after a start/stop
var ErrJobFailed = errors.New("systemd: unit entered failed state")

// Conn is one D-Bus connection to systemd.
//
// No signal subscriptions and no internal mutable state: everything is done through ctx-bound method calls + polling, so it's naturally concurrency-safe
// with no lock needed
type Conn struct {
	bus *dbus.Conn
	mgr dbus.BusObject
}

// Dial connects to the system bus
func Dial(_ context.Context) (*Conn, error) {
	bus, err := dbus.ConnectSystemBus()
	if err != nil {
		return nil, fmt.Errorf("systemd: connect system bus: %w", err)
	}
	return &Conn{bus: bus, mgr: bus.Object(dbusDest, dbusPath)}, nil
}

// Close closes the underlying D-Bus connection
func (c *Conn) Close() error {
	if c == nil || c.bus == nil {
		return nil
	}
	return c.bus.Close()
}

// auxUnit matches the type a(sa(sv)) of StartTransientUnit's aux parameter; nervud uses no auxiliary units
// and always passes an empty array, but the type must match to encode correctly
type auxUnit struct {
	Name       string
	Properties []property
}

// StartTransientUnit starts a sandboxed process as a transient .service unit, and polls to confirm
// the unit really became active (or failed) before returning (never treating request-sent as started)
func (c *Conn) StartTransientUnit(ctx context.Context, spec UnitSpec) error {
	props, err := BuildProperties(spec)
	if err != nil {
		return err
	}
	// Clear any leftover unit with the same name before starting.
	//
	// Mode "replace" doesn't do this: it replaces queued jobs and doesn't touch an already
	// loaded unit. If a unit with the same name is still in the failed/inactive state, StartTransientUnit
	// reports "Unit... was already loaded or has a fragment file" straight away.
	//
	// On the normal path CollectMode=inactive-or-failed (see props.go) already has systemd collect the unit the moment
	// the process exits, so there are no leftovers here. This step covers what that can't:
	// nervud itself crashing / being SIGKILLed and restarting, when the previous round's unit may still be there.
	//
	// Best-effort: ResetFailedUnit errors when the unit doesn't exist, which is normal and ignored.
	// Real failures show up on the StartTransientUnit below.
	_ = c.mgr.CallWithContext(ctx, mgrIface+".ResetFailedUnit", 0, spec.Name).Err

	call := c.mgr.CallWithContext(ctx, mgrIface+".StartTransientUnit", 0,
		spec.Name, "replace", props, []auxUnit{})
	if call.Err != nil {
		return fmt.Errorf("systemd: StartTransientUnit %s: %w", spec.Name, call.Err)
	}
	return c.pollUntil(ctx, spec.Name, startStopPollInterval, func(info ExitInfo, exists bool) (bool, error) {
		if !exists {
			// The unit vanished: possibly a very short-lived process that exited right after starting. Treat it as started and let WaitUnit finish up
			return true, nil
		}
		switch info.ActiveState {
		case "active":
			return true, nil
		case "failed":
			return true, fmt.Errorf("%w: %s (result=%s)", ErrJobFailed, spec.Name, info.Result)
		default:
			return false, nil // activating / reloading: keep polling
		}
	})
}

// StopUnit stops a transient unit (systemd sends SIGTERM, then SIGKILL, per the unit's KillSignal/TimeoutStopSec),
// polling to confirm it went inactive/failed or disappeared
func (c *Conn) StopUnit(ctx context.Context, name string) error {
	if !validUnitName(name) {
		return fmt.Errorf("%w: %q", ErrInvalidUnitName, name)
	}
	call := c.mgr.CallWithContext(ctx, mgrIface+".StopUnit", 0, name, "replace")
	if call.Err != nil {
		if isNoSuchUnit(call.Err) {
			return nil // already gone = already stopped (idempotent)
		}
		return fmt.Errorf("systemd: StopUnit %s: %w", name, call.Err)
	}
	return c.pollUntil(ctx, name, startStopPollInterval, func(info ExitInfo, exists bool) (bool, error) {
		if !exists {
			return true, nil
		}
		return info.Terminal(), nil
	})
}

// ExitInfo is a snapshot of a unit's run/exit information
type ExitInfo struct {
	ActiveState string // active / activating / inactive / failed / deactivating
	Result      string // systemd Result: success / exit-code / signal / oom-kill /...
	ExitStatus  int    // ExecMainStatus (the process exit code or signal number)
}

// Terminal reports whether the unit is in a terminal state (no longer running)
func (e ExitInfo) Terminal() bool {
	return e.ActiveState == "inactive" || e.ActiveState == "failed"
}

// WaitUnit blocks until the unit reaches a terminal state (inactive/failed) or disappears, and returns its exit information.
// Used by service.Manager's crash watcher (waiting for a long-running component to exit)
func (c *Conn) WaitUnit(ctx context.Context, name string) (ExitInfo, error) {
	if !validUnitName(name) {
		return ExitInfo{}, fmt.Errorf("%w: %q", ErrInvalidUnitName, name)
	}
	var last ExitInfo
	err := c.pollUntil(ctx, name, waitPollInterval, func(info ExitInfo, exists bool) (bool, error) {
		if !exists {
			// The unit has disappeared (systemd collects transient units after they exit).
			//
			// Keep the last information read rather than overwriting it with an empty ExitInfo: that snapshot holds
			// Result and ExecMainStatus, the only basis for judging "clean exit or crash".
			// Overwriting it would leave the caller only knowing "it's gone", so an app the user closed and an app that crashed
			// would look exactly the same, which is exactly why apps wouldn't close (the kernel restarted the former as a crash too).
			//
			// When it didn't exist from the first poll (last is the zero value), leave it as is: in that case there really is
			// no exit information to speak of.
			last.ActiveState = "inactive"
			return true, nil
		}
		last = info
		return info.Terminal(), nil
	})
	if err != nil {
		return ExitInfo{}, err
	}
	return last, nil
}

// pollUntil reads the unit state every interval and hands it to done to judge, until done returns true
// or ctx ends. done's second argument exists=false means the unit doesn't currently exist
func (c *Conn) pollUntil(ctx context.Context, name string, interval time.Duration, done func(info ExitInfo, exists bool) (bool, error)) error {
	// Judge once immediately, so a start/stop that has already finished doesn't wait a tick for nothing
	if fin, err := c.pollOnce(ctx, name, done); fin || err != nil {
		return err
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("systemd: waiting on unit %s: %w", name, ctx.Err())
		case <-ticker.C:
			if fin, err := c.pollOnce(ctx, name, done); fin || err != nil {
				return err
			}
		}
	}
}

func (c *Conn) pollOnce(ctx context.Context, name string, done func(ExitInfo, bool) (bool, error)) (bool, error) {
	info, exists, err := c.unitState(ctx, name)
	if err != nil {
		return false, err
	}
	return done(info, exists)
}

// unitState reads a unit's ActiveState / Result / ExecMainStatus. When the unit doesn't exist,
// exists=false (NoSuchUnit isn't an error, it's a normal state)
func (c *Conn) unitState(ctx context.Context, name string) (ExitInfo, bool, error) {
	var unitPath dbus.ObjectPath
	call := c.mgr.CallWithContext(ctx, mgrIface+".GetUnit", 0, name)
	if call.Err != nil {
		if isNoSuchUnit(call.Err) {
			return ExitInfo{}, false, nil
		}
		return ExitInfo{}, false, fmt.Errorf("systemd: GetUnit %s: %w", name, call.Err)
	}
	if err := call.Store(&unitPath); err != nil {
		return ExitInfo{}, false, fmt.Errorf("systemd: decode unit path for %s: %w", name, err)
	}

	obj := c.bus.Object(dbusDest, unitPath)
	var info ExitInfo
	active, err := obj.GetProperty("org.freedesktop.systemd1.Unit.ActiveState")
	if err != nil {
		return ExitInfo{}, false, fmt.Errorf("systemd: get ActiveState: %w", err)
	}
	info.ActiveState, _ = active.Value().(string)
	if res, e := obj.GetProperty("org.freedesktop.systemd1.Service.Result"); e == nil {
		info.Result, _ = res.Value().(string)
	}
	if st, e := obj.GetProperty("org.freedesktop.systemd1.Service.ExecMainStatus"); e == nil {
		if v, ok := st.Value().(int32); ok {
			info.ExitStatus = int(v)
		}
	}
	return info, true, nil
}

// isNoSuchUnit reports whether a D-Bus error is systemd's NoSuchUnit
func isNoSuchUnit(err error) bool {
	var derr dbus.Error
	if errors.As(err, &derr) {
		return derr.Name == "org.freedesktop.systemd1.NoSuchUnit"
	}
	return false
}
