// This file is the request contracts and Gate methods of three privileged operations, process lifecycle and package tree deletion:
//
//	StartSandboxedProcess   starts a sandboxed process through systemd StartTransientUnit
//	StopProcess             stops that unit
//	RemovePackageTree       recursively deletes an installed Package's code or data directory (for uninstall)
//
// Processes aren't started by authority with fork/exec directly but delegated to systemd: sandboxing, cgroup
// limits and UID dropping are all systemd unit properties, while authority only decides whether it's allowed +
// checks local invariants + audits. The real D-Bus is in the internal/authority/systemd subpackage.
package authority

import (
	"context"
	"fmt"

	"github.com/nervus-os/nervud/internal/authority/systemd"
)

// PlatformJREExec is the fixed java executable path of the platform's built-in JRE.
// For runtime=jvm components ExecStart must be exactly this; what's checked to be inside the package is the entry -jar points at
// and native_lib_dir (carried by ContainedPaths)
const PlatformJREExec = "/usr/lib/nervus/jre/bin/java"

// Runtime is how a process entry is started. authority defines its own small enum rather than importing pkgregistry:
// pkgregistry already imports authority (it calls InstallVerifiedPackage when installing), so the reverse dependency
// would form a cycle. service.Manager translates pkgregistry.Runtime into this type
type Runtime uint8

const (
	RuntimeNative Runtime = iota
	RuntimeJVM
)

func (r Runtime) valid() bool { return r == RuntimeNative || r == RuntimeJVM }

// ResourceLimits are a component's resource limits. A zero value means the item isn't set.
// Clamping limits by trust is service.Manager's job; authority only passes them through
type ResourceLimits struct {
	MemoryMaxBytes  uint64
	CPUQuotaPercent uint32
	TasksMax        uint64
}

// ---- StartSandboxedProcess ------------------------------------------------

// StartSandboxedProcessRequest asks to start a sandboxed process as a transient systemd unit
type StartSandboxedProcessRequest struct {
	UnitName string  // nervus-<pkgid>-<compid>.service (allowlisted again on the systemd side)
	Desc     string  // human-readable description, only for systemd status
	Runtime  Runtime // native | jvm
	ExecPath string  // native = the ELF in the package; jvm = PlatformJREExec
	Args     []string
	// ContainedPaths are in-package paths that must be inside PackageRoot (jvm's jar, native_lib_dir
	// and so on). Validate runs CheckContained on each, blocking escapes like java -jar /etc/shadow
	ContainedPaths    []string
	UID               uint32
	GID               uint32
	WorkingDir        string // must be under DataRoot
	Env               []string
	ReadWritePaths    []string // under ProtectSystem=strict the writable directories must be listed (the private data directory)
	ReadOnlyPaths     []string
	InaccessiblePaths []string
	Limits            ResourceLimits
	// When BindToUnit is non-empty, the component unit gets owner-death binding: when the bound unit (nervud.service in production)
	// stops or fails, the component stops with it, so components never stay held by systemd after nervud is SIGKILLed
	BindToUnit string
	// AllowDeviceAccess opens this component's access to host device nodes (PrivateDevices off,
	// DevicePolicy=auto). For semantics and constraints see systemd.Sandbox.AllowDeviceAccess.
	//
	// The Gate doesn't decide "who deserves it" a second time here: that's policy the caller (service.Manager) decides by
	// the Package's origin, and the Gate only passes it faithfully to systemd. The Gate guards
	// invariants (path containment, UID ranges), not policy.
	AllowDeviceAccess bool
	// BindReadOnlyPaths are host paths bind-mounted read-only into the component's namespace.
	// For semantics and constraints see systemd.Sandbox.BindReadOnlyPaths (its only current use is bringing
	// the X11 socket directory into the private /tmp after PrivateTmp).
	//
	// No CheckContained: these paths are by definition outside PackageRoot/DataRoot,
	// which is exactly why bind mounts exist. The values come from a fixed list in service.Manager,
	// never arbitrary paths from the manifest, otherwise a package could mount any host directory into its own
	// namespace.
	BindReadOnlyPaths []string

	// AmbientCapabilities are the Linux capability names to grant this component (such as CAP_NET_ADMIN).
	//
	// Components run as an App UID, not root, and get no capabilities by default, so operations like driving radios
	// or configuring networks all get EPERM. Name validity is guaranteed by pkgregistry's allowlist
	//  (privilege.go), and the Gate only forwards faithfully: as with AllowDeviceAccess,
	// "who deserves it" is the caller's policy, and the Gate guards invariants.
	AmbientCapabilities []string

	// ExtraAddressFamilies are socket address families allowed on top of the baseline (AF_UNIX/AF_INET/AF_INET6).
	//
	// This and capabilities are two unrelated walls: RestrictAddressFamilies is at the seccomp layer,
	// and no amount of capabilities gets round it.
	ExtraAddressFamilies []string
}

func (StartSandboxedProcessRequest) Kind() Kind { return KindStartSandboxedProcess }

func (r StartSandboxedProcessRequest) Detail() string { return r.UnitName }

// Validate branches by runtime (the key point): native's ExecPath must be
// inside the package, while jvm's ExecPath must be exactly the platform JRE; otherwise jvm components could never pass "exec inside the package",
// because java lives in /usr/lib/nervus/jre, not the package directory
func (r StartSandboxedProcessRequest) Validate(inv *Invariants) error {
	if !r.Runtime.valid() {
		return fmt.Errorf("%w: unknown runtime %d", ErrInvariantViolated, r.Runtime)
	}
	switch r.Runtime {
	case RuntimeNative:
		// Two code roots are accepted: dynamic installs are in PackageRoot, the system image in SystemPackageRoot
		if err := inv.CheckContainedInCodeRoot(r.ExecPath); err != nil {
			return err
		}
	case RuntimeJVM:
		if r.ExecPath != PlatformJREExec {
			return fmt.Errorf("%w: jvm exec must be %q, got %q", ErrInvariantViolated, PlatformJREExec, r.ExecPath)
		}
	}
	// In-package paths (jar / native_lib_dir) must fall inside one of the code roots
	for _, p := range r.ContainedPaths {
		if err := inv.CheckContainedInCodeRoot(p); err != nil {
			return err
		}
	}
	// The working directory must be the package's private data directory (under DataRoot)
	if err := inv.CheckContained(r.WorkingDir, inv.DataRoot); err != nil {
		return err
	}
	if err := inv.CheckUID(r.UID); err != nil {
		return err
	}
	return inv.CheckUID(r.GID)
}

// ProcessHandle is an opaque handle of a started unit. The unit name isn't exported: callers can only pass it back
// as is to StopProcess/WaitProcess, never construct a handle pointing at an arbitrary unit
type ProcessHandle struct{ unit string }

// Unit returns the systemd unit name the handle refers to, only for audit/diagnostics
func (h ProcessHandle) Unit() string { return h.unit }

func (g *Gate) StartSandboxedProcess(
	ctx context.Context, subj Subject, req StartSandboxedProcessRequest,
) (ProcessHandle, error) {
	return do(ctx, g, subj, req, g.osStartSandboxedProcess)
}

func (g *Gate) osStartSandboxedProcess(ctx context.Context, req StartSandboxedProcessRequest) (ProcessHandle, error) {
	if g.spawner == nil {
		return ProcessHandle{}, fmt.Errorf("%w: no systemd spawner configured", ErrUnsupportedPlatform)
	}
	rt := systemd.RuntimeNative
	if req.Runtime == RuntimeJVM {
		rt = systemd.RuntimeJVM
	}
	_ = rt // systemd.UnitSpec doesn't need to distinguish runtimes yet (ExecStart is already the final command); the translation is kept for future use
	spec := systemd.UnitSpec{
		Name:        req.UnitName,
		Description: req.Desc,
		ExecPath:    req.ExecPath,
		Args:        req.Args,
		UID:         req.UID,
		GID:         req.GID,
		WorkingDir:  req.WorkingDir,
		Env:         req.Env,
		BindToUnit:  req.BindToUnit,
		Limits: systemd.Limits{
			MemoryMaxBytes:  req.Limits.MemoryMaxBytes,
			CPUQuotaPercent: req.Limits.CPUQuotaPercent,
			TasksMax:        req.Limits.TasksMax,
		},
		Sandbox: systemd.Sandbox{
			ReadWritePaths:       req.ReadWritePaths,
			ReadOnlyPaths:        req.ReadOnlyPaths,
			InaccessiblePaths:    req.InaccessiblePaths,
			AllowDeviceAccess:    req.AllowDeviceAccess,
			BindReadOnlyPaths:    req.BindReadOnlyPaths,
			AmbientCapabilities:  req.AmbientCapabilities,
			ExtraAddressFamilies: req.ExtraAddressFamilies,
		},
	}
	if err := g.spawner.StartTransientUnit(ctx, spec); err != nil {
		return ProcessHandle{}, err
	}
	return ProcessHandle{unit: req.UnitName}, nil
}

// ---- StopProcess ----------------------------------------------------------

// StopProcessRequest stops a unit started earlier by StartSandboxedProcess
type StopProcessRequest struct {
	Handle ProcessHandle
}

func (StopProcessRequest) Kind() Kind { return KindStopProcess }

func (r StopProcessRequest) Detail() string { return r.Handle.unit }

func (r StopProcessRequest) Validate(*Invariants) error {
	if r.Handle.unit == "" {
		return fmt.Errorf("%w: empty process handle", ErrInvariantViolated)
	}
	return nil
}

func (g *Gate) StopProcess(ctx context.Context, subj Subject, req StopProcessRequest) error {
	_, err := do(ctx, g, subj, req, g.osStopProcess)
	return err
}

func (g *Gate) osStopProcess(ctx context.Context, req StopProcessRequest) (struct{}, error) {
	if g.spawner == nil {
		return struct{}{}, fmt.Errorf("%w: no systemd spawner configured", ErrUnsupportedPlatform)
	}
	return struct{}{}, g.spawner.StopUnit(ctx, req.Handle.unit)
}

// ---- WaitProcess ----------------------------------------------------------

// ExitInfo is a process's exit information when it reaches a terminal state, translated from the systemd unit state
type ExitInfo struct {
	Terminal   bool   // whether it has ended (inactive/failed)
	Result     string // systemd Result: success / exit-code / signal / oom-kill /...
	ExitStatus int
}

// WaitProcess blocks until the process ends and returns its exit information. It's an observation (it changes no system state),
// so it doesn't go through the do audit pipeline, but it still requires the spawner to be configured
func (g *Gate) WaitProcess(ctx context.Context, h ProcessHandle) (ExitInfo, error) {
	if g.spawner == nil {
		return ExitInfo{}, fmt.Errorf("%w: no systemd spawner configured", ErrUnsupportedPlatform)
	}
	if h.unit == "" {
		return ExitInfo{}, fmt.Errorf("%w: empty process handle", ErrInvariantViolated)
	}
	info, err := g.spawner.WaitUnit(ctx, h.unit)
	if err != nil {
		return ExitInfo{}, err
	}
	return ExitInfo{Terminal: info.Terminal(), Result: info.Result, ExitStatus: info.ExitStatus}, nil
}

// ---- RemovePackageTree ----------------------------------------------------

// RemovePackageTreeRequest recursively deletes an installed Package's code or data directory (for uninstall).
//
// Root states explicitly whether PackageRoot or DataRoot is being deleted from; arbitrary roots aren't accepted, so a single field
// can't make it recursively delete outside DataRoot/PackageRoot
type RemovePackageTreeRequest struct {
	Root string // must equal inv.PackageRoot or inv.DataRoot
	Path string // must be under Root
}

func (RemovePackageTreeRequest) Kind() Kind { return KindRemovePackageTree }

func (r RemovePackageTreeRequest) Detail() string { return r.Path }

func (r RemovePackageTreeRequest) Validate(inv *Invariants) error {
	// Root allowlist: only one of these two managed roots
	if r.Root != inv.PackageRoot && r.Root != inv.DataRoot {
		return fmt.Errorf("%w: remove root %q is neither PackageRoot nor DataRoot", ErrInvariantViolated, r.Root)
	}
	// Path must be strictly inside Root (Root itself doesn't count: emptying a whole root isn't allowed)
	return inv.CheckContained(r.Path, r.Root)
}

func (g *Gate) RemovePackageTree(ctx context.Context, subj Subject, req RemovePackageTreeRequest) error {
	_, err := do(ctx, g, subj, req, g.osRemovePackageTree)
	return err
}
