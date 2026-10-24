// Package boardprofile parses a board's camera configuration.
//
// # It is read twice, deliberately
//
//	build time  providergen reads it → generates the nervus.resource.camera declarations in ProviderArtifacts
//	run time    camerad reads it     → maps roles to real device nodes
//
// [One file, one parser]. With two config files, "the Catalog has cam.front"
// and "which device cam.front points at at run time" would become two facts that can drift independently, and after drifting
// neither side reports an error; an App asking for the front camera just gets an endpoint that doesn't exist.
//
// # Why the resource declarations must be fixed at build time
//
// ProviderArtifacts go into the manifest's digests and are signed with the image. If the resource set were read
// fresh from JSON at run time, editing one line of JSON would add yourself a "front camera"; something the signature doesn't
// cover isn't a contract.
//
// The cost is repackaging for a new board. That's right: the board config is part of the image anyway.
package boardprofile

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
)

// SchemaVersion is the board config version this package can parse.
const SchemaVersion = 1

// ConfigRoleSuffix is the suffix the config resource adds to the capture role.
//
// # Why the config side can't reuse the same role
//
// One camera maps to [two resources]: nervus.resource.camera (capture, shared observation) and
// nervus.resource.camera.config (config, exclusive lease). Their resource types differ, but
// the Catalog's handle index is [globally unique across types]: a shared handle name fails setup with
// "resource handle collides", and the whole Catalog build fails.
//
// So the config side gets its own role: cam.front's config side is cam.front.config.
const ConfigRoleSuffix = ".config"

// Platform semantic label keys. [The allowlist isn't fussiness]: a misspelt key (nervus.camera.face) raises no
// error; it just makes this camera impossible to select. Apps look up by facing, find nothing, and someone spends
// half a day checking IPC. Refusing it at build time costs one failed build.
const (
	LabelFacing = "nervus.camera.facing"
	LabelKind   = "nervus.camera.kind"
	LabelMount  = "nervus.camera.mount"

	// Keys under privateLabelPrefix aren't limited by the allowlist: that's camerad's own private namespace,
	// and the kernel's namespace rules let this package use it freely.
	privateLabelPrefix = "nervus.camerad."
)

// Semantic values are allowlisted too. A misspelt [value] is as fatal as a misspelt key, and better hidden:
// facing="Front" and facing="front" look the same in JSON.
var (
	knownFacing = map[string]bool{
		"front": true, "rear": true, "left": true, "right": true,
		"up": true, "down": true,
	}
	knownKind = map[string]bool{
		"color": true, "depth": true, "ir": true, "fisheye": true,
	}
)

// Profile is one board's camera layout.
type Profile struct {
	Schema int    `json:"schema"`
	Board  string `json:"board"`

	// Cameras are cameras [in fixed positions]: MIPI modules soldered to the board, or modules inside the body
	// on a known USB port. Their roles and semantics come from board facts and don't change with plugging.
	Cameras []Camera `json:"cameras"`

	// USBPool is where [temporarily plugged-in] USB cameras land. Optional.
	USBPool *USBPool `json:"usb_pool,omitempty"`
}

// Camera is one camera in a fixed position.
type Camera struct {
	// Role is the stable handle, such as "cam.front". It goes into the Catalog and is ResourceSelector.role.
	Role string `json:"role"`

	// Match says which physical position on the board this role corresponds to.
	Match Match `json:"match"`

	// Labels are semantic labels; Apps select devices by them, not by role.
	//
	// [This is what Apps should depend on]: a role is the board's naming (this board calls it cam.front,
	// the next might call it camera0), while labels are semantics (faces forward).
	Labels map[string]string `json:"labels,omitempty"`
}

// Match is how the device is located. [Exactly one] of the two must be given.
type Match struct {
	// USBPath is a USB topology path such as "1-1.2": port 2 under port 1 of controller 1.
	//
	// [A topology path rather than /dev/videoN]: videoN numbers depend on driver load order,
	// and change with a different kernel version or one more device plugged in. A topology path describes "which
	// physical port it's plugged into", which doesn't change unless it's rewired.
	//
	// [Not a serial number either]: replace a camera with one of the same model to repair the machine and the serial changes, losing the role.
	// A board config describes positions, not a specific module.
	USBPath string `json:"usb_path,omitempty"`

	// USBID is "vendor:product" in hex, such as "046d:0825" (a Logitech C270), for a USB camera that moves
	// between ports: on a hosted laptop the cameras hang off a hub and get replugged. It names a model, so it
	// must be the only camera of that model on the machine.
	USBID string `json:"usb_id,omitempty"`

	// DevicePathLink is the name of a stable symlink under /dev/v4l/by-path/, for MIPI/CSI
	// and other cameras that don't use USB.
	DevicePathLink string `json:"device_path_link,omitempty"`
}

// USBPool declares a range of dynamic USB camera slots.
//
// # Cold plug only
//
// Slots are assigned once, sorted by USB topology path, [at the moment the service starts], and never change after that.
// Cameras plugged in while running don't appear automatically; unplugging invalidates the matching endpoint but the slot isn't reclaimed.
//
// Hot plug would mean: a new device appears → an endpoint is registered dynamically → a resource appears out of nowhere in the Catalog.
// But the Catalog's resource set is signed at build time, and adding resources at run time would bypass the signature. Supporting it
// first needs an answer to "how do dynamic resources enter the signed contract", which is bigger than one camera capability.
type USBPool struct {
	// RolePrefix generates roles like "<prefix>.0", "<prefix>.1".
	RolePrefix string `json:"role_prefix"`

	// MaxSlots is the slot limit, fixed at build time.
	//
	// [There must be a limit]: resource declarations are generated at build time, so the count must be fixed. Plugging in camera MaxSlots+1
	// means it doesn't appear, which beats it appearing on a resource the signature doesn't cover.
	MaxSlots int `json:"max_slots"`
}

// Load reads and validates a board config.
func Load(path string) (*Profile, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("boardprofile: read %s: %w", path, err)
	}
	return Parse(raw)
}

// Parse parses and validates a board config.
func Parse(raw []byte) (*Profile, error) {
	var p Profile
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	// [Refuse unknown fields]: a misspelt key ("label" instead of "labels") would be silently ignored,
	// leaving the camera without labels so Apps can't select it, while the config file looks perfectly normal.
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&p); err != nil {
		return nil, fmt.Errorf("boardprofile: parse failed: %w", err)
	}
	if err := p.validate(); err != nil {
		return nil, err
	}
	return &p, nil
}

func (p *Profile) validate() error {
	if p.Schema != SchemaVersion {
		return fmt.Errorf("boardprofile: schema = %d; this build only supports %d", p.Schema, SchemaVersion)
	}
	if p.Board == "" {
		return fmt.Errorf("boardprofile: board can't be empty")
	}
	if len(p.Cameras) == 0 && p.USBPool == nil {
		return fmt.Errorf("boardprofile: there are no fixed cameras and no USB pool, so this config makes no sense")
	}

	seen := make(map[string]struct{}, len(p.Cameras))
	for i := range p.Cameras {
		cam := &p.Cameras[i]
		if err := cam.validate(); err != nil {
			return err
		}
		if _, dup := seen[cam.Role]; dup {
			return fmt.Errorf("boardprofile: role %q is duplicated", cam.Role)
		}
		seen[cam.Role] = struct{}{}
	}

	if p.USBPool != nil {
		if err := p.USBPool.validate(); err != nil {
			return err
		}
		// A pool slot sharing a name with a fixed role would make two cameras claim to be the same resource, and which one
		// wins depends on setup order. Refuse it at build time.
		for _, role := range p.USBPool.Roles() {
			if _, dup := seen[role]; dup {
				return fmt.Errorf("boardprofile: USB pool slot %q collides with a fixed camera role", role)
			}
			seen[role] = struct{}{}
		}
	}

	// Config roles are the capture role plus a suffix, and they also go into the Catalog's global handle
	// index. A camera called cam.front generates cam.front.config, so if another camera
	// is already called cam.front.config, the two collide on a handle during setup, and the error then points at
	// the Catalog, with no hint that the root cause is in this JSON.
	for role := range seen {
		configRole := role + ConfigRoleSuffix
		if _, dup := seen[configRole]; dup {
			return fmt.Errorf(
				"boardprofile: role %q collides with the config role of %q (a config role is the capture role plus %q)",
				configRole, role, ConfigRoleSuffix)
		}
	}
	return nil
}

func (c *Camera) validate() error {
	if c.Role == "" {
		return fmt.Errorf("boardprofile: a camera has no role")
	}
	n := 0
	for _, s := range []string{c.Match.USBPath, c.Match.USBID, c.Match.DevicePathLink} {
		if s != "" {
			n++
		}
	}
	switch {
	case n > 1:
		return fmt.Errorf("boardprofile: %q gives more than one of usb_path, usb_id and device_path_link; pick only one",
			c.Role)
	case n == 0:
		return fmt.Errorf("boardprofile: %q doesn't say where the device is (usb_path, usb_id or device_path_link)",
			c.Role)
	}
	if c.Match.USBID != "" && !usbIDRe.MatchString(c.Match.USBID) {
		return fmt.Errorf("boardprofile: %q usb_id %q must look like 046d:0825", c.Role, c.Match.USBID)
	}
	return validateLabels(c.Role, c.Labels)
}

func validateLabels(role string, labels map[string]string) error {
	for key, value := range labels {
		if strings.HasPrefix(key, privateLabelPrefix) {
			continue
		}
		switch key {
		case LabelFacing:
			if !knownFacing[value] {
				return fmt.Errorf("boardprofile: %q has %s = %q, which is not a known facing (%s)",
					role, key, value, sortedKeys(knownFacing))
			}
		case LabelKind:
			if !knownKind[value] {
				return fmt.Errorf("boardprofile: %q has %s = %q, which is not a known kind (%s)",
					role, key, value, sortedKeys(knownKind))
			}
		case LabelMount:
			if value == "" {
				return fmt.Errorf("boardprofile: %q has an empty %s", role, key)
			}
		default:
			return fmt.Errorf(
				"boardprofile: %q uses unknown label key %q; platform semantic labels are limited to %s/%s/%s, "+
					"and private labels must start with %s",
				role, key, LabelFacing, LabelKind, LabelMount, privateLabelPrefix)
		}
	}
	return nil
}

func (u *USBPool) validate() error {
	if u.RolePrefix == "" {
		return fmt.Errorf("boardprofile: usb_pool.role_prefix can't be empty")
	}
	if strings.ContainsAny(u.RolePrefix, " \t\n") {
		return fmt.Errorf("boardprofile: usb_pool.role_prefix %q contains whitespace", u.RolePrefix)
	}
	if u.MaxSlots <= 0 {
		return fmt.Errorf("boardprofile: usb_pool.max_slots = %d; it must be positive", u.MaxSlots)
	}
	// The limit isn't about saving memory; it's so that "how many cameras could this board have" has
	// an answer you can say out loud. 64 is far beyond any robot.
	if u.MaxSlots > 64 {
		return fmt.Errorf("boardprofile: usb_pool.max_slots = %d, which exceeds 64", u.MaxSlots)
	}
	return nil
}

// Roles gives the roles of every slot in the pool, in slot order.
func (u *USBPool) Roles() []string {
	if u == nil {
		return nil
	}
	roles := make([]string, 0, u.MaxSlots)
	for i := 0; i < u.MaxSlots; i++ {
		roles = append(roles, fmt.Sprintf("%s.%d", u.RolePrefix, i))
	}
	return roles
}

// AllRoles gives every role this config declares, fixed cameras first, then pool slots.
//
// This is the resource set providergen writes into ProviderArtifacts, [including pool slots with no device
// plugged in right now]. Resource declarations are "what this board could have", not "what it has right now";
// the latter is expressed by RegisterEndpoint.
func (p *Profile) AllRoles() []string {
	roles := make([]string, 0, len(p.Cameras)+p.USBPool.slots())
	for _, cam := range p.Cameras {
		roles = append(roles, cam.Role)
	}
	return append(roles, p.USBPool.Roles()...)
}

func (u *USBPool) slots() int {
	if u == nil {
		return 0
	}
	return u.MaxSlots
}

// ConfigRole gives the config role for a capture role. See ConfigRoleSuffix.
func ConfigRole(role string) string { return role + ConfigRoleSuffix }

// AllConfigRoles is AllRoles' config-side counterpart, in the same order.
func (p *Profile) AllConfigRoles() []string {
	roles := p.AllRoles()
	out := make([]string, len(roles))
	for i, role := range roles {
		out[i] = ConfigRole(role)
	}
	return out
}

// LabelsFor returns a role's semantic labels. Pool slots [have no labels].
//
// That's not an omission: nobody knows which way a USB camera plugged in on the fly faces or what kind it is.
// Giving it some facing would let Apps selecting by meaning pick a device facing who knows where, worse than finding nothing.
func (p *Profile) LabelsFor(role string) map[string]string {
	for _, cam := range p.Cameras {
		if cam.Role != role {
			continue
		}
		if len(cam.Labels) == 0 {
			return nil
		}
		out := make(map[string]string, len(cam.Labels))
		for k, v := range cam.Labels {
			out[k] = v
		}
		return out
	}
	return nil
}

// FixedByUSBPath and FixedByDeviceLink are the indexes used at run time for role mapping.
func (p *Profile) FixedByUSBPath() map[string]string {
	out := make(map[string]string)
	for _, cam := range p.Cameras {
		if cam.Match.USBPath != "" {
			out[cam.Match.USBPath] = cam.Role
		}
	}
	return out
}

func (p *Profile) FixedByUSBID() map[string]string {
	out := make(map[string]string)
	for _, cam := range p.Cameras {
		if cam.Match.USBID != "" {
			out[strings.ToLower(cam.Match.USBID)] = cam.Role
		}
	}
	return out
}

var usbIDRe = regexp.MustCompile(`^[0-9A-Fa-f]{4}:[0-9A-Fa-f]{4}$`)

func (p *Profile) FixedByDeviceLink() map[string]string {
	out := make(map[string]string)
	for _, cam := range p.Cameras {
		if cam.Match.DevicePathLink != "" {
			out[cam.Match.DevicePathLink] = cam.Role
		}
	}
	return out
}

func sortedKeys(m map[string]bool) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return strings.Join(keys, "/")
}
