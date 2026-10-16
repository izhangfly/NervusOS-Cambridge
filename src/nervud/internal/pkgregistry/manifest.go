// This file is manifest.json's data model and structural validation.
// It only does pure "is the shape legal" checks, without touching the filesystem or re-checking signatures/digests
// (signatures are in signature.go/sigblock.go, digests in digest.go).
//
// Security boundary: package_id / component.id are used as UID allocation keys,
// directory names, permission ownership keys and signing continuity keys, and package_id is also joined into bookkeeping file names
// (scan.go's stateFilePath). So both get strict character set checks here, plugging holes such as path-write escapes
// at the parsing entry point rather than hoping downstream always checks
package pkgregistry

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"strings"
)

// manifestSchemaV1 is the only manifest schema version this version knows
const manifestSchemaV1 = 1

// CurrentAPILevel is the Platform API Level this kernel implements.
//
// A manifest whose min_nervus_api is higher can't run on this machine. The first of the two parsing passes uses
// it to give ErrPlatformTooOld first, instead of letting a new-schema manifest hit
// DisallowUnknownFields and report a vague unknown field.
//
// # What each Level means
//
// This number must follow "the platform's external capabilities changed in a backward-incompatible way", not version numbers
// or the release cadence. The two levels defined so far:
//
//	1: the initial platform surface. IPC protocol major 1, no event subscriptions, no shared-memory transfer
//	2: IPC protocol major 2 (old implicit defaults removed, not backward compatible) + event subscriptions and event instance
//	   ownership + shared-memory ring transfer + Operation long tasks + the resource directory
//
// Packages declaring min_nervus_api=1 still install on a Level 2 kernel (the criterion is
// MinNervusAPI > CurrentAPILevel), so raising this value doesn't drive old packages away.
//
// This once stayed at 1 for a long time after the platform had already made every Level 2 change above. The result:
// services like camerad that honestly declared min_nervus_api=2 were blocked on the target machine by ErrPlatformTooOld,
// with only an unhelpful "platform api level too old" left in the audit.
// Review this value whenever the platform's external capabilities change.
const CurrentAPILevel uint32 = 2

// The canonical ABI tokens of NSOS Packages. These are NSOS package format identifiers,
// not Android NDK names (arm64-v8a and so on), nor bare CPU names (aarch64 and so on); the latter two
// are always refused, with no normalisation
const (
	ABILinuxArm64  = "linux-arm64"
	ABILinuxArmv7  = "linux-armv7"
	ABILinuxX86_64 = "linux-x86_64"
)

func validABIToken(s string) bool {
	switch s {
	case ABILinuxArm64, ABILinuxArmv7, ABILinuxX86_64:
		return true
	default:
		return false
	}
}

var (
	// ErrEmptyPackageID: the manifest lacks a Package ID
	ErrEmptyPackageID = errors.New("pkgregistry: manifest has empty package id")

	// ErrInvalidPackageID: the Package ID has illegal characters or shape (see validPackageID).
	// This isn't just a naming convention: package_id is joined into bookkeeping file names, so illegal characters amount to a path-write escape
	ErrInvalidPackageID = errors.New("pkgregistry: manifest has invalid package id")

	// ErrInvalidComponentID: the Component ID has illegal characters or shape (see validComponentID)
	ErrInvalidComponentID = errors.New("pkgregistry: manifest has invalid component id")

	// ErrEmptyVersion: the manifest lacks a version
	ErrEmptyVersion = errors.New("pkgregistry: manifest has empty version")

	// ErrInvalidVersion: version has illegal characters or shape (see validVersion). version is
	// joined into the code directory name <PackageRoot>/<id>/<version> (install.go), so illegal characters would let
	// a hostile manifest use version="../com.victim/2.0" to write code into another Package's namespace
	ErrInvalidVersion = errors.New("pkgregistry: manifest has invalid version")

	// ErrMissingVersionCode: the manifest lacks version_code (or it's 0). Upgrade decisions only look at
	// version_code, and without it there's no downgrade protection
	ErrMissingVersionCode = errors.New("pkgregistry: manifest has zero/absent version_code")

	// ErrUnsupportedSchema: the manifest declares a schema version this version doesn't know
	ErrUnsupportedSchema = errors.New("pkgregistry: unsupported manifest schema")

	// ErrPlatformTooOld: the manifest's min_nervus_api is higher than this machine's Platform API Level
	ErrPlatformTooOld = errors.New("pkgregistry: platform api level too old for this package")

	// ErrInvalidABI: supported_abis is empty, or contains unknown or non-canonical tokens (including Android NDK names)
	ErrInvalidABI = errors.New("pkgregistry: manifest has empty or invalid supported_abis")

	// ErrNoComponents: a Package must declare at least one App or Service
	ErrNoComponents = errors.New("pkgregistry: manifest declares no components")

	// ErrNoDigests: the manifest must carry a file digest list, otherwise integrity re-checks are impossible
	ErrNoDigests = errors.New("pkgregistry: manifest has no file digests")

	// ErrDuplicateComponentID: two Components in the same manifest share an ID
	ErrDuplicateComponentID = errors.New("pkgregistry: duplicate component id in manifest")

	// ErrInvalidComponentType: Component.Type can only be app or service
	ErrInvalidComponentType = errors.New("pkgregistry: component type must be app or service")

	// ErrInvalidRuntime: Component.Runtime can only be native or jvm
	ErrInvalidRuntime = errors.New("pkgregistry: component runtime must be native or jvm")

	// ErrInvalidLaunchMode: Component.LaunchMode has an illegal value
	ErrInvalidLaunchMode = errors.New("pkgregistry: component has invalid launch_mode")

	// ErrInvalidCapability: Component.Capabilities has an unknown capability name.
	// See privilege.go: the allowlist stops typos and injection, not policy filtering
	ErrInvalidCapability = errors.New("pkgregistry: component requests an unknown capability")

	// ErrInvalidAddressFamily: Component.AddressFamilies has an unknown address family name
	ErrInvalidAddressFamily = errors.New("pkgregistry: component requests an unknown address family")

	// ErrLaunchModeTypeMismatch: launch_mode conflicts with type: an app can't be always-on,
	// and a service can't be manual
	ErrLaunchModeTypeMismatch = errors.New("pkgregistry: launch_mode incompatible with component type")

	// ErrInvalidCriticality: Component.Criticality has an illegal value
	ErrInvalidCriticality = errors.New("pkgregistry: component has invalid criticality")

	// ErrInvalidVisibility: Export.Visibility has an illegal value
	ErrInvalidVisibility = errors.New("pkgregistry: export has invalid visibility")

	// ErrEntryNotInDigests: Component.Entry isn't covered by the digest list. The entry file must
	// be covered by signature/integrity re-checks, otherwise it amounts to allowing an unverified executable to run
	ErrEntryNotInDigests = errors.New("pkgregistry: component entry not covered by digests")

	// ErrIconNotInDigests: manifest.icon isn't covered by the digest list
	ErrIconNotInDigests = errors.New("pkgregistry: icon not covered by digests")

	// ErrInvalidProviderArtifacts: the Provider descriptor/schema references are incomplete or unsafe,
	// point at the same file, or aren't covered by the manifest digests.
	ErrInvalidProviderArtifacts = errors.New("pkgregistry: invalid provider artifacts")

	// ErrUnsafeRelPath: a field that should be a relative path inside the package escapes the package directory after resolution,
	// or is an absolute path itself
	ErrUnsafeRelPath = errors.New("pkgregistry: path escapes package directory")
)

// ComponentType is a Component's run shape
type ComponentType string

const (
	ComponentApp     ComponentType = "app"
	ComponentService ComponentType = "service"
)

// CanProvideInterfaces says whether this component shape may export interfaces (register endpoints,
// answer Dispatches, register event scopes, report events).
//
// # Why apps can too
//
// It used to be services only, and that made "a component with a UI that other packages can invoke by interface" impossible:
//
//	apps are the only shape that gets X11 (the kernel binds /tmp/.X11-unix and injects DISPLAY
//	only for apps; see service/supervise.go), so any component with a window had to be an app;
//	and registering endpoints used to require a service, so it couldn't be invoked by interface.
//
// Together the two constraints ruled out a whole class of legitimate needs: the permission confirmation screen must be invokable by Settings and return
// a result (did the user agree or refuse), and the install confirmation screen likewise. This "open a UI and wait for an answer"
// is exactly what Android expresses with Activity + Intent, and those Activities have UIs too.
//
// # What didn't change when it was opened up
//
// Not one authorisation criterion changed: the manifest must still declare exports, the Catalog must still accept this component
// as a Provider of that interface major, perm.service.register(.private) is still required,
// and the schema hash must still equal the Catalog's byte for byte. The only change is "which shape may take this path".
//
// # Lifecycle differences are covered by EndpointDied
//
// An app's process lives shorter than a service's, since the user can close the window. But that's not a new problem: services
// crash and get disabled too, and the kernel already has to revoke a connection's endpoints when it drops and send callers
// EndpointDied. Apps just take that path more often.
func (t ComponentType) CanProvideInterfaces() bool {
	return t == ComponentService || t == ComponentApp
}

// Runtime is how a Component's process entry is started.
//
// Note: runtime describes who starts the process entry, not whether there can be native code; both runtimes
// can carry lib/<abi>/*.so and load it through JNI/Panama or direct linking
type Runtime string

const (
	RuntimeNative Runtime = "native"
	RuntimeJVM    Runtime = "jvm"
)

func (r Runtime) valid() bool { return r == RuntimeNative || r == RuntimeJVM }

// LaunchMode is a Component's launch mode
type LaunchMode string

const (
	// LaunchAlwaysOn: started once the kernel is ready, restarted with backoff after crashes (services only)
	LaunchAlwaysOn LaunchMode = "always-on"
	// LaunchOnDemand: started when its endpoint is Resolved, stopped after an idle timeout
	LaunchOnDemand LaunchMode = "on-demand"
	// LaunchManual: only on explicit request (the Launcher starting an app)
	LaunchManual LaunchMode = "manual"
)

func (l LaunchMode) valid() bool {
	return l == LaunchAlwaysOn || l == LaunchOnDemand || l == LaunchManual
}

// Criticality is a Component's importance level.
//
// Ordered strings rather than a separate uint8 enum kept in internal/service: the manifest is
// the source of truth for criticality, and two copies would drift. service's escalation chain compares with Rank
type Criticality string

const (
	CriticalityOptional Criticality = "optional"
	CriticalityRequired Criticality = "required"
	CriticalityVital    Criticality = "vital"
)

func (c Criticality) valid() bool {
	return c == CriticalityOptional || c == CriticalityRequired || c == CriticalityVital
}

// Rank returns criticality's ordered level, for service's escalation chain to compare (optional<required<vital)
func (c Criticality) Rank() int {
	switch c {
	case CriticalityVital:
		return 2
	case CriticalityRequired:
		return 1
	default:
		return 0
	}
}

// Visibility is an Export's visibility scope
type Visibility string

const (
	// VisibilityPackage: only other Components in the same Package can Resolve it (needs perm.service.register.private)
	VisibilityPackage Visibility = "package"
	// VisibilityPublic: visible across Packages (needs perm.service.register, OEM+)
	VisibilityPublic Visibility = "public"
)

func (v Visibility) valid() bool { return v == VisibilityPackage || v == VisibilityPublic }

// Export is an Interface a Component provides outward
type Export struct {
	Interface  string     `json:"interface"`
	Visibility Visibility `json:"visibility"`
}

// Feature is a robot Feature the manifest declares it consumes
type Feature struct {
	ID       string `json:"id"`
	Required bool   `json:"required"`
}

// RuntimeDeps is the runtime dependency declaration
type RuntimeDeps struct {
	MinJavaRelease int `json:"min_java_release,omitempty"`
}

// ProviderArtifactsRef points at the data-driven Provider contract inside the signed package.
//
// Both files must be covered by Digests; RegisterEndpoint at run time can't submit or replace them.
// Descriptor is the deterministic protobuf bytes of the ProviderDescriptor, and Schemas the
// deterministic protobuf bytes of the InterfaceSchemaBundleSet.
type ProviderArtifactsRef struct {
	Descriptor string `json:"descriptor"`
	Schemas    string `json:"schemas"`
}

// ComponentLimits are the resource limits passed to systemd.
//
// The kernel clamps these limits by trust, so Ordinary packages can't give themselves unlimited quotas; the clamping
// lives in internal/service, and this only carries the shape
type ComponentLimits struct {
	MemoryMaxMB     uint64 `json:"memory_max_mb,omitempty"`
	CPUQuotaPercent uint32 `json:"cpu_quota_percent,omitempty"`
	TasksMax        uint32 `json:"tasks_max,omitempty"`
}

// Component is one registrable run unit declared in the manifest
type Component struct {
	ID           string          `json:"id"`
	Type         ComponentType   `json:"type"`
	Runtime      Runtime         `json:"runtime"`
	Entry        string          `json:"entry"`                      // in-package relative entry path
	NativeLibDir string          `json:"native_lib_dir,omitempty"`   // in-package relative directory
	LaunchMode   LaunchMode      `json:"launch_mode"`                //
	Criticality  Criticality     `json:"criticality,omitempty"`      // defaults to optional
	Disableable  bool            `json:"disableable,omitempty"`      // the effective value is Entry.CanDisable
	Exports      []Export        `json:"exports,omitempty"`          //
	Interfaces   []string        `json:"interfaces,omitempty"`       // interface IDs it asks to consume
	IdleTimeout  int             `json:"idle_timeout_sec,omitempty"` // only meaningful for on-demand
	Limits       ComponentLimits `json:"limits,omitempty"`

	// Privileged is the master switch for "give this component everything the sandbox can give": it expands to every
	// capability + every extra address family + device node access.
	//
	// It exists because what it replaces is worse. Without it, a system service that drives hardware
	// would have to list every capability it needs one by one; missing one shows up as some operation getting
	// EPERM at run time, with an error that doesn't say which capability is missing. So in practice people copy
	// a longer list, and in the end every package carries a list nobody understands. An honest
	// "privileged": true at least lets reviewers see at a glance that this package isn't constrained by the sandbox.
	//
	// It isn't root: the process still runs as an App UID, with SystemCallFilter,
	// ProtectSystem=strict and its own data directory isolation. But a process with CAP_SETUID
	// can turn itself into root, so the practical effect is close. Judge it as "this package is fully trusted",
	// not as "a little more permission".
	//
	// The same rule as Capabilities/AddressFamilies: only packages from the system image
	// get it. Dynamically installed packages that fill it in are ignored.
	Privileged bool `json:"privileged,omitempty"`

	// Capabilities are the Linux capabilities this component requests, such as "CAP_NET_ADMIN".
	//
	// It needn't be filled in when Privileged is true (the full set overrides it). Fill it in for precise control,
	// which is better than Privileged, but make sure the list is complete.
	//
	// Only packages from the system image get them. Dynamically installed packages that fill it in are ignored by the service layer,
	// the same rule as AllowDeviceAccess: handing capabilities to an arbitrary third-party package
	// means the sandbox doesn't exist.
	//
	// Why this opening is needed: components run as App UIDs (20000-59999), not root,
	// and the sandbox grants no capabilities. Operations like driving radios (Bluetooth rfkill, hci up) or configuring networks
	// therefore all get EPERM. And running components as root isn't an option: the ipc handshake
	// refuses root peers with CheckUID (internal/ipc/ipc.go), so a component running as root couldn't even reach
	// the control plane. Capabilities are the only way that doesn't break the identity model.
	Capabilities []string `json:"capabilities,omitempty"`

	// AddressFamilies are the extra socket address families this component asks to be allowed, such as "AF_BLUETOOTH".
	//
	// It's separate from Capabilities because they're two unrelated walls:
	// RestrictAddressFamilies is at the seccomp layer, and no amount of capabilities gets round it;
	// socket(AF_BLUETOOTH,...) gets EAFNOSUPPORT straight away. With capabilities but no
	// address family, Bluetooth still won't open, and the error looks like "protocol not supported", nothing to do with permissions.
	//
	// Likewise only packages from the system image get them.
	AddressFamilies []string `json:"address_families,omitempty"`
}

// Manifest is the parsed result of manifest.json.
//
// Signer is deliberately not a JSON field: it comes from independent verification of the detached signature (see signature.go),
// so a manifest can't prove its own identity by putting a signer string in its JSON
type Manifest struct {
	Schema          int                   `json:"schema"`
	PackageID       string                `json:"package_id"`
	Label           string                `json:"label"`
	Labels          map[string]string     `json:"labels,omitempty"`
	Icon            string                `json:"icon,omitempty"`
	Version         string                `json:"version"`
	VersionCode     uint64                `json:"version_code"`
	MinNervusAPI    uint32                `json:"min_nervus_api"`
	TargetNervusAPI uint32                `json:"target_nervus_api"`
	SupportedABIs   []string              `json:"supported_abis"`
	RuntimeDeps     RuntimeDeps           `json:"runtime_deps,omitempty"`
	Permissions     []string              `json:"permissions,omitempty"`
	UsesFeatures    []Feature             `json:"uses_features,omitempty"`
	Components      []Component           `json:"components"`
	Provider        *ProviderArtifactsRef `json:"provider,omitempty"`
	Digests         map[string]string     `json:"digests"` // in-package relative path -> sha256 hex

	Signer string `json:"-"`
}

// Component looks up a component by ID, for service/disable logic
func (m Manifest) Component(id string) (Component, bool) {
	for _, c := range m.Components {
		if c.ID == id {
			return c, true
		}
	}
	return Component{}, false
}

// ParseManifest deserialises and does structural validation; any failure rejects the whole thing.
//
// Two parsing passes:
//  1. A lenient decode that looks at schema and min_nervus_api first, so more accurate, diagnosable errors like
//     "unknown schema" / "platform too old" come before unknown fields; otherwise a newer manifest
//     on an older device would only hit DisallowUnknownFields and report a vague unknown field
//  2. A strict full decode (DisallowUnknownFields): a field this version doesn't know in the manifest
//     most likely means a manifest written for a newer schema, and silently ignoring it would be pretending to understand, so it's refused
func ParseManifest(data []byte) (Manifest, error) {
	var probe struct {
		Schema       int    `json:"schema"`
		MinNervusAPI uint32 `json:"min_nervus_api"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return Manifest{}, fmt.Errorf("pkgregistry: probe manifest: %w", err)
	}
	if probe.Schema != manifestSchemaV1 {
		return Manifest{}, fmt.Errorf("%w: got %d, want %d", ErrUnsupportedSchema, probe.Schema, manifestSchemaV1)
	}
	if probe.MinNervusAPI > CurrentAPILevel {
		return Manifest{}, fmt.Errorf("%w: manifest requires api %d, platform provides %d",
			ErrPlatformTooOld, probe.MinNervusAPI, CurrentAPILevel)
	}

	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()

	var m Manifest
	if err := dec.Decode(&m); err != nil {
		return Manifest{}, fmt.Errorf("pkgregistry: decode manifest: %w", err)
	}
	if err := m.validate(); err != nil {
		return Manifest{}, err
	}
	return m, nil
}

func (m Manifest) validate() error {
	// ---- Top-level identity and structure (keeping the trigger order of existing negative tests: id/version/components/digests) ----
	if m.PackageID == "" {
		return ErrEmptyPackageID
	}
	if !validPackageID(m.PackageID) {
		return fmt.Errorf("%w: %q", ErrInvalidPackageID, m.PackageID)
	}
	if m.Version == "" {
		return ErrEmptyVersion
	}
	if !validVersion(m.Version) {
		return fmt.Errorf("%w: %q", ErrInvalidVersion, m.Version)
	}
	if len(m.Components) == 0 {
		return ErrNoComponents
	}
	if len(m.Digests) == 0 {
		return ErrNoDigests
	}

	for rel := range m.Digests {
		if !validRelPath(rel) {
			return fmt.Errorf("%w: digest path %q", ErrUnsafeRelPath, rel)
		}
	}

	// ---- Component checks happen in two passes ----
	// The first pass only checks identity structure: id validity, de-duplication, type, entry path safety and digest coverage.
	// The second pass checks field values: runtime/launch_mode/criticality/exports. They're separate so structural errors
	// like duplicate IDs / illegal types / path escapes are reported before any field value error,
	// putting the diagnosis closer to the real problem
	seenID := make(map[string]struct{}, len(m.Components))
	for _, c := range m.Components {
		if c.ID == "" {
			return fmt.Errorf("pkgregistry: component with empty id (entry %q)", c.Entry)
		}
		if !validComponentID(c.ID) {
			return fmt.Errorf("%w: %q", ErrInvalidComponentID, c.ID)
		}
		if _, dup := seenID[c.ID]; dup {
			return fmt.Errorf("%w: %q", ErrDuplicateComponentID, c.ID)
		}
		seenID[c.ID] = struct{}{}

		if c.Type != ComponentApp && c.Type != ComponentService {
			return fmt.Errorf("%w: component %q has type %q", ErrInvalidComponentType, c.ID, c.Type)
		}
		if !validRelPath(c.Entry) {
			return fmt.Errorf("%w: component %q entry %q", ErrUnsafeRelPath, c.ID, c.Entry)
		}
		if _, ok := m.Digests[c.Entry]; !ok {
			return fmt.Errorf("%w: component %q entry %q", ErrEntryNotInDigests, c.ID, c.Entry)
		}
	}
	for _, c := range m.Components {
		if !c.Runtime.valid() {
			return fmt.Errorf("%w: component %q runtime %q", ErrInvalidRuntime, c.ID, c.Runtime)
		}
		if !c.LaunchMode.valid() {
			return fmt.Errorf("%w: component %q launch_mode %q", ErrInvalidLaunchMode, c.ID, c.LaunchMode)
		}
		// apps can't be always-on; services can't be manual
		if c.Type == ComponentApp && c.LaunchMode == LaunchAlwaysOn {
			return fmt.Errorf("%w: app %q cannot be always-on", ErrLaunchModeTypeMismatch, c.ID)
		}
		if c.Type == ComponentService && c.LaunchMode == LaunchManual {
			return fmt.Errorf("%w: service %q cannot be manual", ErrLaunchModeTypeMismatch, c.ID)
		}
		if c.Criticality != "" && !c.Criticality.valid() {
			return fmt.Errorf("%w: component %q criticality %q", ErrInvalidCriticality, c.ID, c.Criticality)
		}
		for _, e := range c.Exports {
			if e.Visibility != "" && !e.Visibility.valid() {
				return fmt.Errorf("%w: component %q export %q visibility %q",
					ErrInvalidVisibility, c.ID, e.Interface, e.Visibility)
			}
		}
		if c.NativeLibDir != "" && !validRelPath(c.NativeLibDir) {
			return fmt.Errorf("%w: component %q native_lib_dir %q", ErrUnsafeRelPath, c.ID, c.NativeLibDir)
		}
		// Capability and address family names go into systemd unit properties unchanged, so they must pass the allowlist first.
		// See privilege.go
		if err := validateComponentPrivileges(c); err != nil {
			return err
		}
	}

	// ---- Required top-level new fields (after the component checks, so component structure errors surface first) ----
	if m.VersionCode == 0 {
		return ErrMissingVersionCode
	}
	if len(m.SupportedABIs) == 0 {
		return fmt.Errorf("%w: empty", ErrInvalidABI)
	}
	for _, abi := range m.SupportedABIs {
		if !validABIToken(abi) {
			return fmt.Errorf("%w: %q", ErrInvalidABI, abi)
		}
	}
	if m.Icon != "" {
		if !validRelPath(m.Icon) {
			return fmt.Errorf("%w: icon %q", ErrUnsafeRelPath, m.Icon)
		}
		if _, ok := m.Digests[m.Icon]; !ok {
			return fmt.Errorf("%w: %q", ErrIconNotInDigests, m.Icon)
		}
	}
	if m.Provider != nil {
		descriptor := m.Provider.Descriptor
		schemas := m.Provider.Schemas
		if descriptor == "" || schemas == "" || descriptor == schemas {
			return fmt.Errorf("%w: descriptor and schemas must be distinct non-empty paths",
				ErrInvalidProviderArtifacts)
		}
		for label, rel := range map[string]string{
			"descriptor": descriptor,
			"schemas":    schemas,
		} {
			if !validRelPath(rel) {
				return fmt.Errorf("%w: %s path %q: %w",
					ErrInvalidProviderArtifacts, label, rel, ErrUnsafeRelPath)
			}
			if _, ok := m.Digests[rel]; !ok {
				return fmt.Errorf("%w: %s path %q is not covered by digests",
					ErrInvalidProviderArtifacts, label, rel)
			}
		}
	}
	return nil
}

// validPackageID reports whether s is a safe Package ID: reverse-DNS style
// seg(.seg)*, each segment [a-z][a-z0-9_]*, 1..8 segments, total length <= 128.
//
// This isn't just a naming convention: package_id is joined into bookkeeping file names by scan.go's stateFilePath,
// so '.', '..', '/', '\', NUL, upper case, empty segments and overlong IDs are refused.
// Lower case only is deliberate: on case-insensitive filesystems "Foo" and "foo" point at the same file,
// planting the risk of two Packages fighting over one bookkeeping file
func validPackageID(s string) bool {
	if s == "" || len(s) > 128 {
		return false
	}
	segs := strings.Split(s, ".")
	if len(segs) < 1 || len(segs) > 8 {
		return false
	}
	for _, seg := range segs {
		if !validIDSegment(seg) {
			return false
		}
	}
	return true
}

// validComponentID reports whether s is a safe Component ID: a single segment [a-z][a-z0-9_]*, <= 64
func validComponentID(s string) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	return validIDSegment(s)
}

// validVersion reports whether s is a safe version string.
//
// version is joined into the code directory name <PackageRoot>/<id>/<version>, so it must be tightened for directory name
// safety: only [A-Za-z0-9._+-], length <= 64,
// and "."/".." and shapes containing slashes, backslashes or NUL are refused. Otherwise version="../com.victim/2.0"
// would land in another Package's namespace after filepath.Join cleans it (the same kind of hole)
func validVersion(s string) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	if s == "." || s == ".." {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z',
			c >= 'A' && c <= 'Z',
			c >= '0' && c <= '9',
			c == '.' || c == '_' || c == '+' || c == '-':
			// ok
		default:
			return false
		}
	}
	return true
}

func validIDSegment(seg string) bool {
	if seg == "" {
		return false
	}
	for i := 0; i < len(seg); i++ {
		c := seg[i]
		switch {
		case c >= 'a' && c <= 'z':
			// ok
		case i > 0 && (c >= '0' && c <= '9' || c == '_'):
			// ok (a digit or underscore can't be the first character)
		default:
			return false
		}
	}
	return true
}

// validRelPath reports whether p is a safe in-package relative path: non-empty, not absolute,
// and doesn't escape the package directory with ".." after Clean.
//
// This is a pure string check; the real escape protection comes at install commit time, when authority backs it with
// openat2(RESOLVE_BENEATH)
func validRelPath(p string) bool {
	if p == "" || path.IsAbs(p) {
		return false
	}
	cleaned := path.Clean(p)
	if cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return false
	}
	return true
}
