// This file is the supervisor loop for component instances and crash handling by severity.
//
// Each running instance is watched exclusively by one supervisor goroutine: start the process -> wait for exit ->
// decide (expected stop / crash) -> restart with backoff or break the circuit by criticality. Blocking calls
// (StartSandboxedProcess / WaitProcess) never hold mu; mu is held only briefly, at the moments
// instance state is read or written, so one stuck systemd call can't lock the whole Manager.
package service

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/nervus-os/nervud/internal/audit"
	"github.com/nervus-os/nervud/internal/authority"
	"github.com/nervus-os/nervud/internal/identity"
	"github.com/nervus-os/nervud/internal/pkgregistry"
)

const (
	restartBackoffMin = 1 * time.Second
	restartBackoffMax = 60 * time.Second
	// crashWindow / crashThreshold: reaching the threshold of crashes within the sliding window breaks the circuit
	crashWindow    = 10 * time.Second
	crashThreshold = 5
	// startStopTimeout caps a single StartSandboxedProcess/StopProcess, so even if
	// systemd/D-Bus gets stuck, a supervisor isn't blocked indefinitely. Waiting for a long-running component
	// to exit (WaitProcess) uses m.ctx instead, with no such cap
	startStopTimeout = 30 * time.Second

	// nervudUnit is nervud's own systemd unit name. Transient component units are BindsTo it,
	// so components are stopped by systemd even if nervud is SIGKILLed
	nervudUnit = "nervud.service"
	// registryDir is nervud's trusted state directory, holding _grants/_devmode/ledger/the uid allocator.
	// Component sandboxes set it as InaccessiblePaths, so no component can read it
	registryDir = "/var/lib/nervus/registry"
	// permStorageUser is the permission needed to access the shared user documents area (Invariants.UserDataRoot).
	// It must have the same name as the entry in the central catalog bootstrap: that's the definition, this is the enforcement point
	permStorageUser = "perm.storage.user"
	// permStorageShared is the permission needed to access the shared area between services (Invariants.SharedRuntimeRoot /
	// SharedPersistRoot). It must have the same name as the entry in the central catalog bootstrap:
	// that's the definition, this is the enforcement point
	permStorageShared = "perm.storage.shared"

	// x11SocketDir is the X11 display server's unix socket directory.
	//
	// Hard-coding the path is right: it's hard-coded by the X11 protocol implementations (libxcb / libX11), and clients connect to
	// /tmp/.X11-unix/X<display> without reading any configuration. Moving it would only stop clients finding it.
	x11SocketDir = "/tmp/.X11-unix"

	// defaultDisplay is the value when DISPLAY isn't given in nervud's environment.
	// A robot is a single-display device, so :0 is the only sensible default
	defaultDisplay = ":0"
)

// displayEnv prepares DISPLAY / XAUTHORITY for graphical components.
//
// The values are passed through from nervud's own environment (given by the systemd unit or the image's environment file)
// rather than hard-coded: DISPLAY differs with an external screen, under WSLg, or with Wayland's Xwayland, and
// hard-coding it would nail the deployment shape into the kernel. When DISPLAY is missing it falls back to :0, a working default,
// better than starting components with an empty DISPLAY and an obscure error.
//
// XAUTHORITY is only passed when nervud's environment really has it: passing a path to a file that doesn't exist makes
// X11 clients refuse to connect, which is worse than not passing it (without it they use no-auth or the XDG default lookup).
func displayEnv() []string {
	display := os.Getenv("DISPLAY")
	if display == "" {
		display = defaultDisplay
	}
	env := []string{"DISPLAY=" + display}
	if xauth := os.Getenv("XAUTHORITY"); xauth != "" {
		env = append(env, "XAUTHORITY="+xauth)
	}
	return env
}

// unitName generates the systemd transient unit name from (pkg, comp). The pkg/comp character sets both forbid '-'
// (manifest.validIDSegment), so "nervus-<pkg>-<comp>.service" is unique per component
// and can't collide
func unitName(pkg, comp string) string {
	return "nervus-" + pkg + "-" + comp + ".service"
}

// effectiveCriticality computes the effective criticality: an Ordinary package declaring anything above optional is always
// downgraded, otherwise a third-party App could call itself vital and trigger whole-machine shutdowns by crashing repeatedly, a denial of service
func effectiveCriticality(e pkgregistry.Entry, c pkgregistry.Component) pkgregistry.Criticality {
	crit := c.Criticality
	if crit == "" {
		crit = pkgregistry.CriticalityOptional
	}
	if e.Trust == identity.TrustOrdinary && crit.Rank() > pkgregistry.CriticalityOptional.Rank() {
		return pkgregistry.CriticalityOptional
	}
	return crit
}

// startLocked creates the instance and starts its supervisor. The caller must hold m.mu.
//
// If the key already has a terminal instance (StateStopped/StateFailed: an on-demand component that was stopped,
// or one whose circuit broke, stays in byKey rather than being removed), this doesn't panic but treats it as a fresh
// start: before setting the terminal state, the old supervisor goroutine has really stopped the matching
// systemd unit (stopProc/drain happen before setState; for a circuit break the retries are used up and the process isn't
// running), so once a terminal state is observed from outside, that goroutine will never write to the old instance again, and overwriting
// byKey/byUnit with a new *Instance and starting a new supervisor is safe. That's exactly the path EnsureStarted must support when
// restarting an on-demand component that ran before and then stopped or broke its circuit (see
// the on-demand start branch of Resolve in internal/endpoint)
func (m *Manager) startLocked(e pkgregistry.Entry, c pkgregistry.Component) {
	if m.stopped {
		return
	}
	key := componentKey{e.Manifest.PackageID, c.ID}
	if inst, ok := m.byKey[key]; ok && (inst.State == StateRunning || inst.State == StateStarting) {
		return // already running; idempotent
	}
	inst := &Instance{
		PackageID:   e.Manifest.PackageID,
		ComponentID: c.ID,
		UID:         e.UID,
		Generation:  e.RuntimeGeneration,
		Unit:        unitName(e.Manifest.PackageID, c.ID),
		Type:        c.Type,
		Runtime:     c.Runtime,
		Crit:        effectiveCriticality(e, c),
		LaunchMode:  c.LaunchMode,
		State:       StateStarting,
		stopCh:      make(chan struct{}),
		done:        make(chan struct{}),
	}
	m.byKey[key] = inst
	m.byUnit[inst.Unit] = inst
	m.wg.Add(1)
	go m.supervise(inst, e, c)
}

// requestStop tells the instance's supervisor: this is an expected stop, don't restart after exit (idempotent)
func (m *Manager) requestStop(inst *Instance) {
	inst.stopOnce.Do(func() { close(inst.stopCh) })
}

// supervise is the watch loop of a single instance
func (m *Manager) supervise(inst *Instance, e pkgregistry.Entry, c pkgregistry.Component) {
	// done is closed last: by then the unit has stopped and the state is settled, so ReloadPackage can safely start the new version
	defer func() {
		close(inst.done)
		m.wg.Done()
	}()

	backoff := m.backoffMin
	for {
		req, err := m.buildStartReq(e, c, inst.Unit)
		if err != nil {
			// Building the request failed (such as an invalid path): retrying won't help, so break the circuit directly
			m.audit(inst, "service.start", true, err)
			m.setState(inst, StateFailed)
			return
		}

		m.setState(inst, StateStarting)
		startCtx, cancelStart := context.WithTimeout(m.ctx, startStopTimeout)
		h, serr := m.auth.StartSandboxedProcess(startCtx, authority.SubjectKernel(), req)
		cancelStart()
		if serr != nil {
			if m.ctx.Err() != nil { // shutting down
				m.setState(inst, StateStopped)
				return
			}
			m.audit(inst, "service.start", true, serr)
			if !m.recordCrashAndContinue(inst) {
				m.onCircuitBreak(inst)
				return
			}
			if !m.backoffWait(&backoff, inst) {
				return
			}
			continue
		}

		m.onStarted(inst, h)
		backoff = m.backoffMin

		// Start a goroutine that blocks waiting for exit; the main loop selects on exit/stop/shutdown.
		//
		// The exit information MUST be carried out: it's the only way to tell "the app exited normally" from "it crashed". This once
		// dropped ExitInfo (written as `_, werr :=`), so a user closing an app's window was recorded as a crash
		// and restarted by crash recovery; the symptom was "the app won't close".
		exitCh := make(chan exitOutcome, 1)
		go func(handle authority.ProcessHandle) {
			info, werr := m.auth.WaitProcess(m.ctx, handle)
			exitCh <- exitOutcome{info: info, err: werr}
		}(h)

		select {
		case <-inst.stopCh:
			// An expected stop. Crucially the supervisor StopProcesses itself, without relying on an external caller:
			// StopComponent/Stop may snapshot an empty Handle while this component is still Starting and the Handle isn't settled yet,
			// and so never really stop it (a fix). At this point the h in the supervisor's hands is always valid
			m.stopProc(h)
			m.drain(exitCh)
			m.setState(inst, StateStopped)
			return
		case <-m.ctx.Done():
			// Whole shutdown: likewise the supervisor stops its own unit as a backstop, so nothing is missed in the Starting window
			m.stopProc(h)
			m.drain(exitCh)
			m.setState(inst, StateStopped)
			return
		case out := <-exitCh:
			werr := out.err
			// A natural exit. First re-check whether it was really an expected stop / a shutdown race
			select {
			case <-inst.stopCh:
				m.setState(inst, StateStopped)
				return
			default:
			}
			if m.ctx.Err() != nil {
				m.setState(inst, StateStopped)
				return
			}
			// ---- A clean app exit = the user closed it, not a fault ----
			//
			// Why this branch exists: the rule below, "a service shouldn't exit by itself", holds for services
			// but not for apps. An app with a UI being closed by the user should end; that's the normal
			// end of its lifecycle.
			//
			// Without the distinction, "the app won't close": the user clicks close -> the process exits -> the kernel records a crash ->
			// crash recovery restarts it -> the window comes back a second later. launch_mode=manual can't stop it either,
			// because it goes through crash recovery, not on-demand start.
			//
			// The criterion is systemd's own verdict (Result=="success", the
			// "Deactivated successfully" line in the log), not our guess at the exit code: being killed by a signal,
			// OOM, or a non-zero exit is never success and is still treated as a crash.
			if inst.Type == pkgregistry.ComponentApp && cleanExit(out.info) {
				m.audit(inst, "service.exited", false, nil)
				m.setState(inst, StateStopped)
				return
			}
			// A crash (a service shouldn't exit by itself)
			m.audit(inst, "service.crash", true, werr)
			if !m.recordCrashAndContinue(inst) {
				m.onCircuitBreak(inst)
				return
			}
			if !m.backoffWait(&backoff, inst) {
				return
			}
		}
	}
}

// stopProc stops the systemd unit for a handle. It uses its own bounded ctx rather than m.ctx:
// on the shutdown path m.ctx is already cancelled, so StopProcess with it would fail at once on ctx and the unit wouldn't stop.
// StopUnit is idempotent, so overlapping with external StopComponent/Stop calls is harmless
func (m *Manager) stopProc(h authority.ProcessHandle) {
	if h.Unit() == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), startStopTimeout)
	defer cancel()
	if err := m.auth.StopProcess(ctx, authority.SubjectKernel(), authority.StopProcessRequest{Handle: h}); err != nil {
		m.log.Warn("service: supervisor StopProcess failed", "unit", h.Unit(), "err", err)
	}
}

// drain waits for exitCh or the shutdown signal, so the WaitProcess goroutine neither leaks nor blocks forever
func (m *Manager) drain(exitCh <-chan exitOutcome) {
	select {
	case <-exitCh:
	case <-m.ctx.Done():
	}
}

// onStarted updates the handle and state and audits after the process starts successfully
func (m *Manager) onStarted(inst *Instance, h authority.ProcessHandle) {
	m.mu.Lock()
	inst.Handle = h
	inst.State = StateRunning
	m.mu.Unlock()
	m.audit(inst, "service.started", false, nil)
}

// setState updates the instance state while holding mu
func (m *Manager) setState(inst *Instance, s State) {
	m.mu.Lock()
	inst.State = s
	m.mu.Unlock()
}

// recordCrashAndContinue records one crash and returns whether it should keep restarting (circuit-break threshold not reached)
func (m *Manager) recordCrashAndContinue(inst *Instance) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	// Sliding window: drop old crashes outside the window
	kept := inst.crashes[:0]
	for _, t := range inst.crashes {
		if now.Sub(t) <= crashWindow {
			kept = append(kept, t)
		}
	}
	inst.crashes = append(kept, now)
	return len(inst.crashes) < crashThreshold
}

// onCircuitBreak handles a circuit break: go to Failed, stop restarting, write audit; for Vital, escalate to a Safety Trip
// (the machine stops, but the kernel lives, the audit lives, and the user can still operate it)
func (m *Manager) onCircuitBreak(inst *Instance) {
	m.setState(inst, StateFailed)
	m.audit(inst, "service.circuit-break", true, nil)
	if m.log != nil {
		m.log.Error("service: component circuit-broke after repeated crashes",
			"unit", inst.Unit, "criticality", string(inst.Crit))
	}
	if inst.Crit == pkgregistry.CriticalityVital {
		// Never kill nervud, never reboot: only trigger a Safety latch to stop the machine
		if m.log != nil {
			m.log.Error("service: VITAL component failed - escalating to Safety Trip", "unit", inst.Unit)
		}
		if m.safety != nil {
			m.safety.Trip()
		}
		m.audit(inst, "service.vital-escalation", true, nil)
	}
}

// backoffWait waits with exponential backoff and can be interrupted by stop/shutdown. Returning false means the supervisor should exit
func (m *Manager) backoffWait(backoff *time.Duration, inst *Instance) bool {
	d := *backoff
	if next := d * 2; next <= m.backoffMax {
		*backoff = next
	} else {
		*backoff = m.backoffMax
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-inst.stopCh:
		m.setState(inst, StateStopped)
		return false
	case <-m.ctx.Done():
		m.setState(inst, StateStopped)
		return false
	}
}

// buildStartReq assembles a StartSandboxedProcessRequest from the Entry and the Component.
//
// native: ExecStart = the ELF in the package; LD_LIBRARY_PATH points at the package's native_lib_dir.
// jvm: ExecStart = the platform JRE; -jar points at the entry in the package; -Djava.library.path points at the package's libraries.
// For both runtimes the in-package paths go into ContainedPaths, and authority.Validate checks each one is inside
// PackageRoot
// readWritePaths gives the set of writable paths for a component under ProtectSystem=strict.
//
// By default only its own private data directory. The install service also needs the staging root: nervud creates
// stage-* directories under it for unpacking, and strict makes the whole filesystem read-only. If it's not in this list,
// writing fails with "read-only file system" even when owner and mode are right.
//
// Permissions and mounts are two independent gates. Previously only the ownership gate was handled, and installs got stuck on this one.
func (m *Manager) readWritePaths(e pkgregistry.Entry, dataDir string) []string {
	paths := []string{dataDir}
	// The staging root: the criterion is perm.pkg.admin, the same source as admission to the admin channel.
	// Using Allowed rather than comparing package names means the kernel needn't know any specific package
	if m.stagingRoot != "" && m.perms != nil &&
		m.perms.Allowed(e.Manifest.PackageID, PermissionPackageAdmin) {
		paths = append(paths, m.stagingRoot)
	}
	// Shared user documents area: only packages that declared perm.storage.user get it. The file manager,
	// the file picker and any app that opens user documents see the same set of files through it.
	//
	// The criterion looks only at GrantedPermissions, not the runtime Allowed. The two gates are deliberately on
	// different timescales:
	//
	//	mount gate (here, static at install)    is the package eligible to touch the user documents area at all? It doesn't change after install,
	//	                                         so no process restart is needed for it
	//	access gate (ACL, immediate at run time) does the user agree right now? See ProjectRuntimePermission
	//
	// This used to check Allowed too, so "the user flips a switch" had to stop and restart the whole package for the new
	// mount namespace to take effect, and the app in use vanished and came back in front of the user. With the changed criterion
	// the directory is always mounted, and what really decides writes is the u:<uid>:rwx ACL entry on the directory,
	// evaluated at open(2), so additions and removals take effect immediately on running processes.
	//
	// This doesn't loosen the gate: a package without an ACL entry can't read or write the directory even if it's mounted, because
	// preflight sets it to 01770 and the other bits are empty.
	//
	// System image packages' GrantedPermissions are computed by pkgregistry.arbitrateSystemGrants at
	// the boot scan, and dynamic packages' by Install's Intersect, so both paths are already filled in.
	if m.inv.UserDataRoot != "" && hasPermission(e, permStorageUser) {
		paths = append(paths, m.inv.UserDataRoot)
	}
	// Shared area between services: this package's own subdirectory is writable.
	//
	// It gets the subdirectory, not the root: a writable root would let any package create directories under it, bypassing
	// the "one directory per package, owner = writer" structure. Reading others' directories doesn't need ReadWritePaths:
	// ProtectSystem=strict only makes the filesystem read-only and doesn't stop reads; isolation between packages here
	// relies only on data directories being 0700, while shared subdirectories are 0755 and meant to be readable.
	//
	// Same criterion as perm.storage.user: both GrantedPermissions (install eligibility) and the runtime Allowed
	// must pass. Looking only at the former would still bind the directory into the mount namespace after the user refuses.
	if hasPermission(e, permStorageShared) &&
		m.perms != nil &&
		m.perms.Allowed(e.Manifest.PackageID, permStorageShared) {
		paths = append(paths, m.sharedDirsFor(e.Manifest.PackageID)...)
	}
	return paths
}

// sharedDirsFor gives the two subdirectories the package may write in the shared area. They're the same paths
// pkgregistry.provisionEntry creates; when the two disagree the symptom is
// "the directory exists but can't be written", so there must be only one way to build these paths.
func (m *Manager) sharedDirsFor(packageID string) []string {
	var out []string
	if m.inv.SharedRuntimeRoot != "" {
		out = append(out, filepath.Join(m.inv.SharedRuntimeRoot, packageID))
	}
	if m.inv.SharedPersistRoot != "" {
		out = append(out, filepath.Join(m.inv.SharedPersistRoot, packageID))
	}
	return out
}

// hasPermission reports whether a Package has been granted a permission.
//
// It reads GrantedPermissions rather than the manifest: see the note on readWritePaths.
func hasPermission(e pkgregistry.Entry, perm string) bool {
	return slices.Contains(e.GrantedPermissions, perm)
}

func (m *Manager) buildStartReq(e pkgregistry.Entry, c pkgregistry.Component, unit string) (authority.StartSandboxedProcessRequest, error) {
	verDir := codeDir(m.inv, e)
	dataDir := filepath.Join(m.inv.DataRoot, e.Manifest.PackageID)
	entryPath := filepath.Join(verDir, c.Entry)

	req := authority.StartSandboxedProcessRequest{
		UnitName:   unit,
		Desc:       e.Manifest.PackageID + "/" + c.ID,
		UID:        e.UID,
		GID:        e.UID,
		WorkingDir: dataDir,
		BindToUnit: nervudUnit, // owner-death: if nervud dies, the component stops too (a fix)
		Limits: authority.ResourceLimits{
			MemoryMaxBytes:  c.Limits.MemoryMaxMB * 1024 * 1024,
			CPUQuotaPercent: c.Limits.CPUQuotaPercent,
			TasksMax:        uint64(c.Limits.TasksMax),
		},
		ReadWritePaths: m.readWritePaths(e, dataDir),
		ReadOnlyPaths:  []string{verDir},
		// The whole PackageRoot can't be InaccessiblePaths: that would also hide the component's own code directory
		//  (verDir is under PackageRoot), and once InaccessiblePaths hides a subdirectory it can't be
		// restored by nesting ReadOnlyPaths, so native ELFs / JARs / .so files would all be unreadable and nothing would start.
		// Isolation relies on three layers: 1. ProtectSystem=strict makes the whole fs read-only, including other packages' code directories;
		// 2. each package's data directory uses its own UID and 0700; 3. sensitive registry directories are Inaccessible
		InaccessiblePaths: []string{registryDir},
		ContainedPaths:    []string{entryPath},
		// Only packages built into the system image may touch real device nodes: Providers driving robot hardware
		//  (motors / CAN / serial ports) must see the host /dev, while the default sandbox's
		// PrivateDevices+DevicePolicy=closed blocks them all (regardless of UID;
		// even escalating to root doesn't get round it). Dynamically installed packages never get it: their Source is
		// SourceDynamicInstall, and they take the else branch, keeping the default closed /dev.
		AllowDeviceAccess: e.Source == pkgregistry.SourceSystemImage,
	}

	// Linux-level privileges: capabilities and extra socket address families.
	//
	// Only for packages from the system image, the same rule as AllowDeviceAccess. Dynamically installed packages
	// don't get them even if their manifest asks: handing capabilities to an arbitrary third-party package means the sandbox
	// doesn't exist, and this decision must be made in the kernel, because the manifest is written by the package itself and can't
	// declare its own eligibility.
	//
	// Silently ignored rather than refusing to load: a third-party package declaring CAP_NET_ADMIN shouldn't stop the whole package
	// from installing; it simply doesn't get it. If it really uses that capability it gets EPERM at run time,
	// a failure point closer to the problem than a failed install.
	if e.Source == pkgregistry.SourceSystemImage {
		req.AmbientCapabilities = pkgregistry.EffectiveCapabilities(c)
		req.ExtraAddressFamilies = pkgregistry.EffectiveAddressFamilies(c)
		// Granting privileges must leave a trace. The audit records exactly what was granted, not just "started a process":
		// when someone later traces why a package could touch hardware, this line is the only clue
		if len(req.AmbientCapabilities) > 0 || len(req.ExtraAddressFamilies) > 0 {
			m.aud.Record(context.Background(), audit.Event{
				Action:  "service.GrantLinuxPrivileges",
				Subject: e.Manifest.PackageID + "/" + c.ID,
				Detail:  pkgregistry.DescribePrivileges(req.AmbientCapabilities, req.ExtraAddressFamilies),
			})
		}
	}

	// Extra wiring for graphical components. The criterion is type == app: in the kernel the difference between app and service
	// is exactly "has a UI or not" (apps are opened by the Launcher, services run in the background), so there's no need to add
	// a gui flag to the manifest that people have to fill in, and might get wrong
	if c.Type == pkgregistry.ComponentApp {
		req.BindReadOnlyPaths = append(req.BindReadOnlyPaths, x11SocketDir)
		req.Env = append(req.Env, displayEnv()...)
		// The cookie file XAUTHORITY points to is usually under root's or a user's home, and ProtectHome=yes
		// makes both completely inaccessible. Passing only the environment variable without bringing the file in would leave the component
		// connecting to X with an unopenable path and "No protocol specified", which is harder to diagnose than not setting it
		if xauth := os.Getenv("XAUTHORITY"); xauth != "" {
			req.BindReadOnlyPaths = append(req.BindReadOnlyPaths, xauth)
		}
	}

	var nativeLibDir string
	if c.NativeLibDir != "" {
		nativeLibDir = filepath.Join(verDir, c.NativeLibDir)
		req.ContainedPaths = append(req.ContainedPaths, nativeLibDir)
	}

	switch c.Runtime {
	case pkgregistry.RuntimeNative:
		req.Runtime = authority.RuntimeNative
		req.ExecPath = entryPath
		if nativeLibDir != "" {
			req.Env = append(req.Env, "LD_LIBRARY_PATH="+nativeLibDir)
		}
	case pkgregistry.RuntimeJVM:
		req.Runtime = authority.RuntimeJVM
		req.ExecPath = authority.PlatformJREExec
		if nativeLibDir != "" {
			req.Args = append(req.Args, "-Djava.library.path="+nativeLibDir)
		}
		// Point the JVM's temporary directory and home at this package's private data directory.
		//
		// ## Why it must be set explicitly
		//
		// Many JVM libraries pack native libraries (.so) in the jar, extract them to disk at run time and dlopen them.
		// Compose Desktop's rendering backend skiko is the classic case: libskiko-linux-*.so is 30 MB,
		// written to disk on every start. Its two default locations are both unreliable in this sandbox:
		//
		//	java.io.tmpdir -> /tmp   PrivateTmp's private /tmp is writable, but many hardened
		//	                         systems mount the host /tmp noexec, and the private /tmp inherits it.
		//	                         Then the .so writes fine but dlopen fails with
		//	                         UnsatisfiedLinkError, which looks unrelated to permissions
		//	user.home -> ~           ProtectHome=yes makes it completely inaccessible
		//
		// Pointing them at the private data directory solves three things: it's in ReadWritePaths (writable), it's
		// on /var/lib (never noexec), and it's **persistent**, so extraction happens once
		// instead of moving 30 MB on every boot.
		//
		// For components that don't extract native libraries, these two properties only move the temporary directory, with no side effects.
		req.Args = append(req.Args,
			"-Djava.io.tmpdir="+dataDir,
			"-Duser.home="+dataDir,
		)
		req.Args = append(req.Args, "-jar", entryPath)
	}
	return req, nil
}

// audit records one component lifecycle audit entry
func (m *Manager) audit(inst *Instance, action string, denied bool, err error) {
	if m.aud == nil {
		return
	}
	m.aud.Record(context.Background(), audit.Event{
		Action:  action,
		Subject: "pkg:" + inst.PackageID + " comp:" + inst.ComponentID,
		Denied:  denied,
		Err:     err,
		Detail:  inst.Unit,
	})
}

// codeDir gives a Package's code directory.
//
// The two kinds of package have different layouts and can't share one path scheme:
//
//	dynamic install  <PackageRoot>/<pkg>/<version>/   several versions can coexist; old and new sit side by side during an upgrade
//	system image     <SystemPackageRoot>/<pkg>/       no version subdirectory; follows whole-image OTA
//
// System image packages have no version subdirectory because "several versions coexisting" doesn't exist for them: the whole image changes together.
// The kernel's boot scan globs by this layout too (scan.go: <root>/*/manifest.json).
//
// Building the path from PackageRoot unconditionally would point system packages' ExecStart at a path that doesn't exist, and
// systemd fails at step EXEC (203/EXEC), with an error that only says "executable not found",
// giving no hint that the layout assumption is wrong.
func codeDir(inv *authority.Invariants, e pkgregistry.Entry) string {
	if e.Source == pkgregistry.SourceSystemImage {
		return filepath.Join(inv.SystemPackageRoot, e.Manifest.PackageID)
	}
	return filepath.Join(inv.PackageRoot, e.Manifest.PackageID, e.ActiveVersion)
}

// systemdResultSuccess is systemd's word for "clean exit" (the line in the log
// "Deactivated successfully").
const systemdResultSuccess = "success"

// cleanExit decides whether an exit was "the process ending normally by itself".
//
// # Why Result alone isn't enough
//
// After a transient unit exits, systemd garbage-collects it, so WaitUnit may well return on the "unit is gone"
// path, where there is no Result to read (only the snapshot left by the last poll, and polling has an
// interval, so the whole terminal state may never have been seen). Accepting only Result=="success" would always classify that path as
// a crash, and the symptom is that apps won't close.
//
// So both count as a clean exit:
//
//	Result == "success"                      systemd says it succeeded
//	Result == "" and ExitStatus == 0         the unit is gone, with no trace of a non-zero exit code
//
// Conversely, any non-zero ExitStatus or explicit failure Result (exit-code / signal /
// oom-kill / timeout) is a crash. Fail closed: when unsure, treat it as a crash, because missing
// a crash means an app vanishes silently and never comes back, which is much worse than one extra restart.
func cleanExit(info authority.ExitInfo) bool {
	if info.ExitStatus != 0 {
		return false
	}
	return info.Result == systemdResultSuccess || info.Result == ""
}

// exitOutcome carries both of WaitProcess's return values through the channel.
//
// It's needed because ExitInfo.Result is what distinguishes "the user closed the app" from "the process crashed"; passing only
// the error would make the two look the same (both natural exits with err == nil).
type exitOutcome struct {
	info authority.ExitInfo
	err  error
}
