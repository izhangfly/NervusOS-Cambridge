// This file is the ipc module's kernel.Module implementation: UDS listening, the accept loop, connection admission and
// orderly shutdown. Framing is in frame.go; the per-connection Envelope state machine is in conn.go
//
// Dependency boundary: this package doesn't import syscall / x/sys directly; reading peer credentials goes through internal/sysprobe,
// and privileged operations through internal/authority
package ipc

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	transferv1 "github.com/nervus-os/nervus-ipc/protocol/interface/transferv1"
	ipcv1 "github.com/nervus-os/nervus-ipc/protocol/ipcv1"

	"github.com/nervus-os/nervud/internal/audit"
	"github.com/nervus-os/nervud/internal/authority"
	"github.com/nervus-os/nervud/internal/catalog"
	"github.com/nervus-os/nervud/internal/control"
	"github.com/nervus-os/nervud/internal/endpoint"
	"github.com/nervus-os/nervud/internal/identity"
	"github.com/nervus-os/nervud/internal/operation"
	"github.com/nervus-os/nervud/internal/service"
	"github.com/nervus-os/nervud/internal/subscription"
	"github.com/nervus-os/nervud/internal/sysprobe"
	"github.com/nervus-os/nervud/internal/transfer"
)

// socketMode is the control-plane socket's file mode.
//
// 0666 rather than 0660 with a dedicated group, because every Package has its own separate UID,
// and expressing "who is an App" with a group would make group membership a second source of truth alongside the Package Registry:
// add to the group on install, remove on uninstall, and once they fall out of step you get "the Registry says uninstalled, the group still has it, and it can still
// connect". The Registry is the only source of truth, and nothing should compete with it.
//
// Real authentication happens after accept: SO_PEERCRED for kernel credentials, the App UID range check,
// the Registry lookup. File bits are only a first noise filter and carry no security responsibility.
//
// The cost is that any local process can connect and force nervud through the refusal flow, backstopped by the global
// Limits.MaxConns. If this surface ever proves to be under real pressure, switching to 0660 with a group is one line, but then the group
// would only be hardening, still not a source of truth
const socketMode fs.FileMode = 0o666

// Limits are the admission budgets this module uses.
//
// Only items actually enforced today are here. In-flight request counts, payload bytes, outbound queue bytes and so on
// are per-connection budgets to add when the Envelope layer lands; defining a pile of fields nobody reads would only
// make people think they're in effect
type Limits struct {
	// MaxConns is the process-wide concurrent connection limit. The socket is 0666, so this is the last line stopping local processes
	// from exhausting fds and memory by opening connections wildly
	MaxConns int

	// MaxConnsPerUID limits how many connections one Package can take, so one UID can't eat the global quota
	MaxConnsPerUID int

	// HandshakeTimeout caps the time from accept to receiving the first frame. Without it, connecting and saying nothing
	// would occupy a connection slot for free
	HandshakeTimeout time.Duration

	// IdleTimeout is the longest allowed gap between two Frames in the steady state, kept alive by Ping/Pong
	IdleTimeout time.Duration

	// FrameBodyTimeout caps the time to read a body whose length has been announced.
	//
	// It must be well short of IdleTimeout: once the length prefix arrives, N bytes are already on their way,
	// and however slow, they should arrive soon. Covering body reads with IdleTimeout would hand the idle tolerance to
	// slowloris: after sending the 4-byte header, squeezing out one byte a second would hold the connection for a long time
	FrameBodyTimeout time.Duration

	// MaxFramesPerConnPerSec is the per-connection inbound frame rate limit (the first line).
	//
	// Every frame needs proto.Unmarshal, so a legitimate App flooding frames in an infinite loop can keep creating CPU/GC
	// pressure. The limit is high enough that normal clients (tens per second interactive, hundreds per second for high-rate telemetry)
	// never reach it, and only cuts off pathological tight loops. Exceeding it closes the connection as abuse and audits it.
	//
	// This is only the first line: the full design calls for a per-UID aggregate byte-rate token bucket,
	// and exceeding it should return RESOURCE_EXHAUSTED rather than close the connection outright, which needs the Envelope layer's
	// Response capability and comes with conn.go
	MaxFramesPerConnPerSec int
}

// DefaultLimits are the fixed values
func DefaultLimits() Limits {
	return Limits{
		MaxConns:               256,
		MaxConnsPerUID:         4,
		HandshakeTimeout:       5 * time.Second,
		IdleTimeout:            60 * time.Second,
		FrameBodyTimeout:       5 * time.Second,
		MaxFramesPerConnPerSec: 2000,
	}
}

// normalizeLimits fills in defaults field by field.
//
// Defaults can't only apply when the whole struct is zero: partial configuration (only MaxConns set, the rest zero) would put
// fields like HandshakeTimeout=0 into effect. SetReadDeadline(now) times out at once, so every connection drops as soon as it's made,
// showing up as "listening fine but nobody can connect", very hard to diagnose. Falling back per field pulls such half-filled configs back to safety too
func normalizeLimits(l Limits) Limits {
	d := DefaultLimits()
	if l.MaxConns <= 0 {
		l.MaxConns = d.MaxConns
	}
	if l.MaxConnsPerUID <= 0 {
		l.MaxConnsPerUID = d.MaxConnsPerUID
	}
	if l.HandshakeTimeout <= 0 {
		l.HandshakeTimeout = d.HandshakeTimeout
	}
	if l.IdleTimeout <= 0 {
		l.IdleTimeout = d.IdleTimeout
	}
	if l.FrameBodyTimeout <= 0 {
		l.FrameBodyTimeout = d.FrameBodyTimeout
	}
	if l.MaxFramesPerConnPerSec <= 0 {
		l.MaxFramesPerConnPerSec = d.MaxFramesPerConnPerSec
	}
	return l
}

type Config struct {
	// SockPath is the control-plane entry point; always /run/nervus/nervud.sock in production
	SockPath string

	Log     *slog.Logger
	Auditor audit.Recorder

	// Invariants provide the App UID range check. Passed by value rather than as *authority.Gate:
	// ipc needs no privileged operation, only that one invariant
	Invariants *authority.Invariants

	// Identity maps kernel credentials to trusted identities. The interface is defined by this package (the consumer),
	// and *identity.Registry satisfies it implicitly
	Identity PeerResolver

	// Permission asks about a Package's granted permissions. The interface is defined by this package (the consumer),
	// and *permission.Registry satisfies it implicitly
	Permission PermissionChecker

	// When Limits is zero, DefaultLimits are used
	Limits Limits

	// Components maps systemd units back to the components they host (provided by service.Manager).
	// The interface is defined by this package (the consumer) and *service.Manager satisfies it implicitly. When nil, verifyComponent
	// fails closed.
	Components ComponentResolver

	// Endpoints forwards ResolveEndpoint/RegisterEndpoint/UnregisterEndpoint to
	// internal/endpoint and provides route lookups when dispatching Requests. The interface is defined by this package (the consumer),
	// implemented by *endpoint.Module. When nil, the handshake/request pipeline keeps the existing degraded
	// behaviour (ResolveEndpoint and friends handled as UnsupportedBody, Requests always UNAVAILABLE),
	// which lets endpoint and ipc be merged separately
	Endpoints EndpointResolver

	// Leases connects AcquireControl/ReleaseControl to internal/control's lease state machine.
	// When nil these two bodies return UNAVAILABLE (a capability gap, not a protocol violation),
	// the same shape as the degradation when Endpoints is nil.
	Leases ControlLeases

	// Launcher connects LaunchComponent to internal/service's EnsureStarted.
	// *service.Manager satisfies it implicitly; when nil this body returns UNAVAILABLE.
	Launcher ComponentLauncher

	// Resources resolves AcquireControl's ResourceSelector into a resource_handle.
	// When nil every lease request fails to resolve; v2 has no implicit default.
	Resources ResourceResolver

	// Operations owns the long-task state machine. When nil, methods declaring returns_operation
	// are always refused (a capability gap, not a protocol violation).
	Operations OperationManager

	// Transfer owns the generic high-throughput data plane. Method metadata,
	// rather than capability-specific kernel code, decides which calls may
	// create a stream.
	Transfer TransferManager
}

// ComponentResolver maps a systemd unit name back to the component instance it hosts.
//
// The interface is defined on the consumer (ipc) side and implemented by internal/service. verifyComponent uses
// the peer process's cgroup to work out the unit, then gets through it the kernel facts of which Package's which
// Component this unit is, which UID, and whether it's disabled, and cross-checks them against the declared_component_id the client
// reported itself
type ComponentResolver interface {
	LookupComponentByUnit(unit string) (service.ComponentIdentity, bool)
}

// EndpointResolver forwards ResolveEndpoint/RegisterEndpoint/UnregisterEndpoint to
// internal/endpoint, and provides route lookups when dispatching Requests.
//
// The interface is defined in this package (the consumer) and implemented by *endpoint.Module, the same pattern as ComponentResolver/
// PermissionChecker. The endpoint package itself doesn't import ipc: supporting types such as ConnHandle/
// RouteInfo/RouteError are defined on the endpoint side, and IPC depends only on the narrow interface,
// so endpoint never depends back on the transport layer
type EndpointResolver interface {
	ResolveEndpoint(conn endpoint.ConnHandle, caller identity.Caller, req *ipcv1.ResolveEndpoint) *ipcv1.ResolveEndpointResult
	RegisterEndpoint(conn endpoint.ConnHandle, caller identity.Caller, req *ipcv1.RegisterEndpoint) *ipcv1.RegisterEndpointResult
	UnregisterEndpoint(conn endpoint.ConnHandle, req *ipcv1.UnregisterEndpoint) *ipcv1.UnregisterEndpointResult

	// Route is for handleRequest: given an endpoint_id, one lookup answers "who to forward to, which
	// permissions are needed, and is this call still legal". ipc caches no routing state itself and looks it up every time
	Route(conn endpoint.ConnHandle, endpointID uint64, methodID uint32) (endpoint.RouteInfo, endpoint.RouteError)

	// RouteEvent is subscription-side admission: given (endpoint_id, event_id), one lookup answers
	// "where is the event source, what's the authoritative EventMeta, and is this subscription legal".
	// The admission chain has the same source as Route; only the last step checks the event instead of the method
	RouteEvent(conn endpoint.ConnHandle, endpointID uint64, eventID uint32) (endpoint.EventRoute, endpoint.RouteError)

	// LookupProviderEvent checks one PublishEvent: does this connection really own the
	// endpoint, and was the event_id declared in the contract
	LookupProviderEvent(conn endpoint.ConnHandle, serviceEndpointID uint64, eventID uint32) (catalog.EventDefinition, endpoint.RouteError)

	// OwnsEndpoint answers "does this connection own this registration".
	//
	// BindEventScope goes through it: without this check, any system service could register instance ownership for
	// another endpoint, and so insert itself into someone else's event stream.
	OwnsEndpoint(conn endpoint.ConnHandle, serviceEndpointID uint64) bool

	// ConnClosed is called once by ipc when a connection's serve loop exits, letting endpoint clean up
	// every registration/binding under that connection and invalidate associated bindings that are still alive
	ConnClosed(conn endpoint.ConnHandle)
}

// PeerResolver resolves SO_PEERCRED credentials into a trusted identity.
//
// The interface is defined on the consumer side and implemented by internal/identity. ipc only needs this one
// method and has no reason to hold the whole Registry: the fewer modules get every method,
// the shorter the answer to "who can change the identity index"
type PeerResolver interface {
	Resolve(cred sysprobe.Ucred) (identity.Caller, error)
}

// PermissionChecker asks whether a Package has been granted a permission.
//
// The interface is defined on the consumer (ipc) side and implemented by internal/permission, for the same
// reason: ipc only needs this one method and has no reason to hold the whole Registry.
//
// The request pipeline couldn't yet work out "which permission ID does this Request need" (that depends on internal/
// endpoint mapping endpoint_id/method_id to a permission ID, and endpoint was still
// an empty implementation), so at this stage this interface was only a wiring placeholder; the real decision call sites and
// the use of ipcv1.StatusCode_STATUS_CODE_PERMISSION_DENIED were to be added
// with the request pipeline once endpoint landed
type PermissionChecker interface {
	Allowed(packageID, permission string) bool
}

// TransferManager is the route-bound control surface consumed by IPC. Its
// listener lifecycle remains a separate kernel module.
type TransferManager interface {
	Begin(transfer.Origin, *transferv1.BeginTransferRequest) (*transferv1.BeginTransferResponse, error)
	Commit(transfer.ConnID, []byte) error
	Abort(transfer.ConnID, []byte) error
	FinishRoute(uint64, bool, []*ipcv1.TransferHandle) error
	CloseRoute(uint64)
	ConnClosed(transfer.ConnID)
	EndpointRevoked(transfer.ConnID, uint64, uint64)
	RevokePackage(string)
	RevokePermission(string, string)
	RevokeResource(string, uint64)
	RevokeControl(transfer.ConnID, string)
}

// Server owns the control-plane UDS.
//
// Lifecycle contract: Start must return quickly, and background loops exit only on Stop, not on Start(ctx)'s
// ctx. If background loops also listened to the signal ctx, they'd be woken in parallel with Kernel.stopAll by the same signal,
// with no telling which exits first, and the shutdown order would no longer be decided only by the Kernel's reverse Stop
type Server struct {
	sockPath   string
	log        *slog.Logger
	auditor    audit.Recorder
	inv        *authority.Invariants
	identity   PeerResolver
	permission PermissionChecker
	components ComponentResolver
	// verifyComponentForTest is deliberately unexported. Linux socket tests in
	// this package can replace cgroup discovery without creating a production
	// configuration knob that bypasses kernel-backed component identity.
	verifyComponentForTest func(*net.UnixConn, identity.Caller, string) (service.ComponentIdentity, error)
	endpoints              EndpointResolver
	// subscriptions holds every event subscription. When nil, Subscribe returns UNAVAILABLE and
	// PublishEvent drops silently; minimal wiring and many tests don't need subscriptions.
	subscriptions *subscription.Registry
	// eventScopes records event instance ownership (see eventscope.go). External Providers'
	// per-instance subscriptions are decided by it.
	eventScopes *eventScopes
	leases      ControlLeases
	launcher    ComponentLauncher
	resources   ResourceResolver
	transfer    TransferManager
	// operations owns the long-task state machine. When nil, methods declaring returns_operation
	// are always refused, rather than silently degrading into ordinary calls: that would give the caller an OK
	// while the machine is still moving, and it would think the work was done.
	operations OperationManager
	// operationWire is the built-in endpoint's share of state (the endpoint handle),
	// set up by OperationBuiltinHandler at wiring.
	operationWire *operationWire
	limits        Limits
	// monotonicNow is the Linux CLOCK_MONOTONIC source used for wire deadlines.
	// Tests replace it because production sysprobe support is Linux-only.
	monotonicNow func() (uint64, error)

	// nextConnID assigns each connection the identifier the control module uses. It starts at 1 (0 is kept as
	// the "unspecified" sentinel, the same habit as reserving request_id 0 in the protocol).
	nextConnID atomic.Uint64

	ln *net.UnixListener

	// lockLn is the singleton lock: an abstract namespace socket whose name is derived from sockPath.
	// It never Accepts; it only occupies the abstract name to guarantee there's only one nervud at a time.
	// See acquireSingletonLock
	lockLn *net.UnixListener

	// quit is closed by Stop and is the only exit signal of every background goroutine
	quit     chan struct{}
	quitOnce sync.Once
	wg       sync.WaitGroup

	// fatal carries the error when a background loop can no longer continue; capacity 1, written once
	fatal chan error

	mu    sync.Mutex
	conns map[*net.UnixConn]struct{}
	// controlConns resolves a control.ConnID back to its live IPC connection so
	// terminal lease notifications can retire the exact connection-scoped wire
	// handle. Entries are registered for the whole serve lifecycle.
	controlConns map[control.ConnID]*conn
	perUID       map[uint32]int
	started      bool

	// dispatch is the single authority for route_id -> in-flight Dispatch (dispatch.go),
	// handleRequest/handleDispatchResult/runDispatchReaper/dispatchConnClosed
	// which all consume it
	dispatch *dispatchTable

	// rejectLog rate-limits auditing on the admission refusal path. Refused connections can be flooded at will by an attacker,
	// and without rate limiting the audit log itself would become an amplifier
	rejectLog rateLimiter

	// violationLog rate-limits auditing on the protocol violation path. Malformed frames can likewise be flooded by malicious connections,
	// so it needs rate limiting for the same reason as rejectLog
	violationLog rateLimiter

	// dispatchRaceLog rate-limits auditing of "late/unknown route_id DispatchResults", a separate bucket from
	// violationLog: this is an expected, normal race, and shouldn't compete with real protocol violation signals
	// for the same audit budget (see auditDispatchRace in dispatch.go)
	dispatchRaceLog rateLimiter
}

func New(cfg Config) (*Server, error) {
	if cfg.SockPath == "" {
		return nil, errors.New("ipc: SockPath is required")
	}
	if !filepath.IsAbs(cfg.SockPath) {
		// A relative path's meaning depends on the process cwd, which a systemd unit or a runtime chdir
		// can change, handing the control-plane entry point to external state
		return nil, fmt.Errorf("ipc: SockPath %q must be absolute", cfg.SockPath)
	}
	if cfg.Log == nil {
		return nil, errors.New("ipc: Log is required")
	}
	if cfg.Auditor == nil {
		return nil, errors.New("ipc: Auditor is required")
	}
	if cfg.Invariants == nil {
		return nil, errors.New("ipc: Invariants is required")
	}
	if cfg.Identity == nil {
		// No default implementation: without identity resolution the only safe default is "recognise nobody",
		// which amounts to a socket nobody can connect to. Wiring should catch it
		return nil, errors.New("ipc: Identity is required")
	}
	if cfg.Permission == nil {
		// As with Identity: no default implementation. There's no safe default for a missing permission query; it must surface
		// at wiring, not at run time when it's found that permission queries never really took effect
		return nil, errors.New("ipc: Permission is required")
	}
	if cfg.Transfer == nil {
		return nil, errors.New("ipc: Transfer is required")
	}

	return &Server{
		sockPath:        cfg.SockPath,
		log:             cfg.Log,
		auditor:         cfg.Auditor,
		inv:             cfg.Invariants,
		identity:        cfg.Identity,
		permission:      cfg.Permission,
		components:      cfg.Components,
		endpoints:       cfg.Endpoints,
		subscriptions:   subscription.New(),
		eventScopes:     newEventScopes(),
		leases:          cfg.Leases,
		launcher:        cfg.Launcher,
		resources:       cfg.Resources,
		operations:      cfg.Operations,
		transfer:        cfg.Transfer,
		limits:          normalizeLimits(cfg.Limits),
		monotonicNow:    sysprobe.MonotonicNanos,
		quit:            make(chan struct{}),
		fatal:           make(chan error, 1),
		conns:           make(map[*net.UnixConn]struct{}),
		controlConns:    make(map[control.ConnID]*conn),
		perUID:          make(map[uint32]int),
		dispatch:        newDispatchTable(),
		rejectLog:       newRateLimiter(10, time.Second),
		violationLog:    newRateLimiter(10, time.Second),
		dispatchRaceLog: newRateLimiter(10, time.Second),
	}, nil
}

func (s *Server) Name() string { return "ipc" }

// Fatal implements kernel.FatalReporter: when the accept loop has failed repeatedly to the point of giving up, it reports through this channel,
// so the Kernel can shut modules down in reverse and exit non-zero, instead of leaving a falsely healthy process that has lost its entry point
func (s *Server) Fatal() <-chan error { return s.fatal }

// Start sets up the listener and starts the accept loop. ctx is only used during Start and isn't held by background loops
func (s *Server) Start(context.Context) error {
	s.mu.Lock()
	if s.started {
		s.mu.Unlock()
		return errors.New("ipc: already started")
	}
	s.started = true
	s.mu.Unlock()

	// 1. Take the singleton lock first. It guarantees that from this line on, this process is the only nervud, so the cleanup of leftover
	// sockets after it can be unconditionally safe (no live instance could own that socket)
	if err := s.acquireSingletonLock(); err != nil {
		return err
	}
	// Once the lock is taken, any failing step in Start must give it back, or the same sockPath could never
	// start again (the abstract name would stay held by this process)
	ok := false
	defer func() {
		if !ok {
			_ = s.lockLn.Close()
			s.lockLn = nil
		}
	}()

	// 2. The parent directory is created by systemd's RuntimeDirectory=nervus (0755, owned by root).
	// It's only checked here, not created: creating directories is a privileged filesystem operation that should go through authority, and having ipc
	// MkdirAll itself would open one more write path in the kernel that bypasses the Gate
	dir := filepath.Dir(s.sockPath)
	fi, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf("ipc: runtime dir %s unavailable (systemd RuntimeDirectory=): %w", dir, err)
	}
	if !fi.IsDir() {
		return fmt.Errorf("ipc: runtime dir %s is not a directory", dir)
	}

	// 3. Clear leftovers. With the lock held, any socket file left behind must be dead
	if err := s.clearStaleSocket(); err != nil {
		return err
	}

	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: s.sockPath, Net: "unix"})
	if err != nil {
		return fmt.Errorf("ipc: listen %s: %w", s.sockPath, err)
	}
	// State it explicitly: delete the socket file when closing the listener, rather than relying on default behaviour
	ln.SetUnlinkOnClose(true)

	// At bind time the mode is reduced by umask (usually to 0755), so it must be rewritten explicitly.
	// The window in between is safe: during it the mode can only be stricter, never looser
	if err := os.Chmod(s.sockPath, socketMode); err != nil {
		_ = ln.Close()
		return fmt.Errorf("ipc: chmod %s: %w", s.sockPath, err)
	}

	s.ln = ln
	s.wg.Add(1)
	go s.acceptLoop()
	s.wg.Add(1)
	go s.runDispatchReaper()

	ok = true
	s.log.Info("ipc: listening", "sock", s.sockPath, "mode", socketMode.String(),
		"max_conns", s.limits.MaxConns, "max_conns_per_uid", s.limits.MaxConnsPerUID)
	return nil
}

// singletonLockName derives the abstract namespace socket name from sockPath.
//
// The @ prefix makes the net package treat it as an abstract socket (Go convention: the first byte is replaced by NUL).
// Derived from sockPath rather than a hard-coded global name: instances with different sockPaths don't interfere (test-
// friendly), and instances with the same sockPath exclude each other (correct in production, where the path is fixed as /run/nervus/nervud.sock)
func (s *Server) singletonLockName() string { return "@" + s.sockPath }

// acquireSingletonLock uses the atomicity of binding an abstract namespace socket as a singleton lock.
//
// Why this rather than probing with dial + unlinking leftovers:
//
// The kernel guarantees an abstract socket's bind is atomic: if two processes bind the same name at once, only one
// succeeds and the other gets EADDRINUSE. That removes in one step the TOCTOU between "probe and judge dead -> unlink -> rebind":
// two instances can't both decide "the old socket is dead" and then each bind successfully.
//
// Also, abstract sockets aren't in the filesystem, and the kernel reclaims them when the process exits: no leftovers,
// and no need for the whole "dead or alive probe" machinery. Holding the lock is itself proof that no other live instance exists.
//
// Trade-off (accepted on purpose): abstract names don't go through filesystem permissions, and any process in the netns can bind them,
// so a malicious local process could grab this name first to stop nervud starting. That's acceptable for this deployment:
// nervud starts first at boot (before any app), so nobody competes for the lock; the window where it crashes and restarts while a malicious
// app happens to grab it is self-healed by systemd StartLimitAction=reboot. A really squat-proof
// solution is flock on a root-owned lock file (that needs a depguard decision; discussed separately)
func (s *Server) acquireSingletonLock() error {
	name := s.singletonLockName()
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: name, Net: "unix"})
	if err != nil {
		// Almost always EADDRINUSE: another nervud holds the lock. It could also have been squatted
		return fmt.Errorf("ipc: cannot acquire singleton lock %q (another nervud running?): %w", name, err)
	}
	s.lockLn = ln
	return nil
}

// Stop stops accepting and reclaims every connection.
//
// The order is fixed: first close(quit) so the loop knows this is a planned shutdown, then close the listener to interrupt the blocked
// Accept, and finally force-close live connections to interrupt blocked Reads. The other way round, Accept would first
// return an error before the loop knew it should exit, and go into the error-handling and backoff branch
func (s *Server) Stop(ctx context.Context) error {
	s.quitOnce.Do(func() { close(s.quit) })

	s.mu.Lock()
	ln := s.ln
	conns := make([]*net.UnixConn, 0, len(s.conns))
	for c := range s.conns {
		conns = append(conns, c)
	}
	s.mu.Unlock()

	var errs []error
	if ln != nil {
		if err := ln.Close(); err != nil {
			errs = append(errs, fmt.Errorf("close listener: %w", err))
		}
	}
	for _, c := range conns {
		_ = c.Close()
	}

	done := make(chan struct{})
	go func() { s.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
		// On timeout, give up waiting and return an error. The process still exits, and safety is backed by systemd restart and
		// the MCU heartbeat brake
		errs = append(errs, fmt.Errorf("goroutines not drained: %w", ctx.Err()))
	}

	// Release the singleton lock last, after reclaiming connections: as long as cleanup is in progress the lock stays held,
	// leaving no window for a new instance jumping ahead to think "the previous one has fully exited".
	// Closing an abstract socket lets the kernel reclaim it, with no file left behind
	if s.lockLn != nil {
		if err := s.lockLn.Close(); err != nil {
			errs = append(errs, fmt.Errorf("close singleton lock: %w", err))
		}
		s.lockLn = nil
	}

	s.log.Info("ipc: stopped")
	return errors.Join(errs...)
}

// clearStaleSocket clears leftovers at the main socket path.
//
// Precondition: the caller already holds the singleton lock (acquireSingletonLock). So no dead-or-alive
// probe is needed here: the lock already proves there's no other live instance, so if there's a socket at the path, it must be a leftover from
// the previous run (a crash or SIGKILL with no time to unlink), and it can be deleted unconditionally.
//
// The one line of defence kept: if the path exists but isn't a socket, refuse rather than delete. In case sockPath
// was misconfigured to point at an ordinary file, a blind delete can't be undone; this has nothing to do with the singleton lock and guards against slips
func (s *Server) clearStaleSocket() error {
	fi, err := os.Lstat(s.sockPath)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("ipc: stat %s: %w", s.sockPath, err)
	}

	if fi.Mode()&fs.ModeSocket == 0 {
		return fmt.Errorf("ipc: %s exists and is not a socket (mode %s); refusing to remove", s.sockPath, fi.Mode())
	}

	// With the lock held, this socket must be a leftover
	s.log.Warn("ipc: removing stale socket", "sock", s.sockPath)
	if err := os.Remove(s.sockPath); err != nil {
		return fmt.Errorf("ipc: remove stale socket %s: %w", s.sockPath, err)
	}
	return nil
}

// acceptBackoffMax is the backoff ceiling when accept fails repeatedly
const acceptBackoffMax = 200 * time.Millisecond

// acceptFailureLimit is the number of consecutive failures allowed before giving up.
//
// It exists because backoff can't retry forever: if the listening fd enters a permanent error state, an endless loop would turn the kernel
// into an empty shell holding a socket but never accepting anyone, and from outside you couldn't tell
const acceptFailureLimit = 32

func (s *Server) acceptLoop() {
	defer s.wg.Done()

	var backoff time.Duration
	failures := 0

	for {
		c, err := s.ln.AcceptUnix()
		if err != nil {
			// Planned shutdown: Stop has already done close(quit) and closed the listener
			select {
			case <-s.quit:
				return
			default:
			}

			failures++
			if failures >= acceptFailureLimit {
				err = fmt.Errorf("ipc: accept failed %d times consecutively: %w", failures, err)
				s.log.Error("ipc: accept loop aborting", "err", err)
				select {
				case s.fatal <- err:
				default:
				}
				return
			}

			// No spinning: errors like EMFILE/ENFILE recur at once, and retrying at full speed would only burn a whole core
			// without solving anything
			if backoff == 0 {
				backoff = time.Millisecond
			} else if backoff < acceptBackoffMax {
				backoff *= 2
			}
			s.log.Warn("ipc: accept failed, backing off",
				"err", err, "backoff", backoff, "consecutive", failures)

			select {
			case <-time.After(backoff):
			case <-s.quit:
				return
			}
			continue
		}

		backoff, failures = 0, 0
		s.admit(c)
	}
}

// admit makes the admission decision for a new connection.
//
// The checks are ordered from cheapest to most expensive, and all finish before allocating a goroutine and read buffer. In the other
// order, an attacker could use connections bound to be refused and still force the kernel to allocate 128 KiB buffers first
func (s *Server) admit(c *net.UnixConn) {
	cred, err := sysprobe.PeerCred(c)
	if err != nil {
		// Without kernel credentials there's no attribution, so disconnect. Normal connections never get here
		s.reject(c, 0, "peer credentials unavailable", err)
		return
	}

	// App UID range check: one integer comparison keeps system accounts, login users and root out before any resource
	// is allocated. This invariant is owned by authority and reused here rather than redeclared
	if err := s.inv.CheckUID(cred.UID); err != nil {
		s.reject(c, cred.UID, "uid outside app range", err)
		return
	}

	// Identity resolution: a UID in the range doesn't mean it belongs to a registered Package. Manually created
	// system users, or a process still alive after its Package was uninstalled, end up here and are refused.
	//
	// It comes after the range check: that's one integer comparison, while this step is an atomic Load plus a map lookup,
	// cheap before expensive
	caller, err := s.identity.Resolve(cred)
	if err != nil {
		s.reject(c, cred.UID, "uid maps to no registered package", err)
		return
	}

	s.mu.Lock()
	switch {
	case len(s.conns) >= s.limits.MaxConns:
		s.mu.Unlock()
		s.reject(c, cred.UID, "process-wide connection limit reached", nil)
		return
	case s.perUID[cred.UID] >= s.limits.MaxConnsPerUID:
		s.mu.Unlock()
		s.reject(c, cred.UID, "per-uid connection limit reached", nil)
		return
	}
	s.conns[c] = struct{}{}
	s.perUID[cred.UID]++
	s.mu.Unlock()

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer s.release(c, cred.UID)
		s.serve(c, caller)
	}()
}

// release removes the connection from the registry. It must pair strictly with the registration in admit, otherwise the perUID count
// only ever goes up and that UID would be locked out of its quota permanently
func (s *Server) release(c *net.UnixConn, uid uint32) {
	_ = c.Close()

	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.conns[c]; !ok {
		return
	}
	delete(s.conns, c)
	if n := s.perUID[uid]; n <= 1 {
		delete(s.perUID, uid)
	} else {
		s.perUID[uid] = n - 1
	}
}

func (s *Server) reject(c *net.UnixConn, uid uint32, reason string, err error) {
	_ = c.Close()
	if !s.rejectLog.allow() {
		return
	}
	s.auditor.Record(context.Background(), audit.Event{
		Action:  "ipc.ConnectionRejected",
		Subject: fmt.Sprintf("uid:%d", uid),
		Denied:  true,
		Err:     err,
		Detail:  reason,
	})
}

// serve is the frame pump of a single connection: framing, timeouts, per-frame admission re-checks, then handing each well-formed Envelope
// to the connection state machine (conn). The handshake (Hello/HelloAck version negotiation, sending ConnectionLimits)
// and post-handshake body dispatch are both in conn.go
func (s *Server) serve(c *net.UnixConn, caller identity.Caller) {
	log := s.log.With("package", caller.PackageID, "uid", caller.UID, "pid", caller.PID)

	// One read buffer per connection, reused, so frame reads allocate nothing in the steady state.
	// MaxConns sets the upper bound of this memory
	buf := make([]byte, MaxFrameBytes)

	// The inbound frame rate gate, one per connection (the first line; see Limits.MaxFramesPerConnPerSec)
	frameRate := newRateLimiter(s.limits.MaxFramesPerConnPerSec, time.Second)

	// Connection state machine: the first frame must be Hello, and no other body is accepted before the handshake completes (conn.go)
	co := newConn(s, c, caller, log)
	s.mu.Lock()
	s.controlConns[co.connID] = co
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		if s.controlConns[co.connID] == co {
			delete(s.controlConns, co.connID)
		}
		s.mu.Unlock()
	}()

	// A separate writer goroutine: all of this connection's outbound frames queue through co.outbox,
	// and only it really calls co.c.Write (runWriter in conn.go). It starts before the read loop
	// so the handshake's HelloAck takes the same path
	s.wg.Add(1)
	go co.runWriter()

	// The three defers below run LIFO, so the actual order when serve returns is:
	//  1. dispatchConnClosed: remove entries in the route table with this connection as target/source
	//     (results go to other connections' outboxes and don't affect this connection's own cleanup)
	//  2. endpoints.ConnClosed: tell endpoint to clean up everything under this connection,
	//     invalidating endpoint registrations and bindings
	//  3. close this connection's own outbox and wait for the writer to really exit; only after it stops touching the socket
	//     can admit's release safely Close the underlying connection
	defer func() {
		co.outbox.close()
		<-co.writerDone
	}()
	if s.endpoints != nil {
		// co itself is passed as the endpoint.ConnHandle: endpoint_ids in the two namespaces
		// are told apart by this pointer identity, not by the numbers themselves
		defer s.endpoints.ConnClosed(co)
	}
	if s.subscriptions != nil {
		// Clear every subscription under this connection.
		//
		// No SubscriptionClosed is sent: the peer is gone, so there's nobody to send it to. That's different from ending
		// a single subscription because of backpressure; then the connection is still alive and must be told which one ended.
		//
		// Cleanup for this connection as a Provider is handled by the endpoint invalidation chain triggered by
		// endpoints.ConnClosed, not here.
		defer s.subscriptions.CloseConn(co)
	}
	if s.eventScopes != nil {
		// Both directions must be cleared: this connection may be the registering side (Provider) or the registered
		// owner (caller). Missing either side would leave entries that can never be cleared.
		defer s.eventScopes.closeProvider(co)
		defer s.eventScopes.closeOwner(co)
	}
	if s.leases != nil {
		// Revoke every ControlLease under this connection. Leases are bound to the connection and lapse on disconnect; without revoking,
		// a disconnected App would still "hold" actuator control, which nobody could take away until the TTL
		// expired naturally. For a robot that's a stretch of time when nobody can move it.
		//
		// Registered after endpoints.ConnClosed = runs before it (defers are last in, first out):
		// revoke motion authorisation first, then tear down routes. The other way round, there'd be a window between tearing down routes and revoking leases
		// in which this connection's lease is still valid while its routes are gone.
		defer s.leases.RevokeConn(co.leaseConnID())
	}
	// The transfer data plane is also connection-scoped. Register this before
	// dispatch cleanup so LIFO order first closes route authority/tokens and
	// then tears down every retained stream owned by this connection.
	defer s.transfer.ConnClosed(transfer.ConnID(co.connID))
	defer s.dispatchConnClosed(co)

	for {
		select {
		case <-s.quit:
			return
		default:
		}

		// The read window depends on the conn's current phase: during the handshake the much shorter HandshakeTimeout,
		// after the handshake the IdleTimeout kept alive by Ping/Pong (see conn.readDeadline)
		if err := c.SetReadDeadline(time.Now().Add(co.readDeadline())); err != nil {
			return
		}
		n, err := ReadFrameHeader(c)
		if err != nil {
			s.logConnExit(log, co.caller, err)
			return
		}

		// The length has arrived, so the body must follow soon; switch to the much shorter body deadline
		if err := c.SetReadDeadline(time.Now().Add(s.limits.FrameBodyTimeout)); err != nil {
			return
		}
		body, err := ReadFrameBody(c, buf, n)
		if err != nil {
			s.logConnExit(log, co.caller, err)
			return
		}

		// Frame rate gate: an App flooding frames in an infinite loop is cut off here. Normal clients never reach it
		if !frameRate.allow() {
			log.Warn("ipc: inbound frame rate exceeded, closing connection",
				"limit_per_sec", s.limits.MaxFramesPerConnPerSec)
			s.auditViolation(co.caller, errFrameRateExceeded)
			return
		}

		// Re-check identity liveness on every frame: the UID is stable, so looking it up again finds a Package that was uninstalled
		// or whose Trust changed (a quick liveness re-check on every call). If found, disconnect;
		// the client re-handshakes with its new identity when it reconnects.
		//
		// The coverage is limited, to be clear: this only catches identity-level revocations such as uninstall/downgrade,
		// and only triggers when the client sends a frame; a silent idle connection waits for IdleTimeout
		// to be reclaimed. Fine-grained Permission revocation and actively disconnecting idle connections belong to
		// the permission module and the Envelope layer (EndpointRevoked), not here
		if !s.identityStillValid(co.caller) {
			log.Warn("ipc: caller identity revoked mid-connection, closing")
			s.auditViolation(co.caller, errIdentityRevoked)
			return
		}

		// First decode the small outer structure and check it's well-formed, before any business dispatch. The order matters:
		// only after identity, endpoint, method, length and permission have all passed is the generated code used to decode the method
		// payload, so unauthorised input can't drive business parsers directly
		env, err := parseEnvelope(body)
		if err != nil {
			s.logConnExit(log, co.caller, err)
			return
		}

		// Connection state machine handling: handshake negotiation, sending ConnectionLimits, dispatch by body after the handshake.
		// Returning false means this connection should be closed (the reason has been audited/logged by co)
		if !co.handle(env) {
			return
		}
	}
}

var (
	// errComponentUnverifiable: can't verify (the verification infrastructure isn't wired, the peer isn't in a managed cgroup,
	// or a capability gap such as a kernel too old for SO_PEERPIDFD). It's a capability gap, not an attack: fail closed but not a violation
	errComponentUnverifiable = errors.New("ipc: component verification unavailable")

	// errComponentMismatch: verification found the client's self-reported Component disagrees with kernel facts (UID/package/
	// component ID don't match, the unit isn't a managed component, or the component is disabled). Potential impersonation; always refused and audited
	errComponentMismatch = errors.New("ipc: declared component does not match kernel facts")
)

// verifyComponent is where the claim is verified rather than believed:
// it uses the peer process's cgroup to work out its systemd unit, gets through ComponentResolver the kernel facts of which Package's which
// Component the unit is, its UID and whether it's disabled, cross-checks them against the declared_component_id the client
// reported in Hello, and returns the verified Component ID.
//
// The two kinds of failure are kept strictly apart:
//   - errComponentUnverifiable (a capability gap): Components not wired, the peer not in a managed cgroup,
//     the kernel lacks SO_PEERPIDFD and the fallback also failed. Fail closed, but not audited as a violation.
//   - errComponentMismatch (potential impersonation): verification found a mismatch. Always refused and audited; trusting the self-report
//     would hand the identity decision to the peer, which is exactly what's forbidden.
func (s *Server) verifyComponent(
	uc *net.UnixConn,
	caller identity.Caller,
	declared string,
) (service.ComponentIdentity, error) {
	var inst service.ComponentIdentity
	if s.verifyComponentForTest != nil {
		var err error
		inst, err = s.verifyComponentForTest(uc, caller, declared)
		if err != nil {
			return service.ComponentIdentity{}, err
		}
	} else {
		if s.components == nil {
			return service.ComponentIdentity{}, errComponentUnverifiable
		}

		unit, err := s.resolvePeerUnit(uc, caller)
		if err != nil {
			return service.ComponentIdentity{}, fmt.Errorf("%w: %v", errComponentUnverifiable, err)
		}

		var ok bool
		inst, ok = s.components.LookupComponentByUnit(unit)
		if !ok {
			// The peer's unit isn't a managed component started by nervud: possibly impersonation, or an unmanaged process connected
			return service.ComponentIdentity{}, fmt.Errorf("%w: unit %q is not a managed component", errComponentMismatch, unit)
		}
	}
	// Cross-check against kernel facts (any mismatch is potential impersonation)
	if inst.UID != caller.UID {
		return service.ComponentIdentity{}, fmt.Errorf("%w: unit uid %d != peer uid %d", errComponentMismatch, inst.UID, caller.UID)
	}
	if inst.PackageID != caller.PackageID {
		return service.ComponentIdentity{}, fmt.Errorf("%w: unit package %q != caller package %q", errComponentMismatch, inst.PackageID, caller.PackageID)
	}
	if inst.Generation == 0 || inst.Generation != caller.Generation {
		return service.ComponentIdentity{}, fmt.Errorf(
			"%w: unit runtime generation %d != caller generation %d",
			errComponentMismatch, inst.Generation, caller.Generation)
	}
	if declared != "" && declared != inst.ComponentID {
		return service.ComponentIdentity{}, fmt.Errorf("%w: declared %q != actual %q", errComponentMismatch, declared, inst.ComponentID)
	}
	if inst.State == service.StateDisabled {
		return service.ComponentIdentity{}, fmt.Errorf("%w: component %q is disabled", errComponentMismatch, inst.ComponentID)
	}
	return inst, nil
}

// resolvePeerUnit works out the systemd unit the peer process currently belongs to.
//
// It prefers SO_PEERPIDFD (a stable reference, free of PID recycling races) to read the cgroup; when unavailable (kernel <6.5) it
// falls back to the SO_PEERCRED PID + re-checking that /proc/<pid>'s owner still equals the SO_PEERCRED UID before reading the cgroup.
// The fallback path's residual risk: component A impersonating component B within the same Package (same UID, same data directory).
// Recorded honestly rather than pretending it's solved
func (s *Server) resolvePeerUnit(uc *net.UnixConn, caller identity.Caller) (string, error) {
	cgroup, err := sysprobe.PeerCgroupViaPIDFD(uc)
	if err != nil {
		// Fallback: SO_PEERCRED PID + /proc owner re-check
		owner, oerr := sysprobe.ProcOwnerUID(caller.PID)
		if oerr != nil {
			return "", fmt.Errorf("pidfd path failed (%v); proc owner recheck failed: %v", err, oerr)
		}
		if owner != caller.UID {
			return "", fmt.Errorf("proc owner uid %d != peer uid %d (PID reused?)", owner, caller.UID)
		}
		cgroup, err = sysprobe.CgroupPath(caller.PID)
		if err != nil {
			return "", err
		}
	}
	unit, ok := unitFromCgroup(cgroup)
	if !ok {
		return "", fmt.Errorf("cgroup %q carries no nervus unit", cgroup)
	}
	return unit, nil
}

// unitFromCgroup extracts the systemd unit name started by nervud from a cgroup v2 path.
//
// A managed component's cgroup looks like ".../system.slice/nervus-<pkg>-<comp>.service" (possibly with nested
// .slice levels). It takes the last segment in the path that starts with nervus- and ends with .service
func unitFromCgroup(cgroup string) (string, bool) {
	segs := strings.Split(cgroup, "/")
	for i := len(segs) - 1; i >= 0; i-- {
		seg := segs[i]
		if strings.HasPrefix(seg, "nervus-") && strings.HasSuffix(seg, ".service") {
			return seg, true
		}
	}
	return "", false
}

// ConnectionLimits are the connection budgets sent to the SDK.
//
// In-flight request counts, payload and outbound queue use fixed defaults. Method timeouts
// are declared per method by the Registry; the subscription count takes a conservative default that can be tightened per device profile later.
//
// Every field is sent non-zero: proto3 scalars default to 0, and the protocol doesn't define "0 = not implemented yet",
// so a missing value would be read by the SDK as "no in-flight requests / subscriptions / timeout allowed". Enforcement of these budgets
// lands with the Envelope layer, but the values sent must already be meaningful guides for self-discipline
const (
	defaultMethodPayloadBytes = 16 << 10  // default request/response for ordinary methods
	maxInflightRequests       = 64        //
	maxInflightPayloadBytes   = 1 << 20   //: 1 MiB
	maxOutboundQueueBytes     = 512 << 10 //: 512 KiB
	maxSubscriptions          = 64        // conservative default, to tune per device profile later
	defaultMethodTimeoutMs    = 5_000     // used when Request.timeout_ms=0 (the method Registry refines it)
	maxMethodTimeoutMs        = 30_000    // the ceiling nervud tightens caller timeouts to
)

// connectionLimits assembles the ConnectionLimits sent on this connection. idle_timeout_ms comes from the actually enforced
// Limits.IdleTimeout, and the rest from the fixed or conservative constants above
func (s *Server) connectionLimits() *ipcv1.ConnectionLimits {
	return &ipcv1.ConnectionLimits{
		MaxFrameBytes:             MaxFrameBytes,
		DefaultMethodPayloadBytes: defaultMethodPayloadBytes,
		MaxInflightRequests:       maxInflightRequests,
		MaxInflightPayloadBytes:   maxInflightPayloadBytes,
		MaxOutboundQueueBytes:     maxOutboundQueueBytes,
		MaxSubscriptions:          maxSubscriptions,
		DefaultTimeoutMs:          defaultMethodTimeoutMs,
		MaxTimeoutMs:              maxMethodTimeoutMs,
		IdleTimeoutMs:             uint32(s.limits.IdleTimeout / time.Millisecond),
	}
}

// identityStillValid re-resolves the identity with the credentials from connection setup and decides whether it's still valid and unchanged.
//
// It reuses the kernel credential fields already in caller (UID/GID/PID all come from SO_PEERCRED
// and don't change while the connection lives), without reading the socket again
func (s *Server) identityStillValid(caller identity.Caller) bool {
	fresh, err := s.identity.Resolve(sysprobe.Ucred{UID: caller.UID, GID: caller.GID, PID: caller.PID})
	if err != nil {
		return false // the Package has been uninstalled
	}
	// A change of Package ID, Trust or install/run generation all count as a changed identity. The generation check is the upgrade
	// boundary: the old and new processes share a UID, but an old connection mustn't inherit permissions the new version just published.
	return fresh.PackageID == caller.PackageID &&
		fresh.Trust == caller.Trust &&
		fresh.Generation == caller.Generation
}

// Sentinel errors for violation audits, so offline rules can classify audit records precisely
var (
	errFrameRateExceeded = errors.New("inbound frame rate exceeded")
	errIdentityRevoked   = errors.New("caller identity revoked mid-connection")
)

// logConnExit tells normal disconnects from protocol violations. They're handled and audited completely differently, and mixing them would
// bury real signs of attack in the noise of clients exiting normally
func (s *Server) logConnExit(log *slog.Logger, caller identity.Caller, err error) {
	switch {
	case errors.Is(err, io.EOF):
		log.Debug("ipc: peer closed connection")
	case isProtocolViolation(err):
		log.Warn("ipc: protocol violation, closing connection", "err", err)
		s.auditViolation(caller, err)
	case errors.Is(err, os.ErrDeadlineExceeded):
		log.Debug("ipc: read deadline exceeded, closing connection")
	default:
		log.Debug("ipc: connection closed", "err", err)
	}
}

// auditViolation records one violation audit entry, rate-limited, with Subject filled in for attribution to a Package/UID.
//
// This path used to neither rate-limit (malformed frames could be flooded by malicious connections into an audit amplifier) nor fill in Subject
// (so there was no telling who did it). Both are fixed here
func (s *Server) auditViolation(caller identity.Caller, err error) {
	if !s.violationLog.allow() {
		return
	}
	s.auditor.Record(context.Background(), audit.Event{
		Action:  "ipc.ProtocolViolation",
		Subject: caller.String(),
		Denied:  true,
		Err:     err,
	})
}

// auditPublishRejected records one refused event report.
//
// A separate Action: a Provider pushing an endpoint it doesn't own or an event_id outside the contract
// is neither a protocol violation (the body itself is legal) nor a capability gap (this build implements it),
// but a Provider-side bug or an attempt to overstep. Mixing it into those two would bury the real signal.
// auditScopeRejected records one refused instance ownership registration.
//
// A separate Action: it's neither a protocol violation (the body is legal) nor a capability gap (it's implemented),
// and mixing it into those two would bury the real signal.
func (s *Server) auditScopeRejected(
	caller identity.Caller, endpointID, scope uint64, code ipcv1.StatusCode,
) {
	if !s.violationLog.allow() {
		return
	}
	s.auditor.Record(context.Background(), audit.Event{
		Action:  "ipc.BindEventScopeRejected",
		Subject: caller.String(),
		Denied:  true,
		Detail: fmt.Sprintf("endpoint=%d scope=%d code=%s",
			endpointID, scope, code),
	})
}

func (s *Server) auditPublishRejected(
	caller identity.Caller, endpointID uint64, eventID uint32, code ipcv1.StatusCode,
) {
	if !s.violationLog.allow() {
		return
	}
	s.auditor.Record(context.Background(), audit.Event{
		Action:  "ipc.PublishEventRejected",
		Subject: caller.String(),
		Denied:  true,
		Detail: fmt.Sprintf("endpoint=%d event=%d code=%s",
			endpointID, eventID, code),
	})
}

// auditUnsupported records an audit entry for receiving a body this build hasn't implemented yet, rate-limited, with Subject filled in.
//
// It uses a separate Action from auditViolation so offline rules can tell capability gaps (not implemented) from
// protocol violations / potential attacks; in the same Action, real signs of attack would be buried in the noise of
// unimplemented paths. It shares violationLog's token bucket: both end by closing the connection, so one connection
// produces at most one, and what rate limiting has to stop is many connections each flooding one; sharing is enough
func (s *Server) auditUnsupported(caller identity.Caller, err error) {
	if !s.violationLog.allow() {
		return
	}
	s.auditor.Record(context.Background(), audit.Event{
		Action:  "ipc.UnsupportedBody",
		Subject: caller.String(),
		Denied:  true,
		Err:     err,
	})
}

// rateLimiter is a minimal token bucket for audit paths.
//
// Not x/time/rate: that would add a TCB dependency for something twenty lines can do
type rateLimiter struct {
	mu       sync.Mutex
	burst    int
	tokens   int
	interval time.Duration
	last     time.Time
}

func newRateLimiter(burst int, interval time.Duration) rateLimiter {
	return rateLimiter{burst: burst, tokens: burst, interval: interval, last: time.Now()}
}

func (r *rateLimiter) allow() bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	now := time.Now()
	if now.Sub(r.last) >= r.interval {
		r.tokens = r.burst
		r.last = now
	}
	if r.tokens <= 0 {
		return false
	}
	r.tokens--
	return true
}

// ControlLeases is ipc's narrow interface dependency on internal/control: request, release, and on disconnect
// revoke every lease under that connection.
//
// The interface is defined by the consumer (ipc) and *control.Module satisfies it implicitly, the same pattern as EndpointResolver and
// PermissionChecker. ipc doesn't need control's renewal/pre-emption/snapshot capabilities,
// so they aren't in this interface.
type ControlLeases interface {
	Acquire(req control.Request) (control.Lease, error)
	Release(id control.ID, conn control.ConnID) error
	// CheckResource is the method-gate proof that this exact connection still
	// owns the lease for the resolved resource.
	CheckResource(conn control.ConnID, resource string, generation uint64) (control.LeaseProof, error)
	// CheckLease re-checks by lease handle, answering "is this handle still valid, and which resource does it hold".
	// Creating motion operations goes through it: that path only has a handle, and must confirm it
	// really covers the resource to be bound.
	CheckLease(id control.ID, conn control.ConnID) (control.LeaseProof, error)
	// RevokeConn revokes every lease under a connection.
	//
	// It must be called when a connection finishes. Leases are bound to this connection, can't be transferred, and lapse on disconnect
	//  (envelope.proto: AcquireControlSuccess.lease_id). Without revoking, a disconnected
	// App would still "hold" actuator control, which nobody could take away until the TTL expired naturally. For a robot
	// that means a stretch of time when nobody can move it.
	RevokeConn(conn control.ConnID)
	// RevokeResource invalidates leases issued against an obsolete catalog
	// generation before the same public handle can be reused.
	RevokeResource(resource string, generation uint64)
}

// OperationManager is ipc's narrow interface dependency on internal/operation.
//
// The interface is defined on the consumer (ipc) side and *operation.Manager satisfies it implicitly, the same pattern as
// ComponentResolver / PermissionChecker.
type OperationManager interface {
	// Create creates an operation when dispatch meets a returns_operation method.
	// conn is the caller connection, provider the executing connection.
	Create(conn, provider operation.ConnHandle, caller identity.Caller,
		origin operation.OriginBinding, resources []string,
		leaseID, epoch uint64, deadline time.Time) (uint64, ipcv1.StatusCode)

	// Get / Cancel are on the caller side, with built-in caller visibility decisions.
	Get(caller identity.Caller, id uint64) (operation.Operation, bool)
	Cancel(caller identity.Caller, id uint64) ipcv1.StatusCode

	// ProviderOperation is the ownership check on the reporting side: a Provider can only report on what was dispatched to it.
	ProviderOperation(provider operation.ConnHandle, id uint64) (operation.Operation, bool)

	// Accept / Progress / Succeed / Fail / Cancelled are the Provider's reporting seam.
	Accept(id, epoch uint64) error
	Progress(id uint64, payload []byte) error
	Succeed(id uint64, result []byte) error
	Fail(id uint64, code ipcv1.StatusCode, detail []byte) error
	Cancelled(id uint64) error

	// ReleaseByConn converges the unfinished operations under a connection when it disconnects.
	ReleaseByConn(conn operation.ConnHandle)
}

// ComponentLauncher is ipc's narrow interface dependency on internal/service: start a component,
// and answer whether it's running right now. *service.Manager satisfies it implicitly.
//
// It's split from ComponentResolver (the unit -> component reverse lookup used to verify identity at handshake) into two interfaces:
// the two differ in when they're called, what failure means and the permissions needed, and merging them would only blur
// "who needs what".
type ComponentLauncher interface {
	EnsureStarted(ctx context.Context, pkg, comp string) error
	IsRunning(pkg, comp string) bool
}

// ResourceResolver is ipc's narrow interface dependency on internal/resource: resolve (type, role)
// into a resource_handle.
// AcquireControl uses the narrower exclusive-control lookup so catalog entries
// declared SHARED_OBSERVE can be routed but never leased.
type ResourceResolver interface {
	ResolveControl(resourceType, role string) (handle string, generation uint64, ok bool)
	// ResolveControlBySelector supports labels filtering and multi-candidate policies. AcquireControl goes through it;
	// ResolveControl is kept for internal call sites that only have type/role
	ResolveControlBySelector(sel *ipcv1.ResourceSelector) (handle string, generation uint64, ok bool)
}

// resolveLeaseResource resolves AcquireControl's selector into a resource_handle.
//
// An empty selector is always refused. v1 used to take {motion.base, main} implicitly here; that default has been
// removed: it made "I didn't write a selector" and "I want the base" the same thing, so a call that forgot to fill it in
// would silently get the base's control lease, the very last kind of default a robot should have.
func (s *Server) resolveLeaseResource(sel *ipcv1.ResourceSelector) (string, uint64, bool) {
	if s.resources == nil {
		// No Resource directory means no lease authority. Production wiring injects the catalog-backed module
		return "", 0, false
	}
	if catalog.SelectorIsEmpty(sel) {
		return "", 0, false
	}
	return s.resources.ResolveControlBySelector(sel)
}

// auditLaunch records one successful component launch.
//
// Starting another component is a cross-Package action, and audit must be able to answer "who started it":
// when tracking down a component that shouldn't be running, this is the first place to look.
func (s *Server) auditLaunch(caller identity.Caller, pkg, comp string, alreadyRunning bool) {
	if s.auditor == nil {
		return
	}
	detail := "target=" + pkg + "/" + comp
	if alreadyRunning {
		detail += " already_running=true"
	}
	s.auditor.Record(context.Background(), audit.Event{
		Action:  "ipc.LaunchComponent",
		Subject: caller.PackageID,
		Detail:  detail,
	})
}

// auditLeaseClass records the currently accepted self-reported HUMAN/AI
// choice. Caller identity is kernel-verified, but controller_class is not yet
// backed by a trusted session or human-presence policy.
func (s *Server) auditLeaseClass(caller identity.Caller, class control.Class, resource string) {
	if s.auditor == nil {
		return
	}
	s.auditor.Record(context.Background(), audit.Event{
		Action:  "ipc.AcquireControl.classSelfReported",
		Subject: caller.PackageID,
		Detail:  "class=" + class.String() + " resource=" + resource,
	})
}
