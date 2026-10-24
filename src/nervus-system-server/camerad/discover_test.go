//go:build linux

package main

import (
	"sort"
	"testing"

	"github.com/nervus-os/nervus-system-server/camerad/boardprofile"
)

func profileOf(t *testing.T, raw string) *boardprofile.Profile {
	t.Helper()
	p, err := boardprofile.Parse([]byte(raw))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return p
}

func fixedDevices(devices ...Device) deviceLister {
	return func() ([]Device, error) { return devices, nil }
}

func bindMap(t *testing.T, p *boardprofile.Profile, list deviceLister) map[string]string {
	t.Helper()
	bindings, err := Bind(p, list)
	if err != nil {
		t.Fatalf("Bind: %v", err)
	}
	out := make(map[string]string, len(bindings))
	for _, b := range bindings {
		out[b.Role] = b.Device.Node
	}
	return out
}

const poolProfile = `{
  "schema": 1, "board": "b",
  "cameras": [{"role": "cam.front", "match": {"usb_path": "1-1.1"},
               "labels": {"nervus.camera.facing": "front"}}],
  "usb_pool": {"role_prefix": "cam.usb", "max_slots": 3}
}`

// Fixed roles match by physical position, regardless of the /dev/videoN number.
//
// Numbers depend on driver load order; mapping by them means that after one reboot "front"
// becomes rear, and no log mentions it.
func TestBind_FixedRoleIgnoresNodeNumber(t *testing.T) {
	p := profileOf(t, poolProfile)
	// The front camera got video7 this time, not video0
	got := bindMap(t, p, fixedDevices(
		Device{Node: "/dev/video7", USBPath: "1-1.1"},
	))
	if got["cam.front"] != "/dev/video7" {
		t.Fatalf("cam.front = %q, want /dev/video7", got["cam.front"])
	}
}

// [Pool slots are ordered by USB topology path]: a device on the same port always gets the same slot.
func TestBind_PoolOrdersByUSBTopology(t *testing.T) {
	p := profileOf(t, poolProfile)
	// Deliberately make node numbers the reverse of topology order
	got := bindMap(t, p, fixedDevices(
		Device{Node: "/dev/video0", USBPath: "1-3"},
		Device{Node: "/dev/video1", USBPath: "1-2"},
	))
	if got["cam.usb.0"] != "/dev/video1" {
		t.Errorf("cam.usb.0 = %q, want /dev/video1 (1-2 comes before 1-3)", got["cam.usb.0"])
	}
	if got["cam.usb.1"] != "/dev/video0" {
		t.Errorf("cam.usb.1 = %q, want /dev/video0", got["cam.usb.1"])
	}
}

// [Segments must compare numerically]. Lexicographic order puts "1-10" before "1-2", so on a machine
// with 10 ports in use slot order wouldn't match physical order, and only once the device count passes 10.
func TestLessUSBPath_ComparesSegmentsNumerically(t *testing.T) {
	paths := []string{"1-10", "1-2", "1-1.10", "1-1.2", "2-1", "1-1"}
	sort.Slice(paths, func(i, j int) bool { return lessUSBPath(paths[i], paths[j]) })

	want := []string{"1-1", "1-1.2", "1-1.10", "1-2", "1-10", "2-1"}
	for i := range want {
		if paths[i] != want[i] {
			t.Fatalf("order = %v, want %v", paths, want)
		}
	}
}

// Devices already claimed by a fixed role don't go into the pool, or the same camera would be both cam.front
// and cam.usb.0, with two endpoints fighting over one /dev/videoN.
func TestBind_FixedDeviceIsNotAlsoPooled(t *testing.T) {
	p := profileOf(t, poolProfile)
	got := bindMap(t, p, fixedDevices(
		Device{Node: "/dev/video0", USBPath: "1-1.1"}, // fixed: cam.front
		Device{Node: "/dev/video1", USBPath: "1-2"},   // into the pool
	))
	if got["cam.front"] != "/dev/video0" {
		t.Fatalf("cam.front = %q", got["cam.front"])
	}
	if got["cam.usb.0"] != "/dev/video1" {
		t.Fatalf("cam.usb.0 = %q, want /dev/video1", got["cam.usb.0"])
	}
	if len(got) != 2 {
		t.Fatalf("bindings = %v, want exactly two", got)
	}
}

// [A role that isn't plugged in produces no binding]. The resource exists in the Catalog (declared at build time),
// the endpoint doesn't (not plugged in at run time), so the App gets a clear NOT_FOUND
// instead of an endpoint it can connect to that never produces a frame.
func TestBind_AbsentRolesProduceNoBinding(t *testing.T) {
	p := profileOf(t, poolProfile)
	got := bindMap(t, p, fixedDevices())
	if len(got) != 0 {
		t.Fatalf("bindings = %v, want none", got)
	}
}

// Devices beyond max_slots are dropped: the resource declarations are signed at build time,
// and extra devices have no resource to bind to.
func TestBind_PoolOverflowIsDropped(t *testing.T) {
	p := profileOf(t, poolProfile)
	got := bindMap(t, p, fixedDevices(
		Device{Node: "/dev/video0", USBPath: "1-2"},
		Device{Node: "/dev/video1", USBPath: "1-3"},
		Device{Node: "/dev/video2", USBPath: "1-4"},
		Device{Node: "/dev/video3", USBPath: "1-5"}, // the fourth; the pool has only three slots
	))
	if len(got) != 3 {
		t.Fatalf("bindings = %v, want three", got)
	}
	if got["cam.usb.2"] != "/dev/video2" {
		t.Errorf("cam.usb.2 = %q, want /dev/video2 (the last slot goes to 1-4)", got["cam.usb.2"])
	}
}

// Non-USB devices don't go into the pool: they have no topology path, so there's nothing to sort by.
// MIPI/CSI cameras must be listed under cameras and located with device_path_link.
func TestBind_NonUSBDevicesStayOutOfPool(t *testing.T) {
	p := profileOf(t, poolProfile)
	got := bindMap(t, p, fixedDevices(
		Device{Node: "/dev/video0", PathLink: "platform-csi0-video-index0"},
	))
	if len(got) != 0 {
		t.Fatalf("bindings = %v, want none (CSI devices shouldn't go into the pool)", got)
	}
}

func TestBind_DeviceLinkMatchesFixedRole(t *testing.T) {
	p := profileOf(t, `{
	  "schema": 1, "board": "b",
	  "cameras": [{"role": "cam.depth",
	               "match": {"device_path_link": "platform-csi0-video-index0"},
	               "labels": {"nervus.camera.kind": "depth"}}]
	}`)
	got := bindMap(t, p, fixedDevices(
		Device{Node: "/dev/video4", PathLink: "platform-csi0-video-index0"},
	))
	if got["cam.depth"] != "/dev/video4" {
		t.Fatalf("cam.depth = %q, want /dev/video4", got["cam.depth"])
	}
}

// A role matching two devices means the board config's position description isn't precise enough.
//
// [It must be an error, not a random pick]: picking wrong means the picture faces the wrong way, which is very hard to
// attribute during integration, since both devices are under the same USB path and the config looks perfectly right.
func TestBind_AmbiguousFixedMatchIsAnError(t *testing.T) {
	p := profileOf(t, poolProfile)
	_, err := Bind(p, fixedDevices(
		Device{Node: "/dev/video0", USBPath: "1-1.1"},
		Device{Node: "/dev/video1", USBPath: "1-1.1"},
	))
	if err == nil {
		t.Fatal("one role matched two devices without an error")
	}
}

// An upstream port sorts before the hub ports below it: "1-1" before "1-1.2".
func TestLessUSBPath_ShorterPrefixComesFirst(t *testing.T) {
	if !lessUSBPath("1-1", "1-1.2") {
		t.Error("1-1 should sort before 1-1.2")
	}
	if lessUSBPath("1-1.2", "1-1") {
		t.Error("the comparison isn't antisymmetric")
	}
}

// A segment that doesn't parse as a number counts as -1 instead of being skipped: skipping would misalign "1-a.2" against "1-2"
// and judge two unrelated paths equal.
func TestUSBSegments_KeepsUnparsableSegments(t *testing.T) {
	if got := usbSegments("1-a.2"); len(got) != 3 || got[1] != -1 {
		t.Fatalf("segments = %v, want three segments with -1 in the middle", got)
	}
}

// A camera matched by model keeps its role on any port, and the rest still go to the pool.
func TestBind_USBIDFollowsTheCameraAcrossPorts(t *testing.T) {
	p := profileOf(t, `{
  "schema": 1, "board": "b",
  "cameras": [{"role": "cam.front", "match": {"usb_id": "046D:0825"},
               "labels": {"nervus.camera.facing": "front"}}],
  "usb_pool": {"role_prefix": "cam.usb", "max_slots": 2}
}`)
	for _, port := range []string{"3-5.4", "3-2"} {
		got := bindMap(t, p, fixedDevices(
			Device{Node: "/dev/video32", USBPath: "3-5.1.2", USBID: "2bc5:0557"},
			Device{Node: "/dev/video34", USBPath: port, USBID: "046d:0825"},
		))
		if got["cam.front"] != "/dev/video34" || got["cam.usb.0"] != "/dev/video32" {
			t.Fatalf("port %s: bindings %v", port, got)
		}
	}
	if _, err := boardprofile.Parse([]byte(`{"schema": 1, "board": "b", "cameras": [{"role": "cam.x",
		"match": {"usb_id": "046d:0825", "usb_path": "1-1"}}]}`)); err == nil {
		t.Fatal("usb_id together with usb_path should be refused")
	}
}

// A virtual camera (v4l2loopback fed by libcamera) has no USB path: it is matched by its device name.
func TestBind_NameMatchesAVirtualCamera(t *testing.T) {
	p := profileOf(t, `{
  "schema": 1, "board": "b",
  "cameras": [{"role": "cam.front", "match": {"name": "Surface Front Camera"},
               "labels": {"nervus.camera.facing": "front"}}],
  "usb_pool": {"role_prefix": "cam.usb", "max_slots": 2}
}`)
	got := bindMap(t, p, fixedDevices(
		Device{Node: "/dev/video32", USBPath: "3-5.1.2", USBID: "2bc5:0557", Name: "Dabai DC1: Dabai DC1"},
		Device{Node: "/dev/video50", Name: "Surface Front Camera"},
	))
	if got["cam.front"] != "/dev/video50" || got["cam.usb.0"] != "/dev/video32" {
		t.Fatalf("bindings %v", got)
	}
}
