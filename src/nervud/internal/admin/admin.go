// The admin server: SO_PEERCRED identity admission, then one Request -> handle -> Response -> close per connection.
// The individual commands are handled in handlers.go.
package admin

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
	"sync"
	"time"

	"github.com/nervus-os/nervud/internal/adminwire"
	"github.com/nervus-os/nervud/internal/audit"
	"github.com/nervus-os/nervud/internal/permission"
	"github.com/nervus-os/nervud/internal/pkgregistry"
	"github.com/nervus-os/nervud/internal/sysprobe"
)

// DefaultStagingDir is the dynamic-install staging root nervud controls. It sits under /var/lib/nervus like PackageRoot
//
// (/var/lib/nervus/packages), so both are on the same filesystem and the install-time
//
// renameat2 of a staging directory into PackageRoot can't fail across filesystems (EXDEV).
// preflight creates it at startup (0700, owned by nervud).
const DefaultStagingDir = "/var/lib/nervus/staging"

// PermissionPackageAdmin is the permission needed to connect to this channel. It's defined in the kernel catalog bootstrap
// (SYSTEM_ONLY + PLATFORM trust + the platform-release signing role).
//
// The kernel knows no specific Package ID: this used to be a
// "nervus.pkgmanagerd" constant in main.go, and is now a permission the package must declare explicitly and that has been decided on.
const PermissionPackageAdmin = "perm.pkg.admin"

// socketMode is the socket mode when no package is admitted: 0600, so only the owner (the account running nervud,
// root in production) can connect. That's the first, filesystem-level filter; the real admission is the
// SO_PEERCRED check after accept (see handleConn).
const socketMode fs.FileMode = 0o600

// socketModeWithService is the socket mode when one package is admitted: 0660, together with chowning the socket's
// group to that package's GID.
//
// Why the filesystem layer has to change: under 0600 the install service (UID 20000+) can't even connect(),
// so the SO_PEERCRED check never gets a chance to run. Admitting the UID only in handleConn would do nothing.
//
// Why not 0666 and let SO_PEERCRED guard alone: then any local process could connect and be refused,
// the filesystem layer would be decoration, and it would hand out a free way to use up connection slots. 0660 + the group lets in
// exactly "root and that one package".
//
// In this system a Package's GID always equals its UID (see GID: e.UID in service.buildStartReq),
// so the group is simply the package's UID, with no extra group management.
const socketModeWithService fs.FileMode = 0o660

// PackageService is this package's narrow dependency on pkgregistry.Module: install / uninstall / disable and enable.
// The consumer defines the interface and *pkgregistry.Module satisfies it implicitly. All security re-checks happen inside the Module;
// this package only forwards.
type PackageService interface {
	Install(ctx context.Context, tx pkgregistry.InstallTransaction) (pkgregistry.Entry, error)
	Uninstall(ctx context.Context, pkgID string) error
	SetComponentEnabled(ctx context.Context, pkgID, compID string, enabled bool) error
	// Inspect examines a package in staging without writing anything: parse + verify + compute the
	// USER_CONSENT permissions it requests, writing no state.
	//
	// The confirmation screen relies on it to know what to ask the user BEFORE installing. Without it, the only alternative is for the confirmation screen
	// to unpack and read the manifest itself, which would put untrusted parsing before signature verification inside a process holding
	// perm.permission.admin.
	Inspect(ctx context.Context, stagingDir string) (pkgregistry.InspectResult, error)
}

// PackageLister is the narrow dependency on pkgregistry.Registry: list every installed Package
//
// (the list command). *pkgregistry.Registry satisfies it implicitly.
type PackageLister interface {
	List() []pkgregistry.Entry
}

// PermissionSetter is the narrow dependency on permission.Registry: set runtime grant state
//
// (grant/revoke), and ask whether a package holds a permission (for admission).
//
// *permission.Registry satisfies it implicitly.
type PermissionSetter interface {
	SetRuntimeState(packageID, permission string, state permission.GrantState) error
	Allowed(packageID, permission string) bool
}

// Config is the admin server's wiring input.
type Config struct {
	// SockPath is the admin channel UDS path; always adminwire.DefaultSockPath in production.
	SockPath string
	// StagingRoot is the staging root nervud controls; defaults to DefaultStagingDir.
	StagingRoot string
	// AdminUID is the UID of the ops identity allowed to send admin commands. Passed explicitly at wiring (main.go passes
	// os.Geteuid = the account running nervud, 0/root in production). There's no default, because 0 is itself
	// a valid value, so a zero value can't mean "not set".
	AdminUID uint32

	Packages    PackageService
	Registry    PackageLister
	Permissions PermissionSetter
	Auditor     audit.Recorder
	Log         *slog.Logger
}

// Server owns the admin channel UDS. Lifecycle contract as ipc.Server: Start returns quickly, and the background accept
// loop listens only to the quit closed by Stop, not to Start(ctx)'s ctx.
type Server struct {
	sockPath    string
	stagingRoot string
	adminUID    uint32
	// allowedUIDs is the ops UID plus the UIDs of every package holding PermissionPackageAdmin.
	// Computed once in Start, read-only afterwards.
	//
	// A map rather than comparing one by one: the check runs on every connection, and set semantics are clearer.
	allowedUIDs map[uint32]struct{}

	// admittedUID is the UID of the admitted package, resolved in Start. The socket's group and the staging
	// directory's owner both use it; they must be the same value, otherwise you get split states like "can connect but can't write
	// to staging".
	//
	// 0 means no package is admitted (minimal image, development machine, or fail closed when there are several holders).
	// Then only root uses this channel, and nothing needs handing over.
	admittedUID uint32

	pkgs  PackageService
	reg   PackageLister
	perms PermissionSetter
	aud   audit.Recorder
	log   *slog.Logger

	ln *net.UnixListener

	quit     chan struct{}
	quitOnce sync.Once
	wg       sync.WaitGroup
	fatal    chan error
}

// New checks required dependencies and constructs the Server. Missing any security-relevant dependency fails at wiring; the admin
// channel has no half-working degraded mode.
func New(cfg Config) (*Server, error) {
	if cfg.SockPath == "" {
		cfg.SockPath = adminwire.DefaultSockPath
	}
	if !filepath.IsAbs(cfg.SockPath) {
		return nil, fmt.Errorf("admin: SockPath %q must be absolute", cfg.SockPath)
	}
	if cfg.StagingRoot == "" {
		cfg.StagingRoot = DefaultStagingDir
	}
	if !filepath.IsAbs(cfg.StagingRoot) {
		return nil, fmt.Errorf("admin: StagingRoot %q must be absolute", cfg.StagingRoot)
	}
	if cfg.Packages == nil {
		return nil, errors.New("admin: Packages is required")
	}
	if cfg.Registry == nil {
		return nil, errors.New("admin: Registry is required")
	}
	if cfg.Permissions == nil {
		return nil, errors.New("admin: Permissions is required")
	}
	if cfg.Auditor == nil {
		return nil, errors.New("admin: Auditor is required")
	}
	if cfg.Log == nil {
		return nil, errors.New("admin: Log is required")
	}
	// For now the allowed set holds only the ops identity. Packages holding perm.pkg.admin are added in Start:
	// during wiring the boot scan hasn't run, so neither UIDs nor permission decisions exist yet
	//  (see admitPermittedPackages).
	allowed := map[uint32]struct{}{cfg.AdminUID: {}}

	return &Server{
		sockPath:    cfg.SockPath,
		stagingRoot: filepath.Clean(cfg.StagingRoot),
		adminUID:    cfg.AdminUID,
		allowedUIDs: allowed,
		pkgs:        cfg.Packages,
		reg:         cfg.Registry,
		perms:       cfg.Permissions,
		aud:         cfg.Auditor,
		log:         cfg.Log,
		quit:        make(chan struct{}),
		fatal:       make(chan error, 1),
	}, nil
}

func (s *Server) Name() string { return "admin" }

// Fatal implements kernel.FatalReporter: reported when the accept loop fails unrecoverably, and the kernel then
// shuts down in reverse order and exits non-zero.
func (s *Server) Fatal() <-chan error { return s.fatal }

// Start sets up the listener and starts the accept loop. ctx is only used during Start.
func (s *Server) Start(context.Context) error {
	// The staging root is normally created by preflight (0700, owned by nervud). MkdirAll runs again here as a
	// backstop: when nervud starts with -dev-skip-preflight on a development machine, preflight doesn't run, and installs must still work.
	// MkdirAll is a no-op for an existing directory (owner/mode unchanged), so it doesn't conflict with preflight's squat
	// check; on the production path preflight runs first and has already checked the owner.
	if err := os.MkdirAll(s.stagingRoot, 0o711); err != nil {
		return fmt.Errorf("admin: ensure staging root %s: %w", s.stagingRoot, err)
	}
	// 0711 rather than 0700: system services must be able to pass through this root into their own stage-* directory,
	// but shouldn't be able to list what's in it; other packages' staging is none of their business.
	//
	// An explicit Chmod rather than relying on the MkdirAll above: MkdirAll is a no-op for an existing directory,
	// and preflight (or an older version of this function) creates it as 0700. Without this, installs would still fail on a machine upgraded
	// from an older version, with a "permission denied" that doesn't point at the root's mode bits.
	if err := os.Chmod(s.stagingRoot, 0o711); err != nil {
		return fmt.Errorf("admin: chmod staging root %s: %w", s.stagingRoot, err)
	}

	// The parent directory (/run/nervus) is guaranteed by systemd RuntimeDirectory + preflight;
	// this only clears leftovers and creates no directory (creating directories is a privileged filesystem operation outside this module's job).
	if err := s.clearStaleSocket(); err != nil {
		return err
	}

	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: s.sockPath, Net: "unix"})
	if err != nil {
		return fmt.Errorf("admin: listen %s: %w", s.sockPath, err)
	}
	ln.SetUnlinkOnClose(true)

	// Admit by permission. This must happen here rather than at wiring: this module is registered after pkgregistry,
	// so this function runs after the boot scan, when UIDs are allocated and permissions decided.
	admitted := s.admitPermittedPackages()

	// Order matters: chown the group first, then relax the mode.
	//
	// Doing it the other way round (0660 first, then chown) leaves a window in which the socket's group is still
	// nervud's primary group (root's group in production), and 0660 hands the right to connect to every member of the root
	// group. However short the window, it's really exploitable, and swapping the order costs nothing.
	mode := socketMode
	switch len(admitted) {
	case 0:
		// Only the ops identity; keep 0600
	case 1:
		s.admittedUID = admitted[0]
		// In this system a Package's GID always equals its UID. -1 means keep the owner and change only the group
		if err := os.Chown(s.sockPath, -1, int(s.admittedUID)); err != nil {
			_ = ln.Close()
			return fmt.Errorf("admin: chown group %s to %d: %w", s.sockPath, s.admittedUID, err)
		}
		mode = socketModeWithService
	default:
		// A Unix socket has only one group, so it can't express "admit several packages".
		//
		// Fail closed here and keep 0600 rather than picking one: picking one would make the other packages
		// look admitted in allowedUIDs while they can't even connect(), with the symptom
		// "permissions match but it won't connect", the hardest kind to track down.
		//
		// This shouldn't normally happen: perm.pkg.admin is SYSTEM_ONLY + PLATFORM +
		// platform-release, so a second holder means the platform build is misconfigured.
		// It only affects installs and the ops channel still works, so it doesn't bring down kernel startup.
		s.log.Error("admin: multiple packages hold "+PermissionPackageAdmin+
			"; a Unix socket has only one group, refusing all of them",
			"uids", admitted, "permission", PermissionPackageAdmin)
		s.allowedUIDs = map[uint32]struct{}{s.adminUID: {}}
	}

	// At bind time the mode is reduced by umask, so it's set explicitly here. 0600 when no package is admitted.
	if err := os.Chmod(s.sockPath, mode); err != nil {
		_ = ln.Close()
		return fmt.Errorf("admin: chmod %s: %w", s.sockPath, err)
	}

	s.ln = ln
	s.wg.Add(1)
	go s.acceptLoop()

	s.log.Info("admin: listening",
		"sock", s.sockPath, "mode", mode.String(),
		"admin_uid", s.adminUID, "admitted_uid", s.admittedUID)
	return nil
}

// Stop closes the listener and waits for the accept loop and in-flight connections to exit.
func (s *Server) Stop(ctx context.Context) error {
	s.quitOnce.Do(func() { close(s.quit) })

	var errs []error
	if s.ln != nil {
		if err := s.ln.Close(); err != nil {
			errs = append(errs, fmt.Errorf("close listener: %w", err))
		}
	}

	done := make(chan struct{})
	go func() { s.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
		errs = append(errs, fmt.Errorf("admin goroutines not drained: %w", ctx.Err()))
	}

	s.log.Info("admin: stopped")
	return errors.Join(errs...)
}

// clearStaleSocket removes the socket file left from the previous run. The admin channel has no singleton lock like ipc's
//
// (it isn't a concurrency hot spot), so one line of defence stays here: if the path exists but isn't a socket, refuse rather than
//
// delete blindly, in case SockPath was misconfigured to point at an ordinary file.
func (s *Server) clearStaleSocket() error {
	fi, err := os.Lstat(s.sockPath)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("admin: stat %s: %w", s.sockPath, err)
	}
	if fi.Mode()&fs.ModeSocket == 0 {
		return fmt.Errorf("admin: %s exists and is not a socket (mode %s); refusing to remove", s.sockPath, fi.Mode())
	}
	if err := os.Remove(s.sockPath); err != nil {
		return fmt.Errorf("admin: remove stale socket %s: %w", s.sockPath, err)
	}
	return nil
}

func (s *Server) acceptLoop() {
	defer s.wg.Done()
	for {
		c, err := s.ln.AcceptUnix()
		if err != nil {
			select {
			case <-s.quit:
				return // planned shutdown
			default:
			}
			// The listening fd failed: the admin channel doesn't retry with backoff to heal itself (it isn't a critical path); it reports
			// fatal so systemd restarts the whole of nervud, which is cleaner than spinning on a bad fd.
			err = fmt.Errorf("admin: accept: %w", err)
			s.log.Error("admin: accept loop aborting", "err", err)
			select {
			case s.fatal <- err:
			default:
			}
			return
		}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.handleConn(c)
		}()
	}
}

// connTimeout caps one admin connection's whole exchange, from connecting through reading the Request, handling it and writing
// the Response. Admin operations are fast (an install's renameat2 and disk writes take seconds), and 60s is far beyond any normal
// duration. Its job is to stop a peer that connects and never talks, or never reads the response, from hanging the handleConn
// goroutine and holding up the whole shutdown wait in Stop.
const connTimeout = 60 * time.Second

// handleConn handles one connection: SO_PEERCRED admission -> read one Request -> handle -> write Response
// -> close. One connection serves one command; there's no long-lived connection state machine.
func (s *Server) handleConn(c *net.UnixConn) {
	defer func() { _ = c.Close() }()

	// A finite deadline on the whole exchange: a slow or silent peer must not hang this goroutine (it would hold up Stop).
	if err := c.SetDeadline(time.Now().Add(connTimeout)); err != nil {
		return
	}

	cred, err := sysprobe.PeerCred(c)
	if err != nil {
		// No kernel credentials = no attribution, so disconnect. Normal connections never get here.
		s.audit("admin.Rejected", "", true, err, "peer credentials unavailable")
		return
	}
	if _, ok := s.allowedUIDs[cred.UID]; !ok {
		// Only the ops identity (the account running nervud / root) and the explicitly admitted system service
		//  (pkgmanagerd) may send admin commands. SO_PEERCRED is defence in depth: the UID comes from the kernel
		// and the peer can't forge it.
		//
		// The socket mode and group (see Start) are only the first filter. Leaving admission to filesystem group
		// membership alone would let ops-editable group membership decide; what's wanted here is "only these few
		// specific UIDs", so the decision has to rest on the kernel credentials.
		s.audit("admin.Rejected", fmt.Sprintf("uid:%d", cred.UID), true, nil, "uid not permitted")
		_ = adminwire.WriteTo(c, adminwire.Response{
			OK: false, Code: adminwire.CodeUnauthorized,
			Message: "not authorized to use the admin channel",
		})
		return
	}

	var req adminwire.Request
	if err := adminwire.ReadFrom(c, &req); err != nil {
		if !errors.Is(err, io.EOF) {
			s.log.Debug("admin: read request failed", "err", err)
		}
		return
	}

	resp := s.dispatch(context.Background(), req)
	if err := adminwire.WriteTo(c, resp); err != nil {
		s.log.Debug("admin: write response failed", "err", err)
	}
}

// audit records one admin-plane audit entry. Subject is attributed to the peer/package; Denied marks a refusal.
func (s *Server) audit(action, subject string, denied bool, err error, detail string) {
	s.aud.Record(context.Background(), audit.Event{
		Action: action, Subject: subject, Denied: denied, Err: err, Detail: detail,
	})
}

// admitPermittedPackages adds the UID of every package holding PermissionPackageAdmin to the
// allowed set.
//
// UID 0 is always dropped: root can only come in through the explicit AdminUID path, never slip in
// through the "admitted service" route. This isn't a theoretical risk: the zero value of a failed lookup
// happens to be 0.
//
// The criterion is the permission, not the Package ID. This used to compare against a
// "nervus.pkgmanagerd" hard-coded at wiring; now the kernel knows no specific package name and only recognises "whoever
// declared perm.pkg.admin in their manifest and passed the decision".
//
// The admitted surface is no wider for it: perm.pkg.admin is SYSTEM_ONLY + PLATFORM trust +
// the platform-release signing role, and IntersectAt keeps out dynamically installed packages, OEM packages and packages downgraded
// to Ordinary in development builds. The only difference is that the constraint now lives in the permission catalog,
// where it can be audited and tested, instead of in a constant in main.go.
//
// It must be called in Start rather than at wiring: this module is registered after pkgregistry, so
// this function runs after the boot scan, when UIDs are allocated and permissions decided. At wiring neither exists yet.
func (s *Server) admitPermittedPackages() []uint32 {
	var admitted []uint32
	for _, e := range s.reg.List() {
		pkgID := e.Manifest.PackageID
		if !s.perms.Allowed(pkgID, PermissionPackageAdmin) {
			continue
		}
		// UID 0 means the boot scan didn't allocate a UID to this package. Admitting 0 would admit root,
		// and root's admission is expressed separately by adminUID, so this must be refused, otherwise a
		// package whose allocation failed would silently gain the ops identity.
		if e.UID == 0 {
			s.log.Warn("admin: package holds "+PermissionPackageAdmin+" but has uid 0; refusing to admit",
				"package_id", pkgID)
			continue
		}
		s.allowedUIDs[e.UID] = struct{}{}
		admitted = append(admitted, e.UID)
		s.log.Info("admin: package admitted by permission",
			"package_id", pkgID, "uid", e.UID, "permission", PermissionPackageAdmin)
	}
	if len(admitted) == 0 {
		// Having no install service on a minimal image or development machine is normal: the channel falls back to the ops identity only.
		// No error and no relaxing: the missing link only means "can't install packages", which shouldn't bring down kernel startup.
		s.log.Info("admin: no package holds " + PermissionPackageAdmin + "; channel is operator-only")
	}
	return admitted
}
