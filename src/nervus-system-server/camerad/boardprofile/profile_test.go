package boardprofile

import (
	"fmt"
	"strings"
	"testing"
)

const validProfile = `{
  "schema": 1,
  "board": "acme-quadruped-v2",
  "cameras": [
    {
      "role": "cam.front",
      "match": {"usb_path": "1-1.2"},
      "labels": {"nervus.camera.facing": "front", "nervus.camera.kind": "color"}
    },
    {
      "role": "cam.depth",
      "match": {"device_path_link": "platform-csi0-video-index0"},
      "labels": {"nervus.camera.kind": "depth", "nervus.camera.facing": "front"}
    }
  ],
  "usb_pool": {"role_prefix": "cam.usb", "max_slots": 2}
}`

func mustParse(t *testing.T, raw string) *Profile {
	t.Helper()
	p, err := Parse([]byte(raw))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return p
}

func mustReject(t *testing.T, raw, wantSubstring string) {
	t.Helper()
	_, err := Parse([]byte(raw))
	if err == nil {
		t.Fatal("this config should have been refused")
	}
	if !strings.Contains(err.Error(), wantSubstring) {
		t.Fatalf("err = %v, want it to contain %q", err, wantSubstring)
	}
}

func TestParse_Valid(t *testing.T) {
	p := mustParse(t, validProfile)
	if p.Board != "acme-quadruped-v2" {
		t.Errorf("board = %q", p.Board)
	}
	if len(p.Cameras) != 2 {
		t.Fatalf("cameras = %d, want 2", len(p.Cameras))
	}
}

// The resource set [includes pool slots with no device plugged in right now].
//
// Resource declarations answer "what could this board have"; "what it has right now" is expressed by RegisterEndpoint.
// The other way round (declaring only what's plugged in) would make plugging in a second camera need a repackage.
func TestAllRoles_IncludesEmptyPoolSlots(t *testing.T) {
	p := mustParse(t, validProfile)
	got := p.AllRoles()
	want := []string{"cam.front", "cam.depth", "cam.usb.0", "cam.usb.1"}
	if len(got) != len(want) {
		t.Fatalf("roles = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("roles = %v, want %v", got, want)
		}
	}
}

// [Pool slots have no semantic labels]. Nobody knows which way a camera plugged in on the fly faces,
// and giving it some facing would let Apps selecting by meaning pick a device facing who knows where.
func TestLabelsFor_PoolSlotsHaveNone(t *testing.T) {
	p := mustParse(t, validProfile)
	if labels := p.LabelsFor("cam.usb.0"); labels != nil {
		t.Fatalf("pool slot carries labels: %v", labels)
	}
	if labels := p.LabelsFor("cam.front"); labels[LabelFacing] != "front" {
		t.Fatalf("cam.front labels = %v", labels)
	}
}

// It returns a copy: a caller changing it mustn't affect the Profile.
func TestLabelsFor_ReturnsCopy(t *testing.T) {
	p := mustParse(t, validProfile)
	labels := p.LabelsFor("cam.front")
	labels[LabelFacing] = "rear"
	if again := p.LabelsFor("cam.front"); again[LabelFacing] != "front" {
		t.Fatalf("the caller damaged the Profile: %v", again)
	}
}

// [Unknown fields must be an error]. "label" in the singular would be silently ignored, leaving the camera without labels
// so Apps can't select it, while the config file looks perfectly normal.
func TestParse_UnknownFieldIsRejected(t *testing.T) {
	mustReject(t, `{
	  "schema": 1, "board": "b",
	  "cameras": [{"role": "cam.a", "match": {"usb_path": "1-1"}, "label": {"x": "y"}}]
	}`, "unknown field")
}

// A misspelt label key must be refused at build time. At run time it would look like "this camera can never be selected",
// and nothing would point at the typo.
func TestParse_UnknownLabelKeyIsRejected(t *testing.T) {
	mustReject(t, `{
	  "schema": 1, "board": "b",
	  "cameras": [{"role": "cam.a", "match": {"usb_path": "1-1"},
	               "labels": {"nervus.camera.face": "front"}}]
	}`, "unknown label key")
}

// A misspelt label [value] is just as fatal, and better hidden: facing="Front" looks fine in JSON.
func TestParse_UnknownLabelValueIsRejected(t *testing.T) {
	mustReject(t, `{
	  "schema": 1, "board": "b",
	  "cameras": [{"role": "cam.a", "match": {"usb_path": "1-1"},
	               "labels": {"nervus.camera.facing": "Front"}}]
	}`, "is not a known facing")
}

// Private labels under camerad's own namespace aren't limited by the allowlist.
func TestParse_PrivateLabelNamespaceIsFree(t *testing.T) {
	p := mustParse(t, `{
	  "schema": 1, "board": "b",
	  "cameras": [{"role": "cam.a", "match": {"usb_path": "1-1"},
	               "labels": {"nervus.camerad.tuning_profile": "indoor"}}]
	}`)
	if got := p.LabelsFor("cam.a")["nervus.camerad.tuning_profile"]; got != "indoor" {
		t.Fatalf("private label = %q", got)
	}
}

// The position must be given [exactly] one way. Both means ambiguity; neither means this role
// can never bind to a device.
func TestParse_MatchMustBeExactlyOne(t *testing.T) {
	mustReject(t, `{
	  "schema": 1, "board": "b",
	  "cameras": [{"role": "cam.a", "match": {"usb_path": "1-1", "device_path_link": "x"}}]
	}`, "pick only one")

	mustReject(t, `{
	  "schema": 1, "board": "b",
	  "cameras": [{"role": "cam.a", "match": {}}]
	}`, "doesn't say where the device is")
}

func TestParse_DuplicateRoleIsRejected(t *testing.T) {
	mustReject(t, `{
	  "schema": 1, "board": "b",
	  "cameras": [
	    {"role": "cam.a", "match": {"usb_path": "1-1"}},
	    {"role": "cam.a", "match": {"usb_path": "1-2"}}
	  ]
	}`, "is duplicated")
}

// A pool slot sharing a name with a fixed role would make two cameras claim to be the same resource,
// and which one wins depends on setup order.
func TestParse_PoolSlotCollidingWithFixedRoleIsRejected(t *testing.T) {
	mustReject(t, `{
	  "schema": 1, "board": "b",
	  "cameras": [{"role": "cam.usb.0", "match": {"usb_path": "1-1"}}],
	  "usb_pool": {"role_prefix": "cam.usb", "max_slots": 2}
	}`, "collides with a fixed camera role")
}

// Config roles are generated from the capture role plus a suffix, and also go into the Catalog's global handle index.
//
// If the collision isn't refused here, the error at setup points at the Catalog ("resource handle collides"),
// while the root cause is in this JSON, and by then nobody thinks to look back at the board config.
func TestParse_ConfigRoleCollisionIsRejected(t *testing.T) {
	mustReject(t, `{
	  "schema": 1, "board": "b",
	  "cameras": [
	    {"role": "cam.front", "match": {"usb_path": "1-1"}},
	    {"role": "cam.front.config", "match": {"usb_path": "1-2"}}
	  ]
	}`, "collides with the config role")
}

// A pool slot's config role takes part in the collision check too.
func TestParse_PoolConfigRoleCollisionIsRejected(t *testing.T) {
	mustReject(t, `{
	  "schema": 1, "board": "b",
	  "cameras": [{"role": "cam.usb.0.config", "match": {"usb_path": "1-9"}}],
	  "usb_pool": {"role_prefix": "cam.usb", "max_slots": 2}
	}`, "collides with the config role")
}

func TestAllConfigRoles_MirrorsAllRoles(t *testing.T) {
	p := mustParse(t, validProfile)
	capture, config := p.AllRoles(), p.AllConfigRoles()
	if len(capture) != len(config) {
		t.Fatalf("%d capture roles and %d config roles; they must correspond one to one", len(capture), len(config))
	}
	for i := range capture {
		if config[i] != capture[i]+ConfigRoleSuffix {
			t.Fatalf("config[%d] = %q, want %q", i, config[i], capture[i]+ConfigRoleSuffix)
		}
	}
}

// A schema version mismatch is refused outright, with no "best-effort" parsing.
func TestParse_SchemaVersionMustMatch(t *testing.T) {
	mustReject(t, `{"schema": 2, "board": "b",
	  "cameras": [{"role": "cam.a", "match": {"usb_path": "1-1"}}]}`, "only supports")
}

// An empty config makes no sense; most likely the file was written in the wrong place.
func TestParse_EmptyProfileIsRejected(t *testing.T) {
	mustReject(t, `{"schema": 1, "board": "b"}`, "makes no sense")
}

func TestParse_PoolBoundsAreChecked(t *testing.T) {
	base := `{"schema": 1, "board": "b", "usb_pool": {"role_prefix": %q, "max_slots": %d}}`
	mustReject(t, fmt.Sprintf(base, "", 2), "role_prefix can't be empty")
	mustReject(t, fmt.Sprintf(base, "cam.usb", 0), "must be positive")
	mustReject(t, fmt.Sprintf(base, "cam.usb", 65), "exceeds 64")
}

func TestIndexes_MapPositionToRole(t *testing.T) {
	p := mustParse(t, validProfile)
	if got := p.FixedByUSBPath()["1-1.2"]; got != "cam.front" {
		t.Errorf("USB index = %q, want cam.front", got)
	}
	if got := p.FixedByDeviceLink()["platform-csi0-video-index0"]; got != "cam.depth" {
		t.Errorf("link index = %q, want cam.depth", got)
	}
}
