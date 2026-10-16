// Package adminclient is the client for nervud's admin channel (nervud-admin.sock).
//
// # Why it's rewritten here instead of importing nervud/internal/adminwire
//
// That's an internal package and can't be imported across modules. Also, one of adminwire's design goals is
// "a leaf package depending only on the standard library": its wire format is minimal (a 4-byte big-endian length + JSON),
// and copying it costs far less than dragging the whole kernel module into this repository's dependency tree.
//
// The cost is the risk of the two definitions drifting. The mitigation: golden assertions in wire_test.go pin
// the byte layout and every Cmd/Code constant, so a change on either side fails.
//
// # Who can use it
//
// Only nervus.pkgmanagerd. The kernel's admin.Config.ServiceUID only lets this one package's
// UID through; other processes can't even get past connect() (the socket is 0660, group = pkgmanagerd's GID).
//
// # It makes no security decisions
//
// This package only delivers commands and reads results back. Signature verification, digest re-checks, upgrade decisions
// and permission intersection all live in nervud's pkgregistry. Writing anything shaped like
// "if ... then allow" in this package is always a design error.
package main

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"time"
)

// DefaultSockPath is the admin channel's fixed path.
//
// It's a separate socket from the App control plane /run/nervus/nervud.sock: that one only accepts app-range
// UIDs, this one only accepts ops identities and pkgmanagerd. Don't mix them up.
const DefaultSockPath = "/run/nervus/nervud-admin.sock"

// Admin commands. They must match the kernel adminwire constants exactly.
const (
	CmdBeginStaging  = "begin-staging"
	CmdInstall       = "install"
	CmdUninstall     = "uninstall"
	CmdList          = "list"
	CmdSetEnabled    = "set-enabled"
	CmdSetPermission = "set-permission"
	// CmdInspect inspects a staged package read-only: it returns the list of USER_CONSENT permissions it asks for,
	// installing nothing and changing no state.
	//
	// It shares one staging directory with CmdInstall: the confirmation screen inspects, then installs,
	// and both must refer to the same tree, or there's a gap of "you looked at A, you installed B" in between.
	CmdInspect = "inspect"
)

// Wire representation of grant states.
const (
	GrantStateNotRequested    = "not-requested"
	GrantStateGranted         = "granted"
	GrantStateDenied          = "denied"
	GrantStateDeniedPermanent = "denied-permanent"
)

// Wire representation of RiskClass. Must match the kernel adminwire constants exactly: the two structs have no
// compile-time link, and a typo would make the confirmation screen show a CRITICAL_SAFETY permission as UNSPECIFIED.
const (
	RiskClassUnspecified      = "unspecified"
	RiskClassNormal           = "normal"
	RiskClassPrivacySensitive = "privacy-sensitive"
	RiskClassPhysicalControl  = "physical-control"
	RiskClassCriticalSafety   = "critical-safety"
)

// Result codes.
const (
	CodeOK           = "ok"
	CodeBadRequest   = "bad-request"
	CodeUnauthorized = "unauthorized"
	CodeNotFound     = "not-found"
	CodeFailed       = "failed"
)

// Request is one admin command sent to nervud.
// One command per connection: send a Request, receive a Response, then close.
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
	// [This service only forwards it, without checking]. nervud decides which entries really get stored, taking
	// the intersection "this list ∩ the install-time grant set ∩ USER_CONSENT" and silently ignoring anything outside it
	// (see nervud's pkgregistry.Module.applyInstallConsent). Filtering here in advance
	// would only create a second source of truth: this service can't see the Catalog and can't tell which tier a permission is in.
	//
	// The json tags must match nervud's adminwire.Request exactly: this struct is a hand-copied
	// duplicate of it (internal packages can't be imported across modules), and a mismatched field name raises no error,
	// it just silently drops the consent list, with the symptom "it installed but still doesn't have the permissions".
	ConsentedPermissions []string `json:"consented_permissions,omitempty"`

	// ExpectedManifestDigest travels with install: the digest returned by the confirmation screen's inspect.
	// This service only forwards it; the check is in nervud (see pkgregistry's ErrManifestDigestMismatch).
	ExpectedManifestDigest string `json:"expected_manifest_digest,omitempty"`
}

// Response is nervud's reply.
type Response struct {
	OK         bool          `json:"ok"`
	Code       string        `json:"code,omitempty"`
	Message    string        `json:"message,omitempty"`
	StagingDir string        `json:"staging_dir,omitempty"`
	Package    *PackageInfo  `json:"package,omitempty"`
	Packages   []PackageInfo `json:"packages,omitempty"`
	Inspect    *InspectInfo  `json:"inspect,omitempty"`
}

// InspectInfo is the result of a read-only inspection (the output of inspect).
//
// Deliberately separate from PackageInfo: that one describes an [installed] package (with the trust decision and granted
// permissions), this one a candidate [not yet installed]; fields neither has mean completely different things.
//
// The fields line up exactly with the struct of the same name in nervud's internal/adminwire. There's no
// compile-time link between the two, and a mistyped JSON tag silently drops a whole field (the symptom: the confirmation screen shows no permissions,
// and the user thinks the package asks for no sensitive permissions and agrees straight away).
type InspectInfo struct {
	ID          string `json:"id"`
	Version     string `json:"version"`
	VersionCode uint64 `json:"version_code"`

	// ManifestDigest must be brought back to install unchanged by the confirmation screen, or the gap of "you looked at A, you installed B"
	// stays open.
	ManifestDigest string `json:"manifest_digest,omitempty"`

	ConsentPermissions []ConsentPermissionInfo `json:"consent_permissions,omitempty"`
}

// ConsentPermissionInfo is one permission awaiting consent that the confirmation screen shows.
//
// The kernel takes the text from the Catalog's permission definitions and this service only forwards it: third-party packages can define their own
// permissions, and neither the forwarding layer nor the UI can know their names and descriptions in advance.
type ConsentPermissionInfo struct {
	ID              string `json:"id"`
	DisplayNameZhCN string `json:"display_name_zh_cn,omitempty"`
	DisplayNameEN   string `json:"display_name_en,omitempty"`
	DescriptionZhCN string `json:"description_zh_cn,omitempty"`
	DescriptionEN   string `json:"description_en,omitempty"`
	RiskClass       string `json:"risk_class,omitempty"`
}

// PackageInfo is the public projection of an installed Package.
type PackageInfo struct {
	ID          string   `json:"id"`
	Version     string   `json:"version"`
	VersionCode uint64   `json:"version_code"`
	Trust       string   `json:"trust"`
	Source      string   `json:"source"`
	Granted     []string `json:"granted,omitempty"`
	Disabled    []string `json:"disabled,omitempty"`
}

// MaxMessageBytes is the hard limit for one JSON message; it must match the kernel's.
const MaxMessageBytes = 1 << 20

const lengthPrefixBytes = 4

// ErrMessageTooLarge means the length prefix exceeds the hard limit.
var ErrMessageTooLarge = errors.New("adminclient: message exceeds hard limit")

const (
	dialTimeout = 3 * time.Second
	ioTimeout   = 30 * time.Second
)

// Client is the admin channel client. The zero value isn't usable; construct it with New.
type Client struct {
	sockPath string
}

// newAdminClient constructs a client. An empty sockPath means DefaultSockPath.
func newAdminClient(sockPath string) *Client {
	if sockPath == "" {
		sockPath = DefaultSockPath
	}
	return &Client{sockPath: sockPath}
}

// Do sends one command and waits for the reply. Each call makes a new connection (the admin plane is low-frequency, so simple beats reuse).
func (c *Client) Do(req Request) (Response, error) {
	conn, err := net.DialTimeout("unix", c.sockPath, dialTimeout)
	if err != nil {
		return Response{}, fmt.Errorf("adminclient: dial %s: %w", c.sockPath, err)
	}
	defer func() { _ = conn.Close() }()

	if err := conn.SetDeadline(time.Now().Add(ioTimeout)); err != nil {
		return Response{}, err
	}

	// A write failure [doesn't return at once]; try reading once first. When nervud's SO_PEERCRED check fails it
	// writes a refusal and closes the connection before reading the request, so this client may well hit
	// EPIPE mid-write while that refusal is already waiting in the buffer. Returning the write error directly would
	// log "broken pipe" instead of "unauthorized", disguising a clear permission problem
	// as a network fault. The kernel-side adminwire client does the same.
	writeErr := writeTo(conn, req)

	var resp Response
	if err := readFrom(conn, &resp); err != nil {
		if writeErr != nil {
			return Response{}, writeErr
		}
		return Response{}, fmt.Errorf("adminclient: read response: %w", err)
	}
	return resp, nil
}

func writeTo(w io.Writer, v any) error {
	body, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("adminclient: marshal: %w", err)
	}
	if len(body) > MaxMessageBytes {
		return fmt.Errorf("%w: %d > %d", ErrMessageTooLarge, len(body), MaxMessageBytes)
	}
	var hdr [lengthPrefixBytes]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(len(body)))
	if _, err := w.Write(hdr[:]); err != nil {
		return fmt.Errorf("adminclient: write header: %w", err)
	}
	if _, err := w.Write(body); err != nil {
		return fmt.Errorf("adminclient: write body: %w", err)
	}
	return nil
}

func readFrom(r io.Reader, v any) error {
	var hdr [lengthPrefixBytes]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return err
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n == 0 {
		return errors.New("adminclient: zero-length message")
	}
	if n > MaxMessageBytes {
		// Don't allocate a buffer for a body the other end merely claims
		return fmt.Errorf("%w: %d > %d", ErrMessageTooLarge, n, MaxMessageBytes)
	}
	body := make([]byte, n)
	if _, err := io.ReadFull(r, body); err != nil {
		return err
	}
	return json.Unmarshal(body, v)
}
