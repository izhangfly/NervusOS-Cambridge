// This file plugs endpoint into the kernel.Module lifecycle and centrally holds the narrow interface dependencies and internal state shared by Register, Resolve
// and Route
package endpoint

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/nervus-os/nervud/internal/audit"
	"github.com/nervus-os/nervud/internal/catalog"
	"github.com/nervus-os/nervud/internal/identity"
	"github.com/nervus-os/nervud/internal/pkgregistry"
)

const (
	permServiceRegister        = "perm.service.register"
	permServiceRegisterPrivate = "perm.service.register.private"
)

// onDemandStartTimeout limits how long to wait for an on-demand component to start and RegisterEndpoint,
// so one resolve can't hold the caller forever.
//
// # Why it's tiered by runtime
//
// It used to be a flat 3 seconds, which is nowhere near enough for JVM components: an app with a Compose window has to
// start a JVM, load Skiko and create a GL context (falling back to software rendering on failure), measured at about 20 seconds before
// RegisterEndpoint. So Resolve timed out before it reported in, the caller got an endpoint that wasn't live yet,
// and the first call was bound to get NOT_FOUND, while the component had in fact started successfully and its window appeared
// moments later. That made the symptom especially baffling: "the UI opens fine, but every call errors".
//
// The native tier stays at 3 seconds: a Go service is up in milliseconds, and giving it 30 seconds would only delay real faults
// (a corrupt binary, a crash at start) by 27 seconds before they're reported.
const (
	onDemandStartTimeoutNative = 3 * time.Second
	// 30 seconds covers the worst cold start (measured at about 20 seconds, leaving double the margin for slower boards).
	// It's a ceiling, not a fixed wait: it wakes as soon as RegisterEndpoint arrives, so normally
	// nobody waits the full time.
	onDemandStartTimeoutJVM = 30 * time.Second
)

// onDemandStartTimeout gives the component's start-up wait limit.
//
// When the component can't be found it takes the conservative long tier: waiting too briefly when unsure would judge a component that's starting normally as failed,
// and that error direction is worse: it makes the feature look broken, while waiting a few more seconds is merely slow.
func (m *Module) onDemandStartTimeout(pkg, comp string) time.Duration {
	if m == nil || m.pkgs == nil {
		return onDemandStartTimeoutJVM
	}
	entry, ok := m.pkgs.Lookup(pkg)
	if !ok {
		return onDemandStartTimeoutJVM
	}
	c, ok := entry.Manifest.Component(comp)
	if !ok {
		return onDemandStartTimeoutJVM
	}
	if c.Runtime == pkgregistry.RuntimeNative {
		return onDemandStartTimeoutNative
	}
	return onDemandStartTimeoutJVM
}

// Sentinel errors for diagnostics/audit
var (
	errExplicitComponent = errors.New("endpoint: explicit_component is not supported in v1")
	errResourceNotFound  = errors.New("endpoint: no caller-visible resource matches selector")
	errVersionMismatch   = errors.New("endpoint: no interface version in requested range")
	errPermissionDenied  = errors.New("endpoint: package lacks required permission")
	errInterfaceNotFound = errors.New("endpoint: interface not visible to caller")
	errAmbiguous         = errors.New("endpoint: interface resolves to more than one candidate")
	errOnDemandTimeout   = errors.New("endpoint: timed out waiting for on-demand component to register")
	errOnDemandRejected  = errors.New("endpoint: on-demand component started but its registration was rejected")
)

// registrationRejection records why a component's most recent RegisterEndpoint was refused.
//
// It only serves diagnostics: when an on-demand start fails, the caller gets a ResolveEndpoint failure,
// while the real cause is in a different audit record (endpoint.RegisterEndpoint denied=true). The two records
// had no link between them, so "schema hash mismatch" was read as "the selector can't find a device".
type registrationRejection struct {
	at     time.Time
	reason string
}

// Narrow interfaces are defined by the consumer and satisfied implicitly by concrete types, so endpoint doesn't depend on full implementations

// PackageLookup is the narrow interface to pkgregistry.Registry: read installed packages.
//
// A copy of service.PackageLookup's same-named interface, not reinvented.
type PackageLookup interface {
	Lookup(id string) (pkgregistry.Entry, bool)
	List() []pkgregistry.Entry
}

// PermissionChecker asks whether a Package has been granted a permission.
//
// The interface definition is identical to ipc.PermissionChecker; endpoint and ipc each hold their own narrow reference to
// *permission.Registry
type PermissionChecker interface {
	AllowedAt(snapshot *catalog.Snapshot, packageID, permission string) bool
}

// ComponentStarter starts an on-demand component (called when Resolve resolves to it).
//
// service.Manager already provides this method; the interface is defined here (the consumer)
type ComponentStarter interface {
	EnsureStarted(ctx context.Context, pkg, comp string) error
}

// Module is endpoint's kernel.Module implementation, holding all the runtime state of Register/Resolve/Route.
//
// In v1 Start/Stop are basically no-ops: it has no RT Lane or background loop of its own, and all state
// hangs off the lifecycle of each IPC connection, created and destroyed with the connection
type Module struct {
	definitions *catalog.Registry
	pkgs        PackageLookup
	perm        PermissionChecker
	starter     ComponentStarter
	aud         audit.Recorder
	log         *slog.Logger

	mu sync.Mutex

	byConn      map[ConnHandle]*connState
	byInterface map[string][]*serviceRegistration

	// pendingStarts is the on-demand start wait queue: Resolve hangs a channel after EnsureStarted,
	// and when RegisterEndpoint arrives it closes every waiter's channel to wake them
	pendingStarts map[componentKey][]chan struct{}

	// generations records how many registrations each (pkg,comp,interface) triple has gone through,
	// for serviceRegistration.generation
	generations map[registrationKey]uint64

	// rejections keeps each component's most recently refused report. Only the last one per key is kept: this isn't
	// audit (audit has its own records), just an immediate clue for a Resolve waiting for it to start.
	// The number of keys is bounded by the number of installed components and doesn't grow over time
	rejections map[componentKey]registrationRejection

	// builtinSeq assigns service-side ids to built-in endpoints. It shares the
	// serviceRegistration struct with external registrations but numbers independently: built-ins have no conn, and that id is only for
	// diagnostics, never appearing on any Dispatch.
	builtinSeq uint64
}

// New constructs the endpoint Module
//
// definitions is the one authoritative catalog shared with permission,
// resource, pkgregistry, and IPC. Endpoint keeps no fallback catalog: a
// missing snapshot makes registration, resolution, and routing fail closed.
func New(
	definitions *catalog.Registry,
	pkgs PackageLookup,
	perm PermissionChecker,
	starter ComponentStarter,
	aud audit.Recorder,
	log *slog.Logger,
) *Module {
	return &Module{
		definitions:   definitions,
		pkgs:          pkgs,
		perm:          perm,
		starter:       starter,
		aud:           aud,
		log:           log,
		byConn:        make(map[ConnHandle]*connState),
		byInterface:   make(map[string][]*serviceRegistration),
		pendingStarts: make(map[componentKey][]chan struct{}),
		generations:   make(map[registrationKey]uint64),
		rejections:    make(map[componentKey]registrationRejection),
	}
}

func (m *Module) Name() string { return "endpoint" }

// Start has no worker to launch, but it rejects incomplete assembly before IPC
// can accept connections.
func (m *Module) Start(context.Context) error {
	if m == nil {
		return errors.New("endpoint: nil module")
	}
	if m.snapshot() == nil {
		return errors.New("endpoint: authoritative catalog is unavailable")
	}
	if m.pkgs == nil || m.perm == nil || m.starter == nil {
		return errors.New("endpoint: package, permission, and starter dependencies are required")
	}
	return nil
}

// Stop only prints one log-level diagnostic, "there are still N live bindings"
func (m *Module) Stop(context.Context) error {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	var alive int
	for _, cs := range m.byConn {
		alive += len(cs.bindings)
	}
	m.mu.Unlock()

	if alive > 0 && m.log != nil {
		m.log.Info("endpoint: stopping with live bindings", "count", alive)
	}
	return nil
}

// audit records one Register/Resolve/Unregister audit event
func (m *Module) audit(caller identity.Caller, action string, denied bool, err error, detail string) {
	if m.aud == nil {
		return
	}
	m.aud.Record(context.Background(), audit.Event{
		Action: action, Subject: caller.String(), Denied: denied, Err: err, Detail: detail,
	})
}

// connStateLocked gets or creates a connection's state. The caller must hold m.mu
func (m *Module) connStateLocked(conn ConnHandle) *connState {
	cs, ok := m.byConn[conn]
	if !ok {
		cs = newConnState()
		m.byConn[conn] = cs
	}
	return cs
}

// removeFromInterfaceIndexLocked removes reg from the byInterface index. The caller must
// hold m.mu. The caller sets reg.live to false separately; doing both together is one complete invalidation
func (m *Module) removeFromInterfaceIndexLocked(reg *serviceRegistration) {
	list := m.byInterface[reg.interfaceID]
	for i, r := range list {
		if r == reg {
			list = append(list[:i], list[i+1:]...)
			break
		}
	}
	if len(list) == 0 {
		delete(m.byInterface, reg.interfaceID)
		return
	}
	m.byInterface[reg.interfaceID] = list
}

// visibleCandidatesLocked returns the registrations in byInterface[interfaceID] visible to callerPkg:
// across packages only VisibilityPublic, plus VisibilityPackage within the same package.
// The caller must hold m.mu
func (m *Module) visibleCandidatesLocked(callerPkg, interfaceID string) []*serviceRegistration {
	var out []*serviceRegistration
	for _, reg := range m.byInterface[interfaceID] {
		if reg.live &&
			(reg.packageID == callerPkg || reg.visibility == pkgregistry.VisibilityPublic) {
			out = append(out, reg)
		}
	}
	return out
}

// manifestOnDemandCandidates returns enabled Service components whose manifest
// export is visible to callerPkg. This is only the package-side half of the
// proof; Resolve must intersect it with catalog ProviderInterfaces before a
// component is allowed to start.
func (m *Module) manifestOnDemandCandidates(
	callerPkg,
	interfaceID string,
) map[componentKey]struct{} {
	if m == nil || m.pkgs == nil {
		return nil
	}
	out := make(map[componentKey]struct{})
	for _, e := range m.pkgs.List() {
		for _, c := range e.Manifest.Components {
			if e.ComponentDisabled(c.ID) || !c.Type.CanProvideInterfaces() {
				continue
			}
			for _, exp := range c.Exports {
				if exp.Interface != interfaceID {
					continue
				}
				if exp.Visibility == pkgregistry.VisibilityPublic || e.Manifest.PackageID == callerPkg {
					out[componentKey{pkg: e.Manifest.PackageID, comp: c.ID}] = struct{}{}
				}
			}
		}
	}
	return out
}

// tryOnDemandStart starts an on-demand component and waits for it to complete RegisterEndpoint.
//
// The wait channel must be registered before calling EnsureStarted, otherwise if the component starts quickly enough,
// RegisterEndpoint's broadcast may happen before we register the waiter, and we'd never be woken.
//
// When the wait fails, the second return value gives the component's refused-report reason from THIS wait (empty if none).
// Only records within this window count: an old reason left by a previous start would turn "this time it never came up"
// into "this time it was refused", which is worse than no clue at all
func (m *Module) tryOnDemandStart(pkg, comp string) (started bool, rejected string, err error) {
	if m == nil || m.starter == nil {
		return false, "", errors.New("endpoint: component starter is unavailable")
	}
	key := componentKey{pkg: pkg, comp: comp}
	ch := make(chan struct{})
	since := time.Now()

	m.mu.Lock()
	m.pendingStarts[key] = append(m.pendingStarts[key], ch)
	m.mu.Unlock()

	timeout := m.onDemandStartTimeout(pkg, comp)

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	if serr := m.starter.EnsureStarted(ctx, pkg, comp); serr != nil {
		m.mu.Lock()
		m.removeWaiterLocked(key, ch)
		m.mu.Unlock()
		return false, "", serr
	}

	select {
	case <-ch:
		return true, "", nil
	case <-time.After(timeout):
		m.mu.Lock()
		m.removeWaiterLocked(key, ch)
		reason := ""
		if r, ok := m.rejections[key]; ok && !r.at.Before(since) {
			reason = r.reason
		}
		m.mu.Unlock()
		return false, reason, nil
	}
}

// recordRejection records one refused report. The caller must not hold m.mu
func (m *Module) recordRejection(pkg, comp, reason string) {
	if m == nil || pkg == "" || comp == "" {
		return
	}
	m.mu.Lock()
	m.rejections[componentKey{pkg: pkg, comp: comp}] = registrationRejection{
		at: time.Now(), reason: reason,
	}
	m.mu.Unlock()
}

// removeWaiterLocked removes a waiter that timed out and gave up from the wait queue. The caller must hold m.mu
func (m *Module) removeWaiterLocked(key componentKey, ch chan struct{}) {
	waiters := m.pendingStarts[key]
	for i, w := range waiters {
		if w == ch {
			waiters = append(waiters[:i], waiters[i+1:]...)
			break
		}
	}
	if len(waiters) == 0 {
		delete(m.pendingStarts, key)
		return
	}
	m.pendingStarts[key] = waiters
}

func (m *Module) snapshot() *catalog.Snapshot {
	if m == nil || m.definitions == nil {
		return nil
	}
	return m.definitions.Current()
}

func (m *Module) allowedAt(snapshot *catalog.Snapshot, packageID, permission string) bool {
	if permission == "" {
		return true
	}
	return snapshot != nil && m != nil && m.perm != nil &&
		m.perm.AllowedAt(snapshot, packageID, permission)
}
