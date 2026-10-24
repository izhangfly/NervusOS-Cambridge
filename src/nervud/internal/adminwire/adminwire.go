// Package adminwire is the wire format and client of nervud's privileged admin channel (nervusctl <-> nervud).
//
// It's deliberately a leaf package: it depends only on the standard library and net, and imports no nervud kernel module
//
//	(pkgregistry/permission/authority...). That way cmd/nervusctl links only this small slice of
//
// code instead of dragging the whole kernel TCB into the CLI binary. The server (internal/admin) and the client
//
//	(cmd/nervusctl) share this package's Request/Response/encoding, so the two sides can never drift.
//
// This channel isn't the cross-language control plane Apps use; that one only uses nervus-ipc's frozen protos,
// to avoid two incompatible wire sources of truth. This channel is the internal boundary between nervud and the privileged Go
// ops tools in the same repository: the single writer is still nervud, and nervusctl only delivers commands.
// So it uses a length prefix + JSON (enough for Go <-> Go, low-frequency, root-only ops), without bringing in
// the proto/method_id machinery for cross-language use.
//
// Frame format: a 4-byte big-endian length N + N bytes of JSON. Same layout as internal/ipc/frame.go, but this package
// keeps its own minimal implementation rather than importing internal/ipc (which would drag kernel dependencies into the CLI).
package adminwire

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"time"
)

// Admin command identifiers. The server dispatches on Request.Cmd; unknown commands get CodeBadRequest.
const (
	// CmdBeginStaging asks nervud to create an empty directory under the staging root it controls and return the
	// absolute path. The CLI then unpacks the .nspkg into it and sends CmdInstall. Having nervud create the directory
	//  (rather than the CLI creating one anywhere) guarantees: it's on the same filesystem as PackageRoot (so the install-time
	// renameat2 can succeed), its owner/permissions are controlled, and the path-escape check at install has a clear
	// "it must be a directory I issued" criterion.
	CmdBeginStaging = "begin-staging"
	// CmdInstall triggers the install of a staged directory. Signatures/digests/decisions are all re-checked in nervud's
	// pkgregistry; the CLI makes no security decision.
	CmdInstall = "install"
	// CmdUninstall uninstalls a dynamically installed Package.
	CmdUninstall = "uninstall"
	// CmdList lists the currently installed Packages.
	CmdList = "list"
	// CmdSetEnabled disables/enables a Component.
	CmdSetEnabled = "set-enabled"
	// CmdSetPermission sets the grant state of a runtime (GrantUser) permission (grant/revoke).
	CmdSetPermission = "set-permission"
	// CmdInspect only parses a staged directory and reports the USER_CONSENT permissions it requests,
	// [installing nothing and changing no state].
	//
	// The install confirmation screen uses it: the user must see what a package wants before installing, and the package to install isn't
	// in the Catalog yet, so no existing command can look it up.
	//
	// It shares one staging directory with CmdInstall: the confirmation screen inspects, then installs, and both
	// must refer to the same tree. Staging each separately would open a gap of "you looked at A,
	// you installed B" in between, while the permission list the user saw came from A.
	CmdInspect = "inspect"
	// CmdLaunch starts an installed component (an app opened from outside NervusOS, e.g. an icon on the host
	// desktop), exactly as the launcher's LaunchComponent would. Already running is not an error.
	CmdLaunch = "launch"
)

// Wire representation of grant states. Maps one to one onto permission.GrantState, but this package doesn't import
// permission (to stay a leaf); the mapping is done by the server.
const (
	GrantStateNotRequested    = "not-requested"
	GrantStateGranted         = "granted"
	GrantStateDenied          = "denied"
	GrantStateDeniedPermanent = "denied-permanent"
)

// Wire representation of risk classes. Maps one to one onto ipcv1.RiskClass, but this package doesn't import ipcv1
// (to stay a leaf); the mapping is done by the server, the same shape as GrantState.
//
// Strings rather than numbers: the other end of this channel is a hand-copied struct in pkgmanagerd, and
// a bare number in JSON would silently become a different risk class if the enum order changed. A mistyped string gives
// an empty value, which the UI treats as "unknown risk", never showing CRITICAL_SAFETY as NORMAL.
const (
	RiskClassUnspecified      = ""
	RiskClassNormal           = "normal"
	RiskClassPrivacySensitive = "privacy-sensitive"
	RiskClassPhysicalControl  = "physical-control"
	RiskClassCriticalSafety   = "critical-safety"
)

// Machine-readable result codes. The CLI chooses its exit code/wording from them; Message is just human-readable extra detail.
const (
	CodeOK           = "ok"
	CodeBadRequest   = "bad-request"  // invalid command/arguments (including path escapes)
	CodeUnauthorized = "unauthorized" // the peer isn't a permitted ops identity
	CodeNotFound     = "not-found"    // the target Package/Component doesn't exist
	CodeFailed       = "failed"       // the underlying operation failed (install decision refused, IO error...)
)

// Request is one CLI -> nervud command. One command per connection (send a Request, receive a
// Response, then close), with no long-lived connection state machine: ops is low-frequency, and simple beats reuse.
type Request struct {
	Cmd         string `json:"cmd"`
	StagingDir  string `json:"staging_dir,omitempty"`
	PackageID   string `json:"package_id,omitempty"`
	ComponentID string `json:"component_id,omitempty"`
	Enabled     bool   `json:"enabled,omitempty"`
	Permission  string `json:"permission,omitempty"`
	GrantState  string `json:"grant_state,omitempty"`

	// ConsentedPermissions travels with install: the permissions the user agreed to on the install confirmation screen.
	//
	// The kernel only treats it as an upper bound; what's actually stored is its intersection with the install-time grant set and the USER_CONSENT
	// tier (see pkgregistry.Module.applyInstallConsent), so exaggerating it does no harm.
	// Leaving it out means "no permission was agreed to", and the install goes ahead as normal
	ConsentedPermissions []string `json:"consented_permissions,omitempty"`

	// ExpectedManifestDigest travels with install: the digest returned by the confirmation screen's inspect.
	//
	// It ties "the one the user looked at" to "the one being installed now". The .nspkg sits in the shared
	// user-data area, and a caller could swap the file between inspect and install; each call is
	// legitimate on its own, and only tying them together catches it.
	//
	// Empty = no check: install scripts and deploy never had an inspect step.
	ExpectedManifestDigest string `json:"expected_manifest_digest,omitempty"`
}

// Response is the nervud -> CLI result. When OK=false, Code/Message explain why.
type Response struct {
	OK         bool          `json:"ok"`
	Code       string        `json:"code,omitempty"`
	Message    string        `json:"message,omitempty"`
	StagingDir string        `json:"staging_dir,omitempty"` // output of begin-staging
	Package    *PackageInfo  `json:"package,omitempty"`     // output of install
	Packages   []PackageInfo `json:"packages,omitempty"`    // output of list
	Inspect    *InspectInfo  `json:"inspect,omitempty"`     // output of inspect
}

// InspectInfo is the result of a read-only inspection (the output of inspect).
//
// Deliberately separate from PackageInfo: that one describes the state of an [installed] package (trust, source, granted
// permissions), while this one describes a candidate [not yet installed]: it has no trust decision yet
// and no granted permissions. Reusing one type would mix up two kinds of empty field that mean different things.
type InspectInfo struct {
	ID          string `json:"id"`
	Version     string `json:"version"`
	VersionCode uint64 `json:"version_code"`

	// ManifestDigest is the digest of the content this inspection saw. The confirmation screen must bring it back to install unchanged,
	// or the permission list the user saw and the package actually installed may not be the same one.
	ManifestDigest string `json:"manifest_digest,omitempty"`

	// ConsentPermissions are the sensitive permissions this package requests that need the user's nod.
	// nervud has already filtered them by the Catalog definitions down to the USER_CONSENT tier.
	ConsentPermissions []ConsentPermissionInfo `json:"consent_permissions,omitempty"`
}

// ConsentPermissionInfo is one permission awaiting consent that the confirmation screen shows.
//
// The kernel takes the text from the Catalog's permission definitions rather than the UI hard-coding it: third-party packages can define
// their own permissions, and the UI can't know their names and descriptions in advance.
//
// RiskClass uses the wire string rather than a number: this package is a leaf and doesn't import ipcv1
// (same reason as GrantState); the mapping is done by the server.
type ConsentPermissionInfo struct {
	ID              string `json:"id"`
	DisplayNameZhCN string `json:"display_name_zh_cn,omitempty"`
	DisplayNameEN   string `json:"display_name_en,omitempty"`
	DescriptionZhCN string `json:"description_zh_cn,omitempty"`
	DescriptionEN   string `json:"description_en,omitempty"`
	RiskClass       string `json:"risk_class,omitempty"`
}

// PackageInfo is the public projection of an installed Package (install result / list item).
type PackageInfo struct {
	ID          string   `json:"id"`
	Version     string   `json:"version"`
	VersionCode uint64   `json:"version_code"`
	Trust       string   `json:"trust"`
	Source      string   `json:"source"`
	Granted     []string `json:"granted,omitempty"`  // granted permissions (for people to check)
	Disabled    []string `json:"disabled,omitempty"` // disabled Components
}

// MaxMessageBytes is the hard limit for one JSON message. Admin messages are all small (install carries only a path,
// a list result grows with the number of Packages), so 1 MiB is ample while still stopping malformed length prefixes.
const MaxMessageBytes = 1 << 20

const lengthPrefixBytes = 4

// ErrMessageTooLarge means the length prefix exceeds the hard limit.
var ErrMessageTooLarge = errors.New("adminwire: message exceeds hard limit")

// WriteTo encodes v as JSON and writes it as a 4-byte length + body. The server writes Responses and
// the client writes Requests through it.
func WriteTo(w io.Writer, v any) error {
	body, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("adminwire: marshal: %w", err)
	}
	if len(body) > MaxMessageBytes {
		return fmt.Errorf("%w: %d > %d", ErrMessageTooLarge, len(body), MaxMessageBytes)
	}
	var hdr [lengthPrefixBytes]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(len(body)))
	if _, err := w.Write(hdr[:]); err != nil {
		return fmt.Errorf("adminwire: write header: %w", err)
	}
	if _, err := w.Write(body); err != nil {
		return fmt.Errorf("adminwire: write body: %w", err)
	}
	return nil
}

// ReadFrom reads one complete 4-byte length + body and decodes it into v. The server reads Requests and the client reads
// Responses through it. A length over the limit is an error (no buffer is allocated for a body an attacker merely claims).
func ReadFrom(r io.Reader, v any) error {
	var hdr [lengthPrefixBytes]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return err // including io.EOF: the other end closed
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n == 0 {
		return errors.New("adminwire: zero-length message")
	}
	if n > MaxMessageBytes {
		return fmt.Errorf("%w: %d > %d", ErrMessageTooLarge, n, MaxMessageBytes)
	}
	body := make([]byte, n)
	if _, err := io.ReadFull(r, body); err != nil {
		return err
	}
	return json.Unmarshal(body, v)
}

// DefaultSockPath is the fixed path of nervud's admin channel. Kept apart from the App control plane
//
//	(/run/nervus/nervud.sock): that one serves Apps (UIDs in the App range), this one serves
//
// root/ops (0600), and the two admission rules don't mix.
const DefaultSockPath = "/run/nervus/nervud-admin.sock"

// dialTimeout / ioTimeout give the CLI side a path that can't hang forever. Admin commands are all quick;
// once nervud stops responding, the CLI should report an error and exit rather than stall ops scripts.
const (
	dialTimeout = 5 * time.Second
	ioTimeout   = 30 * time.Second // decisions/writes triggered by install may be a little slower; leave plenty of margin
)

// Client is nervusctl's minimal client. Not resident and stateless: each command is one
// Dial -> write -> read -> close (the source of truth is always the Registry inside nervud; the CLI only delivers).
type Client struct {
	sockPath string
}

// NewClient constructs a client pointing at sockPath.
func NewClient(sockPath string) *Client {
	if sockPath == "" {
		sockPath = DefaultSockPath
	}
	return &Client{sockPath: sockPath}
}

// Do runs one command: connect, write the Request, read the Response, close. A transport failure returns an error;
// a business failure (OK=false) isn't returned as an error and is left to the caller to handle by Response.Code.
func (c *Client) Do(req Request) (Response, error) {
	conn, err := net.DialTimeout("unix", c.sockPath, dialTimeout)
	if err != nil {
		return Response{}, fmt.Errorf("adminwire: dial %s: %w", c.sockPath, err)
	}
	defer func() { _ = conn.Close() }()

	if err := conn.SetDeadline(time.Now().Add(ioTimeout)); err != nil {
		return Response{}, err
	}
	// A write failure doesn't return at once; try reading a response first.
	//
	// When the server's SO_PEERCRED check fails, it writes
	// CodeUnauthorized and closes the connection before reading the request, deliberately (it doesn't read or parse an unauthorised peer's
	// payload). So this client may well hit EPIPE while writing its request: the connection has already
	// been closed by the other end, but the refusal is already waiting in the buffer.
	//
	// Returning the write error directly would show ops "broken pipe" instead of "unauthorized",
	// disguising a clear permission problem as a network fault and wasting a lot of debugging time.
	//
	// Only if the read also fails is the write error reported; that's a real connection problem.
	writeErr := WriteTo(conn, req)

	var resp Response
	if err := ReadFrom(conn, &resp); err != nil {
		if writeErr != nil {
			return Response{}, writeErr
		}
		return Response{}, fmt.Errorf("adminwire: read response: %w", err)
	}
	return resp, nil
}
