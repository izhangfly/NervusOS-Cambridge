//go:build linux

// This file maps "the roles in the board config" to "the V4L2 devices really on the board".
//
// Two paths:
//
//	fixed cameras     exact match by usb_path / device_path_link; same position means same role
//	USB cold-plug pool  slots taken in turn after sorting by USB topology path
//
// # Why not /dev/videoN
//
// videoN numbers depend on driver load order. On the same machine they can change with a different kernel version, one more device plugged in,
// or even start-up timing jitter. Mapping roles by them means that after one reboot
// "front" becomes rear, and no log mentions it.
//
// A USB topology path ("1-1.2") describes [which physical port it's plugged into]. It doesn't change unless it's rewired.
//
// # Why one physical camera may have several videoN
//
// UVC devices often register two nodes: one for capture, one for metadata. Only the one with
// V4L2_CAP_VIDEO_CAPTURE can stream; the other must be skipped, or half the devices
// would open fine and never produce a frame.
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/nervus-os/nervus-system-server/camerad/boardprofile"
)

// sysV4L2Root is where V4L2 devices live in sysfs.
const sysV4L2Root = "/sys/class/video4linux"

// Device is a discovered capture device.
type Device struct {
	// Node is /dev/videoN. Only meaningful for this boot.
	Node string
	// USBPath is the USB topology path, such as "1-1.2". Empty for non-USB devices.
	USBPath string
	// PathLink is the link name under /dev/v4l/by-path/. Empty if there isn't one.
	PathLink string
	// Name is the device name the driver reports, for logs only.
	Name string
}

// Binding binds a role to a device.
type Binding struct {
	Role   string
	Device Device
}

// deviceLister makes discovery testable: the real implementation reads sysfs, tests supply a fixed list.
//
// Pulled out not for "decoupling" but because the slot allocation rules [can only be verified with constructed device lists],
// and plugging four cameras into CI isn't realistic.
type deviceLister func() ([]Device, error)

// Bind maps the board config onto the actual devices.
//
// The returned bindings [only include devices that really exist]. Roles declared in the board config but not plugged in don't appear;
// that's exactly what "only RegisterEndpoint for devices that really exist" means: the resource exists in the Catalog
// (declared at build time), the endpoint doesn't (not plugged in at run time). The App gets a clear
// NOT_FOUND instead of an endpoint it can connect to that never produces a frame.
func Bind(profile *boardprofile.Profile, list deviceLister) ([]Binding, error) {
	devices, err := list()
	if err != nil {
		return nil, err
	}

	byUSB := profile.FixedByUSBPath()
	byLink := profile.FixedByDeviceLink()

	var bindings []Binding
	claimed := make(map[string]bool, len(devices)) // node -> already taken by a fixed role
	seenRole := make(map[string]string)            // role -> node, for spotting duplicate matches

	for _, dev := range devices {
		role, ok := matchFixed(dev, byUSB, byLink)
		if !ok {
			continue
		}
		// One role matching two nodes: UVC's capture and metadata nodes both hang off the same
		// USB path. The list is sorted by node number, so take the first one that can capture; nodes that can't
		// capture never appear in devices at all (see the lister's filter).
		if prev, dup := seenRole[role]; dup {
			return nil, fmt.Errorf(
				"camerad: role %q matches both %s and %s; the board config's position description isn't precise enough",
				role, prev, dev.Node)
		}
		seenRole[role] = dev.Node
		claimed[dev.Node] = true
		bindings = append(bindings, Binding{Role: role, Device: dev})
	}

	bindings = append(bindings, assignPool(profile.USBPool, devices, claimed)...)
	return bindings, nil
}

func matchFixed(dev Device, byUSB, byLink map[string]string) (string, bool) {
	if dev.USBPath != "" {
		if role, ok := byUSB[dev.USBPath]; ok {
			return role, true
		}
	}
	if dev.PathLink != "" {
		if role, ok := byLink[dev.PathLink]; ok {
			return role, true
		}
	}
	return "", false
}

// assignPool fills pool slots, in topology order, with USB devices not claimed by a fixed role.
//
// # Sorting is the stability
//
// Sorting by USB topology path means [a device on the same port always gets the same slot], as long as
// the set of plugged-in devices doesn't change. That's all "cold plug" means: plug in before boot, unchanged after.
//
// [But it isn't stable when the set changes]: unplug the camera on 1-1.2 and the one on 1-1.3 moves from
// usb.1 to usb.0. That's not a defect but an inherent property of a USB pool: "the Nth one"
// is relative by nature. Devices that need absolute stability must be listed under cameras with an exact usb_path.
//
// Devices beyond max_slots are dropped. Resource declarations are signed at build time, and extra devices at run time
// have no resource to bind to; silently dropping them with a log line beats binding them to a role the signature doesn't cover.
func assignPool(pool *boardprofile.USBPool, devices []Device, claimed map[string]bool) []Binding {
	if pool == nil {
		return nil
	}

	var pending []Device
	for _, dev := range devices {
		if claimed[dev.Node] || dev.USBPath == "" {
			continue
		}
		pending = append(pending, dev)
	}
	sort.Slice(pending, func(i, j int) bool {
		return lessUSBPath(pending[i].USBPath, pending[j].USBPath)
	})

	roles := pool.Roles()
	if len(pending) > len(roles) {
		pending = pending[:len(roles)]
	}
	bindings := make([]Binding, 0, len(pending))
	for i, dev := range pending {
		bindings = append(bindings, Binding{Role: roles[i], Device: dev})
	}
	return bindings
}

// lessUSBPath compares two USB topology paths.
//
// [Segments must be compared numerically]. Lexicographic order puts "1-10" before "1-2", so on a machine with
// 10 ports in use slot order wouldn't match physical order, and only once the device count passes 10,
// which test setups almost never reach.
func lessUSBPath(a, b string) bool {
	sa, sb := usbSegments(a), usbSegments(b)
	for i := 0; i < len(sa) && i < len(sb); i++ {
		if sa[i] != sb[i] {
			return sa[i] < sb[i]
		}
	}
	if len(sa) != len(sb) {
		// With an equal prefix the shorter comes first: "1-1" before "1-1.2", the upstream port before the hub ports below it.
		return len(sa) < len(sb)
	}
	// Identical segments (two interfaces on the same port): fall back to string comparison for a total order.
	return a < b
}

// usbSegments splits "1-1.2" into [1, 1, 2].
//
// A segment that doesn't parse as a number counts as -1 instead of being skipped: skipping would misalign "1-a.2" against "1-2"
// and judge two unrelated paths equal.
func usbSegments(path string) []int {
	fields := strings.FieldsFunc(path, func(r rune) bool {
		return r == '-' || r == '.' || r == ':'
	})
	out := make([]int, 0, len(fields))
	for _, field := range fields {
		n, err := strconv.Atoi(field)
		if err != nil {
			n = -1
		}
		out = append(out, n)
	}
	return out
}

// listSysfsDevices enumerates the V4L2 devices in sysfs that can capture.
func listSysfsDevices() ([]Device, error) {
	entries, err := os.ReadDir(sysV4L2Root)
	if err != nil {
		if os.IsNotExist(err) {
			// No video4linux subsystem = no camera driver on this machine.
			// [Not an error]: on a machine without cameras camerad should start normally,
			// just without any endpoints. Returning an error would put it into a restart loop.
			return nil, nil
		}
		return nil, fmt.Errorf("camerad: read %s: %w", sysV4L2Root, err)
	}

	links := deviceLinkIndex()

	var devices []Device
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, "video") {
			continue
		}
		node := filepath.Join("/dev", name)
		if !canCapture(node) {
			// UVC metadata nodes take this path. Letting them in would make half the devices open fine
			// and never produce a frame.
			continue
		}
		devices = append(devices, Device{
			Node:     node,
			USBPath:  usbPathOf(filepath.Join(sysV4L2Root, name)),
			PathLink: links[node],
			Name:     readSysAttr(filepath.Join(sysV4L2Root, name, "name")),
		})
	}

	// Sort by node number so "one role matching several nodes" errors always point at the same pair of devices.
	sort.Slice(devices, func(i, j int) bool { return devices[i].Node < devices[j].Node })
	return devices, nil
}

// usbPathOf works out the USB topology path from a sysfs entry.
//
// /sys/class/video4linux/videoN/device points at a USB interface (such as "1-1.2:1.0"),
// and the part before the colon is the device's topology path. Non-USB devices (MIPI/CSI) don't parse and
// return an empty string; they are located by device_path_link.
func usbPathOf(sysEntry string) string {
	target, err := filepath.EvalSymlinks(filepath.Join(sysEntry, "device"))
	if err != nil {
		return ""
	}
	base := filepath.Base(target)
	if idx := strings.IndexByte(base, ':'); idx >= 0 {
		base = base[:idx]
	}
	// A USB topology path looks like "<bus>-<port>[.<port>...]". No hyphen means it isn't USB.
	if !strings.ContainsRune(base, '-') {
		return ""
	}
	return base
}

// deviceLinkIndex builds a reverse index from /dev/videoN to its by-path link name.
func deviceLinkIndex() map[string]string {
	const byPath = "/dev/v4l/by-path"
	entries, err := os.ReadDir(byPath)
	if err != nil {
		return nil
	}
	index := make(map[string]string, len(entries))
	for _, entry := range entries {
		target, err := filepath.EvalSymlinks(filepath.Join(byPath, entry.Name()))
		if err != nil {
			continue
		}
		// A node may have several links. Take the lexicographically smallest so the mapping is deterministic.
		if prev, ok := index[target]; !ok || entry.Name() < prev {
			index[target] = entry.Name()
		}
	}
	return index
}

func readSysAttr(path string) string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(raw))
}
