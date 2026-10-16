// This file builds StartTransientUnit properties and does the local allowlist checks.
//
// It only does pure data transformation (UnitSpec -> []property) and shape checks, making no D-Bus call,
// so it can be fully unit-tested without systemd. Which sandbox/limit properties, with which types:
// keeping that layer separate from really calling D-Bus (systemd.go) puts a regression lock on the property mapping, which is the easiest to get wrong.
//
// Dependency boundary: this subpackage is the only place in the repository allowed to import github.com/godbus/dbus/v5
// (.golangci.yml's godbus-boundary depguard rule). It belongs to nervud's TCB
// and is treated as a kernel dependency (pinned version, go.sum committed, included in the SBOM).
package systemd

import (
	"errors"
	"fmt"
	"strings"

	"github.com/godbus/dbus/v5"
)

var (
	// ErrInvalidUnitName: the unit name doesn't match the nervus-<pkg>-<comp>.service allowlist
	ErrInvalidUnitName = errors.New("systemd: invalid transient unit name")
	// ErrInvalidExec: the ExecStart path is invalid (empty / not absolute / contains control characters)
	ErrInvalidExec = errors.New("systemd: invalid exec path")
	// ErrInvalidEnv: an environment entry is invalid (no '=', invalid key, contains control characters)
	ErrInvalidEnv = errors.New("systemd: invalid environment entry")
	// ErrInvalidWorkingDir: WorkingDirectory is invalid (empty / not absolute)
	ErrInvalidWorkingDir = errors.New("systemd: invalid working directory")
)

// Runtime corresponds to pkgregistry.Runtime; the systemd subpackage doesn't import pkgregistry (to avoid
// an inverted dependency) and uses its own small enum, translated by authority
type Runtime uint8

const (
	RuntimeNative Runtime = iota
	RuntimeJVM
)

// Limits are the resource limits passed to systemd. A zero value means the item isn't set
type Limits struct {
	MemoryMaxBytes uint64
	// CPUQuotaPercent is the CPU quota percentage (100 = one core). Converted to CPUQuotaPerSecUSec
	CPUQuotaPercent uint32
	TasksMax        uint64
}

// Sandbox is the process's isolation profile. Fields map directly to systemd properties;
// authority fills it in by trust/component type and passes it in, and the systemd subpackage only translates it into D-Bus properties
type Sandbox struct {
	// ReadWritePaths must list the component's writable directories under ProtectSystem=strict: strict
	// makes the whole filesystem read-only, and without explicitly allowing it, not even its own private data directory is writable
	ReadWritePaths []string
	// ReadOnlyPaths / InaccessiblePaths make other Packages' directories unreadable/unwritable
	ReadOnlyPaths     []string
	InaccessiblePaths []string

	// AllowDeviceAccess opens the unit's access to host device nodes: PrivateDevices off,
	// and DevicePolicy relaxed from closed to auto.
	//
	// Why this opening is needed: the default PrivateDevices=true gives the process a private /dev containing only
	// null/zero/full/random/urandom/tty, and DevicePolicy=closed tightens the
	// cgroup device allowlist to the same set. Together, whatever UID the process runs as,
	// it can't see /dev/ttyUSB*, CAN, SPI or i2c, which means system services driving robot hardware
	// can't do anything in the default sandbox. Escalating to root doesn't get round it:
	// it's a mount namespace and cgroup device controller restriction, not a DAC permission problem.
	//
	// It should only be opened for Providers built into the system image (see service.buildStartReq).
	// Dynamically installed packages never get it: that would hand device nodes to arbitrary third-party packages.
	//
	// The other hard sandbox items (NoNewPrivileges / ProtectSystem=strict / SystemCallFilter /
	// RestrictAddressFamilies...) aren't affected by this switch and still apply unconditionally.
	AllowDeviceAccess bool

	// BindReadOnlyPaths bind-mounts host paths into the unit's mount namespace (read-only).
	//
	// The direct reason it exists is graphical UIs: PrivateTmp=true gives the component a private /tmp, while X11
	// clients connect to the display server through the unix socket /tmp/.X11-unix/X<n>.
	// The private /tmp doesn't have that directory, so every GUI component fails to start with
	// "Can't connect to X11 window server", an error that doesn't point at the sandbox at all.
	//
	// Bind mounts take effect after PrivateTmp, so the host's X11 socket directory can be brought back into
	// the private /tmp while keeping the property "components' /tmp are isolated from each other",
	// far more precise than just turning PrivateTmp off.
	//
	// Read-only, and ignore_enoent: on a headless start (no X server) the path doesn't exist,
	// and the component should start normally and then fail by itself, rather than the unit not even starting.
	// The latter errors at the systemd layer, where troubleshooting can't see which component wanted a GUI.
	BindReadOnlyPaths []string

	// AmbientCapabilities are the Linux capability names granted to the unit.
	//
	// Why Ambient rather than Bounding: the process runs as a non-root User=,
	// and the ordinary permitted/effective sets are cleared after execve (non-root, and the binary has no
	// file capability). The ambient set is the only way to survive execve and take effect under a non-root identity;
	// systemd raises them before dropping privileges.
	//
	// CapabilityBoundingSet is sent too: the bounding set is the upper limit of ambient, and without it
	// systemd's default bounding set may not include the cap being granted, so the ambient raise fails
	// while the unit starts normally, just without that capability. The symptom is EPERM at run time, as if it was never configured.
	AmbientCapabilities []string

	// ExtraAddressFamilies are socket address families allowed on top of the baseline.
	//
	// The baseline (AF_UNIX/AF_INET/AF_INET6) is given to every component unconditionally; see BuildProperties.
	// Bluetooth's AF_BLUETOOTH and the CAN bus's AF_CAN come through here.
	ExtraAddressFamilies []string
}

// UnitSpec is the complete input of one StartTransientUnit
type UnitSpec struct {
	Name        string   // nervus-<pkgid>-<compid>.service
	Description string   // human-readable, only for systemd status display
	ExecPath    string   // absolute path: native = the ELF in the package; jvm = /usr/lib/nervus/jre/bin/java
	Args        []string // ExecStart arguments (excluding argv[0])
	UID         uint32
	GID         uint32
	WorkingDir  string // /var/lib/nervus/package-data/<id>
	Env         []string
	Limits      Limits
	Sandbox     Sandbox
	// When BindToUnit is non-empty, the transient unit gets BindsTo/After=<that unit>: once the bound unit is
	// inactive/failed (including nervud.service going failed after nervud is SIGKILLed), systemd
	// stops this unit with it, so components never keep running held by systemd after nervud dies. Set to
	// nervud.service in production; empty in tests (to avoid binding a unit that doesn't exist and failing to start)
	BindToUnit string
}

// property is one StartTransientUnit property (the D-Bus struct (sv))
type property struct {
	Name  string
	Value dbus.Variant
}

// execStartItem matches ExecStart's D-Bus type a(sasb): the path, argv (including argv[0]),
// uncleanIsFailure. By convention argv[0] is ExecPath itself
type execStartItem struct {
	Path             string
	Argv             []string
	UncleanIsFailure bool
}

// restrictSet matches the D-Bus type of SystemCallFilter / RestrictAddressFamilies (bas):
// an allowlist flag + a list of names
type restrictSet struct {
	Whitelist bool
	Values    []string
}

// bindPath matches the D-Bus type of BindPaths / BindReadOnlyPaths, a(ssbt):
// source path, target path, whether to ignore a missing path, mount flags.
//
// MountFlags is 0 (equivalent to MS_NONE, non-recursive). There are no nested mounts under the X11 socket directory,
// so MS_REC isn't needed; 0 is least privilege, avoiding dragging in whatever happens to be mounted under that host path.
type bindPath struct {
	Source       string
	Destination  string
	IgnoreENOENT bool
	MountFlags   uint64
}

// validateSpec does the local allowlist checks before calling D-Bus (callers can't
// pass arbitrary systemd properties). Properties may only be built when it returns nil
func validateSpec(spec UnitSpec) error {
	if !validUnitName(spec.Name) {
		return fmt.Errorf("%w: %q", ErrInvalidUnitName, spec.Name)
	}
	if !isAbsClean(spec.ExecPath) {
		return fmt.Errorf("%w: %q", ErrInvalidExec, spec.ExecPath)
	}
	if !isAbsClean(spec.WorkingDir) {
		return fmt.Errorf("%w: %q", ErrInvalidWorkingDir, spec.WorkingDir)
	}
	for _, a := range spec.Args {
		if strings.ContainsAny(a, "\x00\n") {
			return fmt.Errorf("%w: arg contains control char", ErrInvalidExec)
		}
	}
	for _, e := range spec.Env {
		if !validEnvEntry(e) {
			return fmt.Errorf("%w: %q", ErrInvalidEnv, e)
		}
	}
	return nil
}

// BuildProperties translates a UnitSpec into StartTransientUnit's property array.
//
// The property set is fixed: callers can only influence exec/uid/limits/sandbox paths through UnitSpec's restricted fields,
// never inject arbitrary systemd properties. The hard sandbox items (NoNewPrivileges/ProtectSystem=strict/
// PrivateDevices/...) are always added, with no way to relax them
func BuildProperties(spec UnitSpec) ([]property, error) {
	if err := validateSpec(spec); err != nil {
		return nil, err
	}

	argv := append([]string{spec.ExecPath}, spec.Args...)
	props := []property{
		{"Description", dbus.MakeVariant(spec.Description)},

		// CollectMode=inactive-or-failed: after the process exits, systemd garbage-collects this
		// transient unit automatically, leaving nothing behind.
		//
		// Without it, any component that crashed once could never start again. By default systemd keeps an exited
		// transient unit in the inactive/failed state; when the supervisor restarts with backoff it calls StartTransientUnit again with the same
		// unit name, and systemd reports
		// "Unit... was already loaded or has a fragment file" and fails.
		// That would defeat the whole backoff-restart and circuit-breaker mechanism: the first crash would be the last.
		//
		// This isn't theoretical: in end-to-end testing pkgmanagerd exited two seconds after starting, and the restart hit this
		// straight away.
		//
		// Why CollectMode rather than calling ResetFailedUnit before each start:
		// the latter is a "clear the leftovers, then start" patch with a window between the two steps, and it only covers failed,
		// not the inactive left by a normal exit. CollectMode makes collection a property of the unit itself,
		// handled by systemd the moment it exits.
		{"CollectMode", dbus.MakeVariant("inactive-or-failed")},
		{"ExecStart", dbus.MakeVariant([]execStartItem{{Path: spec.ExecPath, Argv: argv, UncleanIsFailure: false}})},
		// Numeric UID/GID are passed to systemd as strings (User/Group accept numeric strings)
		{"User", dbus.MakeVariant(fmt.Sprintf("%d", spec.UID))},
		{"Group", dbus.MakeVariant(fmt.Sprintf("%d", spec.GID))},
		{"WorkingDirectory", dbus.MakeVariant(spec.WorkingDir)},

		// ---- Generic hard sandbox items (unconditional) ----
		{"NoNewPrivileges", dbus.MakeVariant(true)},
		{"ProtectSystem", dbus.MakeVariant("strict")},
		{"ProtectHome", dbus.MakeVariant("yes")},
		{"PrivateTmp", dbus.MakeVariant(true)},
		{"ProtectKernelTunables", dbus.MakeVariant(true)},
		{"ProtectKernelModules", dbus.MakeVariant(true)},
		{"RestrictSUIDSGID", dbus.MakeVariant(true)},
		{"RestrictRealtime", dbus.MakeVariant(true)},
		// SystemCallFilter=@system-service (allowlist) already excludes @mount/@module/@raw-io/
		// @privileged/@debug
		{"SystemCallFilter", dbus.MakeVariant(restrictSet{Whitelist: true, Values: []string{"@system-service"}})},
	}

	// Address families: the UNIX/INET/INET6 baseline blocks raw/packet sockets and the like; components can add more as their manifest
	// declares (Bluetooth's AF_BLUETOOTH, CAN's AF_CAN).
	//
	// This wall has nothing to do with capabilities: it's at the seccomp layer, and socket(AF_BLUETOOTH,...)
	// not on the allowlist gives EAFNOSUPPORT, even if the process has CAP_NET_ADMIN.
	families := append([]string{"AF_UNIX", "AF_INET", "AF_INET6"}, spec.Sandbox.ExtraAddressFamilies...)
	props = append(props, property{
		"RestrictAddressFamilies",
		dbus.MakeVariant(restrictSet{Whitelist: true, Values: families}),
	})

	// Capabilities: ambient + bounding are sent together.
	//
	// They must be uint64 bitmasks, not string arrays. Writing
	// "AmbientCapabilities=CAP_NET_ADMIN" in a unit file is systemd configuration sugar; but
	// StartTransientUnit goes over D-Bus, where both properties have signature t (uint64).
	// Sending a string array makes systemd refuse the whole thing with "Failed to set unit properties: Unexpected
	// message contents", and the error doesn't say which property, so when troubleshooting you can't tell
	// a type problem from an invalid value.
	//
	// Only sent when non-empty: with an empty set systemd keeps its default behaviour for a non-root User= (no capabilities);
	// explicitly sending a 0 mask would be written as a unit property, making systemctl show's output
	// impossible to tell from "nothing configured".
	if len(spec.Sandbox.AmbientCapabilities) > 0 {
		mask, err := capMask(spec.Sandbox.AmbientCapabilities)
		if err != nil {
			return nil, err
		}
		props = append(props,
			// Bounding must be given too: it's ambient's upper limit, and without it the ambient raise
			// fails silently, the unit starts normally but without the capability. The symptom is EPERM at run time,
			// as if it was never configured
			property{"CapabilityBoundingSet", dbus.MakeVariant(mask)},
			property{"AmbientCapabilities", dbus.MakeVariant(mask)},
		)
	}

	// Device access: by default (AllowDeviceAccess=false) the original two hard items stay unchanged;
	// when opened, PrivateDevices is off + DevicePolicy=auto, so the Provider can see the host's
	// /dev and access real device nodes by DAC permissions. Both properties are sent explicitly in either branch,
	// without relying on systemd defaults: transient unit defaults change between versions, and only writing them is predictable.
	if spec.Sandbox.AllowDeviceAccess {
		props = append(props,
			property{"PrivateDevices", dbus.MakeVariant(false)},
			property{"DevicePolicy", dbus.MakeVariant("auto")},
		)
	} else {
		props = append(props,
			property{"PrivateDevices", dbus.MakeVariant(true)},
			property{"DevicePolicy", dbus.MakeVariant("closed")},
		)
	}

	if spec.BindToUnit != "" {
		// BindsTo: when the bound unit stops/fails, this unit stops with it (owner-death semantics).
		// After: make sure this unit starts after the bound unit, so the binding is meaningful
		props = append(props, property{"BindsTo", dbus.MakeVariant([]string{spec.BindToUnit})})
		props = append(props, property{"After", dbus.MakeVariant([]string{spec.BindToUnit})})
	}
	if len(spec.Env) > 0 {
		props = append(props, property{"Environment", dbus.MakeVariant(spec.Env)})
	}
	if len(spec.Sandbox.ReadWritePaths) > 0 {
		props = append(props, property{"ReadWritePaths", dbus.MakeVariant(spec.Sandbox.ReadWritePaths)})
	}
	if len(spec.Sandbox.ReadOnlyPaths) > 0 {
		props = append(props, property{"ReadOnlyPaths", dbus.MakeVariant(spec.Sandbox.ReadOnlyPaths)})
	}
	if len(spec.Sandbox.InaccessiblePaths) > 0 {
		props = append(props, property{"InaccessiblePaths", dbus.MakeVariant(spec.Sandbox.InaccessiblePaths)})
	}
	if len(spec.Sandbox.BindReadOnlyPaths) > 0 {
		binds := make([]bindPath, 0, len(spec.Sandbox.BindReadOnlyPaths))
		for _, p := range spec.Sandbox.BindReadOnlyPaths {
			// Same source and target path: the aim is "bring this host path into the namespace unchanged",
			// not change where the component sees it. Moving it would only make the absolute paths in the component
			//  (X11's /tmp/.X11-unix is hard-coded in the client libraries) not line up
			binds = append(binds, bindPath{
				Source: p, Destination: p, IgnoreENOENT: true, MountFlags: 0,
			})
		}
		props = append(props, property{"BindReadOnlyPaths", dbus.MakeVariant(binds)})
	}

	// ---- Resource limits (zero means not set) ----
	if spec.Limits.MemoryMaxBytes > 0 {
		props = append(props, property{"MemoryMax", dbus.MakeVariant(spec.Limits.MemoryMaxBytes)})
	}
	if spec.Limits.TasksMax > 0 {
		props = append(props, property{"TasksMax", dbus.MakeVariant(spec.Limits.TasksMax)})
	}
	if spec.Limits.CPUQuotaPercent > 0 {
		// CPUQuotaPerSecUSec: CPU microseconds available per second. 100% = 1 second = 1_000_000us
		usec := uint64(spec.Limits.CPUQuotaPercent) * 10_000
		props = append(props, property{"CPUQuotaPerSecUSec", dbus.MakeVariant(usec)})
	}

	return props, nil
}

// ---- Capability names -> bitmask -------------------------------------------

// ErrUnknownCapability: the capability name isn't known and can't be translated into a bit position
var ErrUnknownCapability = errors.New("systemd: unknown capability name")

// capBits maps capability names -> bit positions. The positions are the CAP_* constant values in linux/capability.h,
// matching the CapEff/CapAmb bitmasks in /proc/<pid>/status one to one.
//
// Why this package keeps its own copy rather than getting it from pkgregistry: the systemd subpackage doesn't import
// pkgregistry (that would invert the dependency; the Runtime enum is handled the same way). Both tables are copies of
// capabilities(7), and TestCapMask compares them entry by entry to catch copying mistakes.
var capBits = map[string]uint{
	"CAP_CHOWN": 0, "CAP_DAC_OVERRIDE": 1, "CAP_DAC_READ_SEARCH": 2, "CAP_FOWNER": 3,
	"CAP_FSETID": 4, "CAP_KILL": 5, "CAP_SETGID": 6, "CAP_SETUID": 7,
	"CAP_SETPCAP": 8, "CAP_LINUX_IMMUTABLE": 9, "CAP_NET_BIND_SERVICE": 10,
	"CAP_NET_BROADCAST": 11, "CAP_NET_ADMIN": 12, "CAP_NET_RAW": 13,
	"CAP_IPC_LOCK": 14, "CAP_IPC_OWNER": 15, "CAP_SYS_MODULE": 16, "CAP_SYS_RAWIO": 17,
	"CAP_SYS_CHROOT": 18, "CAP_SYS_PTRACE": 19, "CAP_SYS_PACCT": 20, "CAP_SYS_ADMIN": 21,
	"CAP_SYS_BOOT": 22, "CAP_SYS_NICE": 23, "CAP_SYS_RESOURCE": 24, "CAP_SYS_TIME": 25,
	"CAP_SYS_TTY_CONFIG": 26, "CAP_MKNOD": 27, "CAP_LEASE": 28, "CAP_AUDIT_WRITE": 29,
	"CAP_AUDIT_CONTROL": 30, "CAP_SETFCAP": 31, "CAP_MAC_OVERRIDE": 32, "CAP_MAC_ADMIN": 33,
	"CAP_SYSLOG": 34, "CAP_WAKE_ALARM": 35, "CAP_BLOCK_SUSPEND": 36, "CAP_AUDIT_READ": 37,
	"CAP_PERFMON": 38, "CAP_BPF": 39, "CAP_CHECKPOINT_RESTORE": 40,
}

// capMask translates a list of capability names into the uint64 bitmask systemd's D-Bus wants.
//
// An unknown name fails the whole thing rather than being skipped: if skipped, the component would start normally and simply lack that capability,
// with EPERM at run time and no clue in the log that one was missing.
func capMask(names []string) (uint64, error) {
	var mask uint64
	for _, n := range names {
		bit, ok := capBits[n]
		if !ok {
			return 0, fmt.Errorf("%w: %q", ErrUnknownCapability, n)
		}
		mask |= 1 << bit
	}
	return mask, nil
}

// ---- Local allowlist checks ------------------------------------------------------

// validUnitName reports whether name is a safe transient unit name: it must have the nervus- prefix +
// .service suffix, only [a-z0-9._-] in between, no slashes or path separators, and a bounded length.
//
// The prefix is locked to nervus- to keep transient units started by nervud in a separate namespace from other system units,
// so StopUnit can never stop a critical system unit by mistake
func validUnitName(name string) bool {
	const prefix = "nervus-"
	const suffix = ".service"
	if len(name) < len(prefix)+len(suffix)+1 || len(name) > 255 {
		return false
	}
	if !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, suffix) {
		return false
	}
	mid := name[:len(name)-len(suffix)] // strip .service, prefix included
	for i := 0; i < len(mid); i++ {
		c := mid[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '_', c == '-', c == '.':
			// ok
		default:
			return false
		}
	}
	return true
}

// isAbsClean reports whether p is an absolute Linux path with no control characters
func isAbsClean(p string) bool {
	if p == "" || !strings.HasPrefix(p, "/") {
		return false
	}
	return !strings.ContainsAny(p, "\x00\n")
}

// validEnvEntry reports whether e is a safe "KEY=VALUE": it contains '=', the key is [A-Za-z_][A-Za-z0-9_]*,
// and the whole entry has no NUL/newline (preventing injection into the systemd unit)
func validEnvEntry(e string) bool {
	if strings.ContainsAny(e, "\x00\n") {
		return false
	}
	i := strings.IndexByte(e, '=')
	if i <= 0 {
		return false
	}
	key := e[:i]
	for j := 0; j < len(key); j++ {
		c := key[j]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c == '_':
			// ok (at either end)
		case j > 0 && c >= '0' && c <= '9':
			// ok (a digit can't be the first character)
		default:
			return false
		}
	}
	return true
}
