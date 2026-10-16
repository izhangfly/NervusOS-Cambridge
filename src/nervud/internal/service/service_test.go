package service

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/nervus-os/nervud/internal/audit"
	"github.com/nervus-os/nervud/internal/authority"
	"github.com/nervus-os/nervud/internal/authority/systemd"
	"github.com/nervus-os/nervud/internal/identity"
	"github.com/nervus-os/nervud/internal/pkgregistry"
)

type ctrlSpawner struct {
	mu         sync.Mutex
	startN     map[string]int
	stopN      map[string]int
	exit       map[string]chan systemd.ExitInfo
	specs      map[string][]systemd.UnitSpec
	startErr   error
	startGate  chan struct{}
	stopGate   chan struct{}
	noStopExit bool
}

func newCtrlSpawner() *ctrlSpawner {
	return &ctrlSpawner{
		startN: map[string]int{}, stopN: map[string]int{},
		exit: map[string]chan systemd.ExitInfo{}, specs: map[string][]systemd.UnitSpec{},
	}
}

func (s *ctrlSpawner) exitCh(name string) chan systemd.ExitInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.exit[name] == nil {
		s.exit[name] = make(chan systemd.ExitInfo, 16)
	}
	return s.exit[name]
}

func (s *ctrlSpawner) StartTransientUnit(_ context.Context, spec systemd.UnitSpec) error {
	s.mu.Lock()
	gate := s.startGate
	s.mu.Unlock()
	if gate != nil {
		<-gate
	}
	s.mu.Lock()
	s.startN[spec.Name]++
	s.specs[spec.Name] = append(s.specs[spec.Name], spec)
	err := s.startErr
	s.mu.Unlock()
	s.exitCh(spec.Name)
	return err
}

func (s *ctrlSpawner) stops(name string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stopN[name]
}

func (s *ctrlSpawner) StopUnit(ctx context.Context, name string) error {
	s.mu.Lock()
	s.stopN[name]++
	gate := s.stopGate
	noExit := s.noStopExit
	s.mu.Unlock()
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if noExit {
		return nil
	}
	select {
	case s.exitCh(name) <- systemd.ExitInfo{ActiveState: "inactive"}:
	default:
	}
	return nil
}

func (s *ctrlSpawner) setStopGate(gate chan struct{}) {
	s.mu.Lock()
	s.stopGate = gate
	s.mu.Unlock()
}

func (s *ctrlSpawner) suppressStopExit(suppress bool) {
	s.mu.Lock()
	s.noStopExit = suppress
	s.mu.Unlock()
}

func (s *ctrlSpawner) WaitUnit(ctx context.Context, name string) (systemd.ExitInfo, error) {
	select {
	case info := <-s.exitCh(name):
		return info, nil
	case <-ctx.Done():
		return systemd.ExitInfo{}, ctx.Err()
	}
}

func (s *ctrlSpawner) crash(name string) {
	s.exitCh(name) <- systemd.ExitInfo{ActiveState: "failed", Result: "exit-code"}
}

func (s *ctrlSpawner) starts(name string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.startN[name]
}

func (s *ctrlSpawner) lastSpec(name string) (systemd.UnitSpec, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	specs := s.specs[name]
	if len(specs) == 0 {
		return systemd.UnitSpec{}, false
	}
	return specs[len(specs)-1], true
}

type fakePkgs struct{ entries []pkgregistry.Entry }

func (f *fakePkgs) List() []pkgregistry.Entry { return f.entries }
func (f *fakePkgs) Lookup(id string) (pkgregistry.Entry, bool) {
	for _, e := range f.entries {
		if e.Manifest.PackageID == id {
			return e, true
		}
	}
	return pkgregistry.Entry{}, false
}

type fakePermissions struct {
	mu      sync.RWMutex
	allowed map[string]bool
}

func (f *fakePermissions) Allowed(packageID, permission string) bool {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.allowed[packageID+"/"+permission]
}

func (f *fakePermissions) set(packageID, permission string, allowed bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.allowed == nil {
		f.allowed = make(map[string]bool)
	}
	f.allowed[packageID+"/"+permission] = allowed
}

type fakeSafety struct {
	mu   sync.Mutex
	trip int
}

func (f *fakeSafety) Trip() { f.mu.Lock(); f.trip++; f.mu.Unlock() }
func (f *fakeSafety) trips() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.trip
}

type fakeAud struct {
	mu sync.Mutex
	ev []audit.Event
}

func (f *fakeAud) Record(_ context.Context, e audit.Event) {
	f.mu.Lock()
	f.ev = append(f.ev, e)
	f.mu.Unlock()
}

func testInvariants() *authority.Invariants {
	return authority.DefaultInvariants()
}

// recordingAuth intercepts the two ACL operations from the real Gate and records them, delegating everything else as before.
//
// service's job is to call authority correctly, not to write xattrs itself. Whether the bytes are right
// is for authority's tests to check against a real kernel (acl_linux_test.go). Intercepting at this layer also
// means the cases don't depend on the host really having /var/lib/nervus/user-data.
type recordingAuth struct {
	ProcessController

	mu         sync.Mutex
	userAccess []authority.SetUserDataAccessRequest
	reconciled [][]uint32
}

func (r *recordingAuth) SetUserDataAccess(
	_ context.Context, _ authority.Subject, req authority.SetUserDataAccessRequest,
) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.userAccess = append(r.userAccess, req)
	return nil
}

func (r *recordingAuth) ReconcileUserDataAccess(
	_ context.Context, _ authority.Subject, req authority.ReconcileUserDataAccessRequest,
) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reconciled = append(r.reconciled, append([]uint32(nil), req.UIDs...))
	return nil
}

// grants reports the last decision recorded for uid. With none recorded it falls back to the reconciliation result
func (r *recordingAuth) grants(uid uint32) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := len(r.userAccess) - 1; i >= 0; i-- {
		if r.userAccess[i].UID == uid {
			return r.userAccess[i].Allowed
		}
	}
	for i := len(r.reconciled) - 1; i >= 0; i-- {
		for _, u := range r.reconciled[i] {
			if u == uid {
				return true
			}
		}
		return false
	}
	return false
}

func makeEntry(pkg string, uid uint32, trust identity.TrustProfile, comps ...pkgregistry.Component) pkgregistry.Entry {
	return pkgregistry.Entry{
		Manifest:      pkgregistry.Manifest{PackageID: pkg, Version: "1.0.0", Components: comps},
		ActiveVersion: "1.0.0", VersionCode: 100, UID: uid, Trust: trust,
	}
}

func alwaysOnService(id string, crit pkgregistry.Criticality) pkgregistry.Component {
	return pkgregistry.Component{
		ID: id, Type: pkgregistry.ComponentService, Runtime: pkgregistry.RuntimeNative,
		Entry: "bin", LaunchMode: pkgregistry.LaunchAlwaysOn, Criticality: crit,
	}
}

func onDemandService(id string, crit pkgregistry.Criticality) pkgregistry.Component {
	return pkgregistry.Component{
		ID: id, Type: pkgregistry.ComponentService, Runtime: pkgregistry.RuntimeNative,
		Entry: "bin", LaunchMode: pkgregistry.LaunchOnDemand, Criticality: crit,
	}
}

func newTestManager(t *testing.T, sp authority.UnitManager, pkgs PackageLookup, safety SafetyEscalator) *Manager {
	m, _ := newTestManagerWithPermissions(t, sp, pkgs, &fakePermissions{}, safety)
	return m
}

func newTestManagerWithPermissions(
	t *testing.T,
	sp authority.UnitManager,
	pkgs PackageLookup,
	perms PermissionLookup,
	safety SafetyEscalator,
) (*Manager, *recordingAuth) {
	t.Helper()
	rec := &fakeRecorderDiscard{}
	gate, err := authority.New(authority.Config{
		Auditor: rec, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Spawner: sp,
	})
	if err != nil {
		t.Fatalf("authority.New: %v", err)
	}
	auth := &recordingAuth{ProcessController: gate}
	aud := &fakeAud{}
	m := New(auth, pkgs, perms, safety, aud, slog.New(slog.NewTextHandler(io.Discard, nil)), testInvariants())
	m.backoffMin = time.Millisecond
	m.backoffMax = 2 * time.Millisecond
	return m, auth
}

type fakeRecorderDiscard struct{}

func (fakeRecorderDiscard) Record(context.Context, audit.Event) {}

func waitFor(t *testing.T, d time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("condition not met within %s", d)
}

func TestStart_LaunchesAlwaysOnEnabled(t *testing.T) {
	sp := newCtrlSpawner()
	pkgs := &fakePkgs{entries: []pkgregistry.Entry{
		makeEntry("com.example.app", 20001, identity.TrustOrdinary,
			alwaysOnService("worker", pkgregistry.CriticalityOptional)),
	}}
	m := newTestManager(t, sp, pkgs, &fakeSafety{})
	defer func() { _ = m.Stop(context.Background()) }()

	if err := m.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	unit := unitName("com.example.app", "worker")
	waitFor(t, time.Second, func() bool { return sp.starts(unit) == 1 })

	waitFor(t, time.Second, func() bool {
		inst, ok := m.LookupByUnit(unit)
		return ok && inst.State == StateRunning && inst.ComponentID == "worker" && inst.UID == 20001
	})
}

func TestCrash_RestartsWithBackoff(t *testing.T) {
	sp := newCtrlSpawner()
	pkgs := &fakePkgs{entries: []pkgregistry.Entry{
		makeEntry("com.example.app", 20001, identity.TrustOrdinary,
			alwaysOnService("worker", pkgregistry.CriticalityOptional)),
	}}
	m := newTestManager(t, sp, pkgs, &fakeSafety{})
	defer func() { _ = m.Stop(context.Background()) }()
	if err := m.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	unit := unitName("com.example.app", "worker")
	waitFor(t, time.Second, func() bool { return sp.starts(unit) >= 1 })
	sp.crash(unit)
	waitFor(t, time.Second, func() bool { return sp.starts(unit) >= 2 })
}

func TestCircuitBreak_VitalEscalatesSafety(t *testing.T) {
	sp := newCtrlSpawner()
	fs := &fakeSafety{}
	pkgs := &fakePkgs{entries: []pkgregistry.Entry{
		makeEntry("nervus.core", 20002, identity.TrustPlatform,
			alwaysOnService("provider", pkgregistry.CriticalityVital)),
	}}
	m := newTestManager(t, sp, pkgs, fs)
	defer func() { _ = m.Stop(context.Background()) }()
	if err := m.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	unit := unitName("nervus.core", "provider")
	for i := 0; i < crashThreshold; i++ {
		waitFor(t, time.Second, func() bool { return sp.starts(unit) >= i+1 })
		sp.crash(unit)
	}
	waitFor(t, 2*time.Second, func() bool { return fs.trips() >= 1 })
	waitFor(t, time.Second, func() bool {
		inst, ok := m.LookupByUnit(unit)
		return ok && inst.State == StateFailed
	})
}

func TestEffectiveCriticality_OrdinaryDowngraded(t *testing.T) {
	e := makeEntry("com.third.party", 20003, identity.TrustOrdinary)
	c := alwaysOnService("svc", pkgregistry.CriticalityVital)
	if got := effectiveCriticality(e, c); got != pkgregistry.CriticalityOptional {
		t.Fatalf("Ordinary vital should downgrade to optional, got %q", got)
	}
	e2 := makeEntry("nervus.core", 20004, identity.TrustPlatform)
	if got := effectiveCriticality(e2, c); got != pkgregistry.CriticalityVital {
		t.Fatalf("Platform vital should stay vital, got %q", got)
	}
}

func TestStopComponent_NoRestart(t *testing.T) {
	sp := newCtrlSpawner()
	pkgs := &fakePkgs{entries: []pkgregistry.Entry{
		makeEntry("com.example.app", 20001, identity.TrustOrdinary,
			alwaysOnService("worker", pkgregistry.CriticalityOptional)),
	}}
	m := newTestManager(t, sp, pkgs, &fakeSafety{})
	defer func() { _ = m.Stop(context.Background()) }()
	if err := m.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	unit := unitName("com.example.app", "worker")
	waitFor(t, time.Second, func() bool { return sp.starts(unit) >= 1 })

	if err := m.StopComponent(context.Background(), "com.example.app", "worker"); err != nil {
		t.Fatalf("StopComponent: %v", err)
	}
	time.Sleep(30 * time.Millisecond)
	if n := sp.starts(unit); n != 1 {
		t.Fatalf("stopped component restarted: starts=%d, want 1", n)
	}
	waitFor(t, time.Second, func() bool {
		inst, ok := m.LookupByUnit(unit)
		return ok && inst.State == StateStopped
	})
}

func TestStopDuringStarting_StillStopsUnit(t *testing.T) {
	sp := newCtrlSpawner()
	sp.startGate = make(chan struct{})
	pkgs := &fakePkgs{entries: []pkgregistry.Entry{
		makeEntry("com.example.app", 20001, identity.TrustOrdinary,
			alwaysOnService("worker", pkgregistry.CriticalityOptional)),
	}}
	m := newTestManager(t, sp, pkgs, &fakeSafety{})
	defer func() { _ = m.Stop(context.Background()) }()
	if err := m.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	unit := unitName("com.example.app", "worker")

	done := make(chan struct{})
	go func() { _ = m.StopComponent(context.Background(), "com.example.app", "worker"); close(done) }()
	time.Sleep(20 * time.Millisecond)
	select {
	case <-done:
		t.Fatal("StopComponent returned before the starting unit was stopped")
	default:
	}

	close(sp.startGate)
	<-done

	waitFor(t, 2*time.Second, func() bool { return sp.stops(unit) >= 1 })
	waitFor(t, time.Second, func() bool {
		inst, ok := m.LookupByUnit(unit)
		return ok && inst.State == StateStopped
	})
}

func TestReloadPackage_RestartsFromCurrentRegistry(t *testing.T) {
	sp := newCtrlSpawner()
	pkgs := &fakePkgs{entries: []pkgregistry.Entry{
		makeEntry("com.example.app", 20001, identity.TrustOrdinary,
			alwaysOnService("worker", pkgregistry.CriticalityOptional)),
	}}
	m := newTestManager(t, sp, pkgs, &fakeSafety{})
	defer func() { _ = m.Stop(context.Background()) }()
	if err := m.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	unit := unitName("com.example.app", "worker")
	waitFor(t, time.Second, func() bool { return sp.starts(unit) == 1 })

	if err := m.ReloadPackage(context.Background(), "com.example.app"); err != nil {
		t.Fatalf("ReloadPackage: %v", err)
	}
	waitFor(t, 2*time.Second, func() bool { return sp.starts(unit) == 2 })
	waitFor(t, time.Second, func() bool {
		inst, ok := m.LookupByUnit(unit)
		return ok && inst.State == StateRunning
	})
}

func TestReloadPackage_TimeoutRetainsOldInstanceForRetry(t *testing.T) {
	sp := newCtrlSpawner()
	pkgs := &fakePkgs{entries: []pkgregistry.Entry{
		makeEntry("com.example.app", 20001, identity.TrustOrdinary,
			alwaysOnService("worker", pkgregistry.CriticalityOptional)),
	}}
	m := newTestManager(t, sp, pkgs, &fakeSafety{})
	defer func() { _ = m.Stop(context.Background()) }()
	if err := m.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	unit := unitName("com.example.app", "worker")
	waitFor(t, time.Second, func() bool { return sp.starts(unit) == 1 })

	stopGate := make(chan struct{})
	releaseStop := sync.OnceFunc(func() { close(stopGate) })
	defer releaseStop()
	sp.setStopGate(stopGate)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := m.ReloadPackage(ctx, "com.example.app"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("ReloadPackage error = %v, want deadline exceeded", err)
	}
	if _, ok := m.LookupByUnit(unit); !ok {
		t.Fatal("timed-out reload removed the old unit mapping")
	}
	if err := m.EnsureStarted(context.Background(), "com.example.app", "worker"); err != nil {
		t.Fatalf("EnsureStarted while old unit is stopping: %v", err)
	}
	if got := sp.starts(unit); got != 1 {
		t.Fatalf("EnsureStarted raced a replacement onto the old unit: starts=%d", got)
	}

	releaseStop()
	sp.setStopGate(nil)
	waitFor(t, time.Second, func() bool {
		inst, ok := m.LookupByUnit(unit)
		return ok && inst.State == StateStopped
	})
	if err := m.ReloadPackage(context.Background(), "com.example.app"); err != nil {
		t.Fatalf("retry ReloadPackage: %v", err)
	}
	waitFor(t, time.Second, func() bool { return sp.starts(unit) == 2 })
}

func TestManagerDoesNotStartAfterStop(t *testing.T) {
	sp := newCtrlSpawner()
	pkgs := &fakePkgs{entries: []pkgregistry.Entry{
		makeEntry("com.example.app", 20001, identity.TrustOrdinary,
			onDemandService("worker", pkgregistry.CriticalityOptional)),
	}}
	m := newTestManager(t, sp, pkgs, &fakeSafety{})
	if err := m.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if err := m.Start(context.Background()); !errors.Is(err, ErrManagerStopped) {
		t.Fatalf("Start after Stop error = %v, want ErrManagerStopped", err)
	}
	if err := m.EnsureStarted(context.Background(), "com.example.app", "worker"); !errors.Is(err, ErrManagerStopped) {
		t.Fatalf("EnsureStarted after Stop error = %v, want ErrManagerStopped", err)
	}
	if got := sp.starts(unitName("com.example.app", "worker")); got != 0 {
		t.Fatalf("stopped manager launched a component: starts=%d", got)
	}
}

func TestStopCancelsReloadWaitingForSupervisor(t *testing.T) {
	sp := newCtrlSpawner()
	sp.suppressStopExit(true)
	pkgs := &fakePkgs{entries: []pkgregistry.Entry{
		makeEntry("com.example.app", 20001, identity.TrustOrdinary,
			alwaysOnService("worker", pkgregistry.CriticalityOptional)),
	}}
	m := newTestManager(t, sp, pkgs, &fakeSafety{})
	if err := m.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	unit := unitName("com.example.app", "worker")
	waitFor(t, time.Second, func() bool { return sp.starts(unit) == 1 })

	reloadDone := make(chan error, 1)
	go func() {
		reloadDone <- m.ReloadPackage(context.Background(), "com.example.app")
	}()
	waitFor(t, time.Second, func() bool { return sp.stops(unit) >= 1 })

	stopDone := make(chan error, 1)
	go func() { stopDone <- m.Stop(context.Background()) }()
	select {
	case err := <-stopDone:
		if err != nil {
			t.Fatalf("Stop: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Stop remained blocked behind ReloadPackage")
	}
	select {
	case err := <-reloadDone:
		if !errors.Is(err, ErrManagerStopped) {
			t.Fatalf("ReloadPackage error = %v, want ErrManagerStopped", err)
		}
	case <-time.After(time.Second):
		t.Fatal("ReloadPackage did not observe manager shutdown")
	}
	if got := sp.starts(unit); got != 1 {
		t.Fatalf("reload started a replacement during shutdown: starts=%d", got)
	}
}

// Neither grant nor revocation restarts the process; that's the whole point of the ACL rework.
//
// This used to assert the opposite: every USER_CONSENT change stopped and restarted the whole package, so the new process's
// mount namespace would gain or lose ReadWritePaths. The user flipped a switch in Settings, and the app they were
// using vanished and came back in front of them.
//
// Now the mount is fixed (decided by install eligibility), and whether it can be written is decided by the u:<uid>:rwx ACL entry on the directory;
// the ACL is evaluated at open(2), so additions and removals take effect immediately on already running processes.
func TestProjectRuntimePermission_TogglesACLWithoutRestart(t *testing.T) {
	sp := newCtrlSpawner()
	entry := makeEntry("com.example.files", 20001, identity.TrustOrdinary,
		alwaysOnService("worker", pkgregistry.CriticalityOptional))
	entry.GrantedPermissions = []string{permStorageUser}
	pkgs := &fakePkgs{entries: []pkgregistry.Entry{entry}}
	perms := &fakePermissions{}
	perms.set("com.example.files", permStorageUser, true)
	m, auth := newTestManagerWithPermissions(t, sp, pkgs, perms, &fakeSafety{})
	defer func() { _ = m.Stop(context.Background()) }()

	if err := m.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	unit := unitName("com.example.files", "worker")
	waitFor(t, time.Second, func() bool { return sp.starts(unit) == 1 })

	// The mount gate only looks at install eligibility, so the directory is in ReadWritePaths from the start
	first, ok := sp.lastSpec(unit)
	if !ok || !slices.Contains(first.Sandbox.ReadWritePaths, m.inv.UserDataRoot) {
		t.Fatalf("initial ReadWritePaths = %v, want %q", first.Sandbox.ReadWritePaths, m.inv.UserDataRoot)
	}
	if !auth.grants(entry.UID) {
		t.Fatal("after Start reconciled, the ACL has no entry for the granted package")
	}

	// permission.Registry commits the denied state before invoking this hook.
	perms.set("com.example.files", permStorageUser, false)
	if err := m.ProjectRuntimePermission("com.example.files", permStorageUser, false); err != nil {
		t.Fatalf("ProjectRuntimePermission: %v", err)
	}

	if auth.grants(entry.UID) {
		t.Error("the ACL entry is still there after revocation")
	}
	if got := sp.starts(unit); got != 1 {
		t.Errorf("revocation restarted the process: starts=%d, want 1", got)
	}
	if got := sp.stops(unit); got != 0 {
		t.Errorf("revocation stopped the process: stops=%d, want 0", got)
	}

	// Granting it back shouldn't touch the process either
	perms.set("com.example.files", permStorageUser, true)
	if err := m.ProjectRuntimePermission("com.example.files", permStorageUser, true); err != nil {
		t.Fatalf("grant again: %v", err)
	}
	if !auth.grants(entry.UID) {
		t.Error("the ACL entry didn't come back after granting again")
	}
	if got := sp.starts(unit); got != 1 {
		t.Errorf("granting again restarted the process: starts=%d, want 1", got)
	}
}

func TestRevokeInstallGrant_StopsActiveSandboxWithoutRestart(t *testing.T) {
	sp := newCtrlSpawner()
	entry := makeEntry("com.example.files", 20001, identity.TrustOrdinary,
		alwaysOnService("worker", pkgregistry.CriticalityOptional))
	entry.GrantedPermissions = []string{permStorageUser}
	pkgs := &fakePkgs{entries: []pkgregistry.Entry{entry}}
	perms := &fakePermissions{}
	perms.set("com.example.files", permStorageUser, true)
	m, _ := newTestManagerWithPermissions(t, sp, pkgs, perms, &fakeSafety{})
	defer func() { _ = m.Stop(context.Background()) }()

	if err := m.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	unit := unitName("com.example.files", "worker")
	waitFor(t, time.Second, func() bool { return sp.starts(unit) == 1 })
	first, ok := sp.lastSpec(unit)
	if !ok || !slices.Contains(first.Sandbox.ReadWritePaths, m.inv.UserDataRoot) {
		t.Fatalf("initial sandbox did not contain UserDataRoot: %v", first.Sandbox.ReadWritePaths)
	}

	m.RevokeInstallGrant("com.example.files", permStorageUser)
	waitFor(t, time.Second, func() bool {
		inst, ok := m.LookupByUnit(unit)
		return ok && inst.State == StateStopped
	})
	if got := sp.starts(unit); got != 1 {
		t.Fatalf("install-grant revoke restarted component: starts=%d", got)
	}
}

func TestEnsureStarted_RestartsAfterStop(t *testing.T) {
	sp := newCtrlSpawner()
	pkgs := &fakePkgs{entries: []pkgregistry.Entry{
		makeEntry("com.example.app", 20001, identity.TrustOrdinary,
			onDemandService("worker", pkgregistry.CriticalityOptional)),
	}}
	m := newTestManager(t, sp, pkgs, &fakeSafety{})
	defer func() { _ = m.Stop(context.Background()) }()

	unit := unitName("com.example.app", "worker")

	if err := m.EnsureStarted(context.Background(), "com.example.app", "worker"); err != nil {
		t.Fatalf("first EnsureStarted: %v", err)
	}
	waitFor(t, time.Second, func() bool { return sp.starts(unit) >= 1 })

	if err := m.StopComponent(context.Background(), "com.example.app", "worker"); err != nil {
		t.Fatalf("StopComponent: %v", err)
	}
	waitFor(t, time.Second, func() bool {
		inst, ok := m.LookupByUnit(unit)
		return ok && inst.State == StateStopped
	})

	if err := m.EnsureStarted(context.Background(), "com.example.app", "worker"); err != nil {
		t.Fatalf("second EnsureStarted after stop: %v", err)
	}
	waitFor(t, time.Second, func() bool { return sp.starts(unit) >= 2 })
	waitFor(t, time.Second, func() bool {
		inst, ok := m.LookupByUnit(unit)
		return ok && inst.State == StateRunning
	})
}

func TestEnsureStarted_RestartsAfterCircuitBreak(t *testing.T) {
	sp := newCtrlSpawner()
	pkgs := &fakePkgs{entries: []pkgregistry.Entry{
		makeEntry("com.example.app", 20001, identity.TrustOrdinary,
			onDemandService("worker", pkgregistry.CriticalityOptional)),
	}}
	m := newTestManager(t, sp, pkgs, &fakeSafety{})
	defer func() { _ = m.Stop(context.Background()) }()

	unit := unitName("com.example.app", "worker")

	if err := m.EnsureStarted(context.Background(), "com.example.app", "worker"); err != nil {
		t.Fatalf("first EnsureStarted: %v", err)
	}
	for i := 0; i < crashThreshold; i++ {
		waitFor(t, time.Second, func() bool { return sp.starts(unit) >= i+1 })
		sp.crash(unit)
	}
	waitFor(t, time.Second, func() bool {
		inst, ok := m.LookupByUnit(unit)
		return ok && inst.State == StateFailed
	})
	startsBeforeRetry := sp.starts(unit)

	if err := m.EnsureStarted(context.Background(), "com.example.app", "worker"); err != nil {
		t.Fatalf("EnsureStarted after circuit break: %v", err)
	}
	waitFor(t, time.Second, func() bool { return sp.starts(unit) > startsBeforeRetry })
}

// cleanExit simulates "the process exited cleanly by itself": systemd's Result is success
// (the "Deactivated successfully" line in the log), which is exactly the path the user closing a window takes.
func (s *ctrlSpawner) cleanExit(name string) {
	s.exitCh(name) <- systemd.ExitInfo{ActiveState: "inactive", Result: "success"}
}

// manualApp makes an app-shaped, manually started component, like the file manager or Settings.
func manualApp(id string) pkgregistry.Component {
	return pkgregistry.Component{
		ID: id, Type: pkgregistry.ComponentApp,
		Runtime: pkgregistry.RuntimeJVM, Entry: "lib/main.jar",
		LaunchMode: pkgregistry.LaunchManual,
	}
}

// TestAppCleanExit_NotRestarted pins "once the user closes an app, it should stay closed".
//
// # What this test guards against: "the app won't close"
//
// The supervisor used to record service.crash for any natural exit and restart it by crash recovery. The rule
// "a service shouldn't exit by itself" holds for services, not for apps: an app with a UI being closed by the user
// is a normal end. Without the distinction, the user clicks close and the window comes back a second later,
// and launch_mode=manual can't stop it (it goes through crash recovery, not on-demand start).
func TestAppCleanExit_NotRestarted(t *testing.T) {
	sp := newCtrlSpawner()
	pkgs := &fakePkgs{entries: []pkgregistry.Entry{
		makeEntry("com.example.app", 20011, identity.TrustOrdinary, manualApp("main")),
	}}
	m := newTestManager(t, sp, pkgs, &fakeSafety{})
	defer func() { _ = m.Stop(context.Background()) }()
	if err := m.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	unit := unitName("com.example.app", "main")
	// manual components don't start with Start; start it once by hand (the same as the user opening it from the desktop)
	if err := m.EnsureStarted(context.Background(), "com.example.app", "main"); err != nil {
		t.Fatalf("EnsureStarted: %v", err)
	}
	waitFor(t, time.Second, func() bool { return sp.starts(unit) >= 1 })

	sp.cleanExit(unit)

	// The state converges to Stopped, and it is not restarted
	waitFor(t, 2*time.Second, func() bool {
		inst, ok := m.LookupByUnit(unit)
		return ok && inst.State == StateStopped
	})
	// Give the supervisor enough time to do the wrong thing: the old implementation restarted after the backoff
	time.Sleep(1500 * time.Millisecond)
	if got := sp.starts(unit); got != 1 {
		t.Fatalf("restarted after a clean exit: starts = %d, want 1", got)
	}
}

// TestAppCrash_StillRestarts: the fix mustn't break crash recovery.
//
// An app that really crashes (non-zero exit / killed by a signal) must still restart, otherwise the user sees the app vanish silently,
// which is a completely different thing from closing it themselves.
func TestAppCrash_StillRestarts(t *testing.T) {
	sp := newCtrlSpawner()
	pkgs := &fakePkgs{entries: []pkgregistry.Entry{
		makeEntry("com.example.app", 20012, identity.TrustOrdinary, manualApp("main")),
	}}
	m := newTestManager(t, sp, pkgs, &fakeSafety{})
	defer func() { _ = m.Stop(context.Background()) }()
	if err := m.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	unit := unitName("com.example.app", "main")
	if err := m.EnsureStarted(context.Background(), "com.example.app", "main"); err != nil {
		t.Fatalf("EnsureStarted: %v", err)
	}
	waitFor(t, time.Second, func() bool { return sp.starts(unit) >= 1 })

	sp.crash(unit)
	waitFor(t, 2*time.Second, func() bool { return sp.starts(unit) >= 2 })
}

// TestServiceCleanExit_StillRestarts pins that the exemption applies only to apps.
//
// A service exiting by itself is a fault: it has no "window closed", and exiting means it no longer provides
// its interface while callers are still waiting for it. Extending the app exemption to services would leave a Provider that
// exited silently never coming back, which is much harder to track down than repeated restarts.
func TestServiceCleanExit_StillRestarts(t *testing.T) {
	sp := newCtrlSpawner()
	pkgs := &fakePkgs{entries: []pkgregistry.Entry{
		makeEntry("com.example.svc", 20013, identity.TrustOrdinary,
			alwaysOnService("worker", pkgregistry.CriticalityOptional)),
	}}
	m := newTestManager(t, sp, pkgs, &fakeSafety{})
	defer func() { _ = m.Stop(context.Background()) }()
	if err := m.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	unit := unitName("com.example.svc", "worker")
	waitFor(t, time.Second, func() bool { return sp.starts(unit) >= 1 })

	sp.cleanExit(unit)
	waitFor(t, 2*time.Second, func() bool { return sp.starts(unit) >= 2 })
}
