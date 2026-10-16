// Package service manages the lifecycle of App / Service components.
//
// It's the link that decides whether an installed package can actually run: pkgregistry verifies, decides on and
// registers packages in the Registry, while service.Manager starts the Registry's enabled components through authority
// -> systemd as sandboxed processes, watches for crashes and handles them by criticality, and maintains a
// unit -> component reverse index (byUnit) that unlocks ipc.verifyComponent.
//
// Dependency direction: service -> authority (starting/stopping processes), pkgregistry (reading the Registry),
// audit. It doesn't import safety; a Vital component's circuit breaker is reported through the narrow interface SafetyEscalator
// (connected to safety.Trip by an adapter in main.go), so safety's ReasonCode semantics don't
// leak into this package.
package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/nervus-os/nervud/internal/audit"
	"github.com/nervus-os/nervud/internal/authority"
	"github.com/nervus-os/nervud/internal/pkgregistry"
)

// State is a component instance's lifecycle state
type State uint8

const (
	StateStopped  State = iota // not running (initial / stopped)
	StateStarting              // being started
	StateRunning               // running
	StateStopping              // being stopped (intentional, so the crash watcher won't restart it)
	StateFailed                // circuit broken: restart budget used up, automatic restarts stopped
	StateDisabled              // disabled (SetEnabled(false))
)

func (s State) String() string {
	switch s {
	case StateStarting:
		return "starting"
	case StateRunning:
		return "running"
	case StateStopping:
		return "stopping"
	case StateFailed:
		return "failed"
	case StateDisabled:
		return "disabled"
	default:
		return "stopped"
	}
}

// componentKey uniquely identifies a component instance (Components are unique within a Package)
type componentKey struct {
	pkg  string
	comp string
}

// Instance is a component instance's runtime state.
//
// A read-only snapshot is returned to ipc through LookupByUnit; internal mutable fields (State/Handle/crash counts) are
// read and written by the Manager while holding mu, or by the instance's own supervisor goroutine.
type Instance struct {
	PackageID   string
	ComponentID string
	UID         uint32
	Generation  uint64
	Unit        string
	Type        pkgregistry.ComponentType
	Runtime     pkgregistry.Runtime
	Crit        pkgregistry.Criticality
	LaunchMode  pkgregistry.LaunchMode

	State  State
	Handle authority.ProcessHandle

	// crashes holds recent crash timestamps (a sliding window), for the "5 within 10s -> circuit break" decision
	crashes []time.Time

	// stopCh is closed by requestStop to tell this instance's supervisor: this is an expected stop,
	// so don't restart after WaitProcess returns
	stopCh   chan struct{}
	stopOnce sync.Once
	// done is closed after the supervisor has fully exited (including stopping the unit). ReloadPackage uses it during an upgrade
	// to confirm the old instance is completely stopped before starting the new version, avoiding start/stop races on the shared unit name
	done chan struct{}

	// sandboxRestartPending records that a runtime-permission projection must
	// restart this component after its old mount namespace is gone. It remains
	// set across a caller timeout so a retry can finish the same transition
	// instead of treating the stopped instance as unrelated. Protected by
	// Manager.mu.
	sandboxRestartPending bool
}

// ComponentIdentity is the lock-free projection used by IPC during handshake.
// Returning this value avoids copying Instance's internal sync.Once/channels
// across the package boundary.
type ComponentIdentity struct {
	PackageID   string
	ComponentID string
	UID         uint32
	Generation  uint64
	Type        pkgregistry.ComponentType
	State       State
}

// snapshot returns a read-only copy of the Instance (without internal channels), for returning across packages
func (i *Instance) snapshot() Instance {
	return Instance{
		PackageID: i.PackageID, ComponentID: i.ComponentID, UID: i.UID, Unit: i.Unit,
		Generation: i.Generation,
		Type:       i.Type, Runtime: i.Runtime, Crit: i.Crit, LaunchMode: i.LaunchMode,
		State: i.State, Handle: i.Handle,
	}
}

// ---- Narrow interface dependencies (defined by the consumer, satisfied implicitly by concrete types) ---------------------------

// ProcessController is the narrow interface to authority.Gate: start/stop/wait for a sandboxed process,
// and apply a USER_CONSENT decision to the ACL on the user documents area
type ProcessController interface {
	StartSandboxedProcess(ctx context.Context, subj authority.Subject, req authority.StartSandboxedProcessRequest) (authority.ProcessHandle, error)
	StopProcess(ctx context.Context, subj authority.Subject, req authority.StopProcessRequest) error
	WaitProcess(ctx context.Context, h authority.ProcessHandle) (authority.ExitInfo, error)
	SetUserDataAccess(ctx context.Context, subj authority.Subject, req authority.SetUserDataAccessRequest) error
	ReconcileUserDataAccess(ctx context.Context, subj authority.Subject, req authority.ReconcileUserDataAccessRequest) error
}

// PackageLookup is the narrow interface to pkgregistry.Registry: read installed packages
type PackageLookup interface {
	Lookup(id string) (pkgregistry.Entry, bool)
	List() []pkgregistry.Entry
}

// PermissionLookup is the runtime permission view used when projecting
// user-consent grants into a process sandbox. The package Entry still supplies
// install-time eligibility; both checks must pass before a shared writable path
// is mounted.
type PermissionLookup interface {
	Allowed(packageID, permission string) bool
}

// SafetyEscalator is called when a Vital component's circuit breaks, triggering a Safety latch.
// service doesn't import safety; main.go connects Trip to it with an adapter
// safety.Trip(ReasonSupervisorEscalation)
type SafetyEscalator interface {
	Trip()
}

// Invariants are the path/UID invariants service needs (uses authority.Invariants)
type Invariants = authority.Invariants

// Manager is the component lifecycle manager (kernel.Module)
type Manager struct {
	auth   ProcessController
	pkgs   PackageLookup
	perms  PermissionLookup
	safety SafetyEscalator
	aud    audit.Recorder
	log    *slog.Logger
	inv    *Invariants

	mu     sync.Mutex
	byKey  map[componentKey]*Instance
	byUnit map[string]*Instance // <- reverse index for verifyComponent

	// ctx/cancel control WaitProcess and the backoff waits of every supervisor goroutine;
	// Stop cancels so a blocked WaitProcess returns at once
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	// fatal is the seam implementing kernel.FatalReporter, capacity 1. Note component crashes never write to it:
	// a service crash must not take the kernel down; a crash only restarts / breaks the circuit / (for Vital) escalates to Safety,
	// never kills nervud. It's reserved for truly fatal cases such as the Manager's own monitoring infrastructure failing unrecoverably;
	// v1 has no such path yet, so the channel currently never fires
	fatal chan error

	// backoffMin/backoffMax are the bounds of the crash-restart backoff; production uses the restartBackoff* constants,
	// and tests can shrink them so circuit-break cases don't really wait a dozen seconds
	backoffMin time.Duration
	backoffMax time.Duration

	stopped      bool
	stopComplete bool

	// stagingRoot is the path for the "install service may additionally write to the staging root" exception.
	// Injected through GrantStagingAccess; when not injected it's empty and no component gets an extra writable path.
	// Who gets it is decided by PermissionPackageAdmin, not by package name.
	// Written once at wiring and read-only after Start, so it isn't protected by mu.
	stagingRoot string

	// sandboxReloadMu serializes package restarts caused by upgrades and runtime
	// permission projection. It is deliberately independent from mu: both paths
	// wait for supervisor goroutines, which briefly need mu while exiting.
	sandboxReloadMu sync.Mutex
}

// PermissionPackageAdmin is the permission whose holders get write access to the staging root. It's the same ID as
// admin.PermissionPackageAdmin: the package that can connect to the admin channel to send install commands
// is exactly the one that needs to unpack in staging.
//
// Each side keeps its own constant rather than sharing one: service doesn't import admin (the dependency goes the other way),
// and opening a common package for one string would add a node to the dependency graph. If the two drift apart, installs
// fail at "can connect but can't write", and the assertion in permission_test.go locks this down.
const PermissionPackageAdmin = "perm.pkg.admin"

// GrantStagingAccess opens the exception "packages holding PermissionPackageAdmin may write to stagingRoot".
//
// This is the only exception where "some packages get one more writable directory than others", so it's an explicit injection rather than
// inferred from elsewhere: the install service has to unpack into the staging directory nervud allocates, while ProtectSystem=strict
// makes the whole filesystem read-only. The existence of this exception must be visible at a glance at wiring (main.go).
//
// The criterion is the permission, not the package name. This used to take a packageID, passed by main.go as
// "nervus.pkgmanagerd"; now it only takes the path, and who can use it is decided by the permission decision, the same
// criterion as admission to the admin channel, so there's no mismatch of "can connect to the channel but can't write to staging".
//
// It must be called before Start.
func (m *Manager) GrantStagingAccess(stagingRoot string) {
	m.stagingRoot = stagingRoot
}

// New constructs the Manager
func New(
	auth ProcessController,
	pkgs PackageLookup,
	perms PermissionLookup,
	safety SafetyEscalator,
	aud audit.Recorder,
	log *slog.Logger,
	inv *Invariants,
) *Manager {
	if inv == nil {
		inv = authority.DefaultInvariants()
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Manager{
		auth: auth, pkgs: pkgs, perms: perms, safety: safety, aud: aud, log: log, inv: inv,
		byKey:  make(map[componentKey]*Instance),
		byUnit: make(map[string]*Instance),
		ctx:    ctx, cancel: cancel,
		fatal:      make(chan error, 1),
		backoffMin: restartBackoffMin,
		backoffMax: restartBackoffMax,
	}
}

func (m *Manager) Name() string { return "service" }

// Fatal implements kernel.FatalReporter: supervisor fatal errors are reported here, triggering the kernel's reverse shutdown
func (m *Manager) Fatal() <-chan error { return m.fatal }

// Start starts every enabled, always-on component (registered after safety, before ipc).
//
// A single component failing to start is only audited and doesn't block the whole startup: one bad component shouldn't bring down kernel startup. What really
// blocks startup is wiring-level errors (such as nil auth/pkgs), which should surface before New.
func (m *Manager) Start(ctx context.Context) error {
	m.sandboxReloadMu.Lock()
	defer m.sandboxReloadMu.Unlock()

	m.mu.Lock()
	if m.stopped {
		m.mu.Unlock()
		return ErrManagerStopped
	}
	m.mu.Unlock()

	// Reconcile the ACL on the user documents area first, then start components: the first process up should already see the correct access table.
	//
	// A failure here blocks startup, unlike a single component failing to start, which is only audited. This isn't "one bad component";
	// it's "the kernel can't establish access control on the user documents area". ACL entries left by the previous run would
	// take effect as they are, possibly including orphan entries of uninstalled packages, and UID reuse would hand them to a new package.
	// Carrying on with an access table that can't be verified is worse than failing to start.
	if err := m.ReconcileUserDataAccess(ctx); err != nil {
		return fmt.Errorf("service: reconcile user-data access: %w", err)
	}

	for _, e := range m.pkgs.List() {
		for _, c := range e.Manifest.Components {
			if c.LaunchMode != pkgregistry.LaunchAlwaysOn {
				continue // on-demand waits for EnsureStarted; manual waits for an explicit request
			}
			if e.ComponentDisabled(c.ID) {
				continue
			}
			m.mu.Lock()
			m.startLocked(e, c)
			m.mu.Unlock()
		}
	}
	return nil
}

// Stop stops every instance in reverse: first ask each instance to stop (intentional), then cancel so WaitProcess
// returns, and finally join every supervisor (shutdown in reverse: ipc -> service -> safety)
func (m *Manager) Stop(ctx context.Context) error {
	// Publish the terminal state and cancel supervisors before waiting for a
	// package reload. A reload may itself be waiting for one of those
	// supervisors, so taking sandboxReloadMu first would deadlock shutdown.
	m.mu.Lock()
	m.stopped = true
	m.mu.Unlock()
	m.cancel()

	m.sandboxReloadMu.Lock()
	defer m.sandboxReloadMu.Unlock()

	m.mu.Lock()
	if m.stopComplete {
		m.mu.Unlock()
		return nil
	}
	insts := make([]*Instance, 0, len(m.byKey))
	for _, inst := range m.byKey {
		insts = append(insts, inst)
	}
	m.mu.Unlock()

	// Ask each instance to stop + actively StopProcess (systemd StopUnit). Uses the ctx passed in (the kernel's shutdown
	// budget), and reads Handle through a snapshot under mu to avoid racing with the supervisor's writes
	for _, inst := range insts {
		m.requestStop(inst)
		m.mu.Lock()
		h := inst.Handle
		unit := inst.Unit
		m.mu.Unlock()
		if h.Unit() != "" {
			if err := m.auth.StopProcess(ctx, authority.SubjectKernel(),
				authority.StopProcessRequest{Handle: h}); err != nil {
				m.log.Warn("service: StopProcess failed during shutdown", "unit", unit, "err", err)
			}
		}
	}

	// cancel was issued before waiting for the lifecycle lock; joining here guarantees no supervisor is missed.
	m.wg.Wait()
	m.mu.Lock()
	m.stopComplete = true
	m.mu.Unlock()
	return nil
}

// LookupByUnit looks up a component instance snapshot by systemd unit name, for IPC to verify Component identity
func (m *Manager) LookupByUnit(unit string) (Instance, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	inst, ok := m.byUnit[unit]
	if !ok {
		return Instance{}, false
	}
	return inst.snapshot(), true
}

// LookupComponentByUnit returns only the kernel facts needed to authenticate an
// IPC component. It deliberately excludes supervisor synchronization state.
func (m *Manager) LookupComponentByUnit(unit string) (ComponentIdentity, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	inst, ok := m.byUnit[unit]
	if !ok {
		return ComponentIdentity{}, false
	}
	return ComponentIdentity{
		PackageID:   inst.PackageID,
		ComponentID: inst.ComponentID,
		UID:         inst.UID,
		Generation:  inst.Generation,
		Type:        inst.Type,
		State:       inst.State,
	}, true
}

// Sentinel errors for start failures.
//
// Sentinels rather than just error strings: the caller (ipc's LaunchComponent wiring) has to map the failure
// reason to a typed reason on the wire, so the Launcher can tell "no such app" from
// "the app is disabled". The latter should prompt the user to enable it in Settings; the former shouldn't.
// Doing this by matching error strings would silently break the day someone rewords a message.
var (
	// ErrUnknownPackage: the target Package isn't installed
	ErrUnknownPackage = errors.New("service: unknown package")
	// ErrUnknownComponent: the Package exists but has no such Component
	ErrUnknownComponent = errors.New("service: unknown component")
	// ErrComponentDisabled: the component is disabled (nervusctl disable, or declared in the manifest)
	ErrComponentDisabled = errors.New("service: component disabled")
	// ErrManagerStopped means the service lifecycle has reached its final state and can't create supervisors any more.
	ErrManagerStopped = errors.New("service: manager stopped")
	// ErrComponentFailed: the component used up its restart budget and its circuit is broken.
	//
	// EnsureStarted doesn't return it today: a broken circuit only stops automatic restarts, and explicit requests may still
	// retry (see the note in EnsureStarted). It's kept because the wire reason
	// LAUNCH_COMPONENT_REASON_COMPONENT_FAILED is already frozen, so if this ever
	// becomes "refuse explicit starts after a circuit break", the mapping needn't change.
	ErrComponentFailed = errors.New("service: component failed (restart budget exhausted)")
)

// IsRunning reports whether a component is running right now (including starting).
//
// It's for LaunchComponent to answer already_running. It's a momentary snapshot and the state can change any time after it returns;
// callers shouldn't use it for "check, then start" (that's TOCTOU). EnsureStarted
// is idempotent anyway, so just call it; this value is only informational.
func (m *Manager) IsRunning(pkg, comp string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	inst, ok := m.byKey[componentKey{pkg, comp}]
	return ok && (inst.State == StateRunning || inst.State == StateStarting)
}

// EnsureStarted starts a component; if it's already running it returns idempotently.
//
// Two callers: endpoint.Resolve when it resolves to an on-demand provider, and ipc's
// LaunchComponent (the Launcher opening an App).
func (m *Manager) EnsureStarted(_ context.Context, pkg, comp string) error {
	m.sandboxReloadMu.Lock()
	defer m.sandboxReloadMu.Unlock()

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.stopped {
		return ErrManagerStopped
	}

	key := componentKey{pkg, comp}
	if inst, ok := m.byKey[key]; ok {
		if inst.State == StateRunning || inst.State == StateStarting {
			return nil // idempotent
		}
		if inst.State == StateDisabled {
			return fmt.Errorf("%w: %s/%s", ErrComponentDisabled, pkg, comp)
		}
		// StateFailed (circuit broken) isn't blocked here: the restart budget guards against "automatic endless restarts",
		// while both of EnsureStarted's callers are explicit actions: the user clicking an icon, or someone
		// Resolving this interface. A person deliberately retrying once should be allowed, otherwise a component that has crashed five times
		// could never be opened again short of rebooting the machine. See TestEnsureStarted_RestartsAfterCircuitBreak
	}

	e, ok := m.pkgs.Lookup(pkg)
	if !ok {
		return fmt.Errorf("%w: %q", ErrUnknownPackage, pkg)
	}
	c, ok := e.Manifest.Component(comp)
	if !ok {
		return fmt.Errorf("%w: %q has no component %q", ErrUnknownComponent, pkg, comp)
	}
	if e.ComponentDisabled(comp) {
		return fmt.Errorf("%w: %s/%s", ErrComponentDisabled, pkg, comp)
	}
	m.startLocked(e, c)
	return nil
}

// ReloadPackage switches a Package's running instances to the new version after an upgrade:
// stop every old instance and wait for them to exit completely (unit stopped), then restart the
// always-on components with the new version from the current Registry. Stop-then-start is required: component unit names don't depend on the version, so starting the new version before the old unit stops
// would race start/stop on the same unit name (an upgrade fix)
func (m *Manager) ReloadPackage(ctx context.Context, pkg string) error {
	m.sandboxReloadMu.Lock()
	defer m.sandboxReloadMu.Unlock()

	// 1. Ask every old instance of the package to stop. The mapping must stay until all are done; if the caller times out,
	// EnsureStarted still sees the old instance and won't jump ahead on the same unit name.
	m.mu.Lock()
	if m.stopped {
		m.mu.Unlock()
		return ErrManagerStopped
	}
	var olds []*Instance
	for key, inst := range m.byKey {
		if key.pkg == pkg {
			olds = append(olds, inst)
			m.requestStop(inst)
		}
	}
	m.mu.Unlock()

	// 2. Wait for the old supervisors to exit completely (they stopProc their units themselves). Don't wait holding mu,
	//  or it deadlocks fighting the supervisor's setState/onStarted for the lock
	for _, inst := range olds {
		select {
		case <-inst.done:
		case <-ctx.Done():
			return ctx.Err()
		case <-m.ctx.Done():
			return ErrManagerStopped
		}
	}

	// 3. Only after every old supervisor has exited, remove the mapping and restart always-on components with the new version
	//  (on-demand/manual wait for a Resolve / explicit request).
	e, ok := m.pkgs.Lookup(pkg)
	m.mu.Lock()
	m.detachInstancesLocked(olds)
	if !ok {
		m.mu.Unlock()
		return nil // uninstalled again after the upgrade: no restart needed
	}
	if m.stopped {
		m.mu.Unlock()
		return ErrManagerStopped
	}
	for _, c := range e.Manifest.Components {
		if c.LaunchMode == pkgregistry.LaunchAlwaysOn && !e.ComponentDisabled(c.ID) {
			m.startLocked(e, c)
		}
	}
	m.mu.Unlock()
	return nil
}

// RevokeInstallGrant records an immediate stop intent for processes whose
// install-time sandbox authority was removed. It intentionally performs no
// package lookup, systemd call, or wait because permission.Registry.Replace may
// invoke it while pkgregistry holds its transaction mutex. Each supervisor owns
// the eventual StopProcess call, including the window where process startup has
// not returned a handle yet.
func (m *Manager) RevokeInstallGrant(packageID, permission string) {
	if permission != permStorageUser {
		return
	}
	m.mu.Lock()
	for key, inst := range m.byKey {
		if key.pkg == packageID && instanceMayBeRunning(inst.State) {
			m.requestStop(inst)
		}
	}
	m.mu.Unlock()
}

// ProjectRuntimePermission applies a USER_CONSENT decision to authority outside
// the IPC data plane. Runtime state has already been committed by
// permission.Registry before this method is called.
//
// # Why it changes the ACL instead of restarting the package
//
// Writable paths are burned into the mount namespace by systemd at spawn time and can't change once the process is up.
// That's why this hook used to have to stop and restart the whole package: the user flipped a switch in Settings, and the app
// they were using vanished and came back in front of them.
//
// Now mounting and access are two separate gates (see supervise.readWritePaths): the directory is always mounted in,
// and whether it can be written is decided by a u:<uid>:rwx ACL entry on the directory. The ACL is evaluated at open(2),
// so grants and revocations take effect immediately on already running processes, without touching a single process.
//
// This also makes the line at the top of permission/runtime.go, "an immediate-revocation ability Android doesn't have",
// true. Before this, the "immediate" was achieved by killing processes.
//
// This hook is intentionally not used by permission.Registry.Replace. Package
// projection can run while pkgregistry holds its transaction mutex; looking the
// package up from that callback would introduce a lock inversion.
func (m *Manager) ProjectRuntimePermission(packageID, permission string, allowed bool) error {
	if permission != permStorageUser {
		return nil
	}
	err := m.projectUserDataAccess(packageID, allowed)
	if m.aud != nil {
		m.aud.Record(context.Background(), audit.Event{
			Action:  "service.ProjectRuntimePermission",
			Subject: packageID,
			Denied:  err != nil,
			Err:     err,
			Detail:  fmt.Sprintf("%s allowed=%t", permission, allowed),
		})
	}
	return err
}

// ReconcileUserDataAccess rebuilds the ACL on the user documents area in full from the current grant state.
//
// Called once at startup. The ACL and _grants.json are persisted separately, so they can drift: if a package is uninstalled while
// nervud isn't running, its grant records are cleared but the ACL entries stay behind, and when the UID is later reused
// write access is handed to a new package for free. Reconciliation restores the invariant "the ACL is a projection of the grant state".
//
// It checks Allowed per package rather than just GrantedPermissions: the former is "does the user agree right now",
// the latter only install eligibility.
func (m *Manager) ReconcileUserDataAccess(ctx context.Context) error {
	if m.auth == nil || m.pkgs == nil || m.inv == nil || m.inv.UserDataRoot == "" {
		return nil
	}
	var uids []uint32
	for _, e := range m.pkgs.List() {
		if !hasPermission(e, permStorageUser) {
			continue
		}
		if m.perms == nil || !m.perms.Allowed(e.Manifest.PackageID, permStorageUser) {
			continue
		}
		uids = append(uids, e.UID)
	}
	err := m.auth.ReconcileUserDataAccess(ctx, authority.SubjectKernel(),
		authority.ReconcileUserDataAccessRequest{UIDs: uids})
	if m.aud != nil {
		m.aud.Record(ctx, audit.Event{
			Action: "service.ReconcileUserDataAccess", Subject: "kernel",
			Denied: err != nil, Err: err,
			Detail: fmt.Sprintf("granted uids=%v", uids),
		})
	}
	return err
}

// projectUserDataAccess applies one grant/revocation to the ACL on the user documents area.
//
// It uses the package's App UID rather than its name: the ACL recognises UIDs. When the package isn't in the Registry (uninstalled, or
// the grant lost a race with an uninstall) it quietly returns nil: a package that doesn't exist has no access to revoke,
// and treating it as an error would only make the uninstall path fail mysteriously.
func (m *Manager) projectUserDataAccess(packageID string, allowed bool) error {
	if m.auth == nil || m.inv == nil || m.inv.UserDataRoot == "" {
		return nil
	}
	e, ok := m.pkgs.Lookup(packageID)
	if !ok {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), startStopTimeout)
	defer cancel()
	return m.auth.SetUserDataAccess(ctx, authority.SubjectKernel(),
		authority.SetUserDataAccessRequest{UID: e.UID, Allowed: allowed})
}

func instanceMayBeRunning(state State) bool {
	return state == StateStarting || state == StateRunning || state == StateStopping
}

// detachInstancesLocked removes only the exact completed instances supplied by
// the caller. The identity check makes this safe against future code that may
// install a replacement between collection and cleanup. Caller must hold m.mu.
func (m *Manager) detachInstancesLocked(instances []*Instance) {
	for _, inst := range instances {
		key := componentKey{pkg: inst.PackageID, comp: inst.ComponentID}
		if m.byKey[key] == inst {
			delete(m.byKey, key)
		}
		if m.byUnit[inst.Unit] == inst {
			delete(m.byUnit, inst.Unit)
		}
		inst.sandboxRestartPending = false
	}
}

// StopComponent stops a component instance (without changing its enabled state; only SetEnabled changes persistent disabling)
func (m *Manager) StopComponent(ctx context.Context, pkg, comp string) error {
	m.sandboxReloadMu.Lock()
	defer m.sandboxReloadMu.Unlock()

	m.mu.Lock()
	inst, ok := m.byKey[componentKey{pkg, comp}]
	var h authority.ProcessHandle
	if ok {
		// An explicit lifecycle stop (disable/uninstall) supersedes a pending
		// permission-driven sandbox restart.
		inst.sandboxRestartPending = false
		m.requestStop(inst)
		h = inst.Handle
	}
	m.mu.Unlock()
	if !ok {
		return nil // not running; idempotent
	}
	var stopErr error
	if h.Unit() != "" {
		stopErr = m.auth.StopProcess(ctx, authority.SubjectKernel(),
			authority.StopProcessRequest{Handle: h})
	}
	select {
	case <-inst.done:
		return stopErr
	case <-ctx.Done():
		if stopErr != nil {
			return errors.Join(stopErr, ctx.Err())
		}
		return ctx.Err()
	}
}
