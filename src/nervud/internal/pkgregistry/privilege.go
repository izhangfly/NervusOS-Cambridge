// This file is the allowlist of Linux-level privileges a Component may request: capabilities and socket
// address families.
//
// # Why these two exist
//
// Components run as App UIDs (20000-59999), not root, and the sandbox grants no capabilities.
// Operations like driving radios (Bluetooth rfkill, hci up) or configuring networks therefore all get EPERM.
//
// Running components as root isn't an option: the ipc handshake refuses root peers with CheckUID
// (internal/ipc/ipc.go), so a component running as root couldn't even reach the control plane; it would no longer be a
// Nervus service, just an ordinary daemon running on the system. Capabilities are the only way that doesn't break
// the identity model.
//
// # What the allowlist stops here
//
// Not policy filtering: v1 includes every Linux capability, and a system service fills in whichever it wants.
// It stops two more basic things:
//
//	typos       "CAP_NET_ADMN" would be silently ignored by systemd, and the component would start normally, get EPERM normally,
//	            with no clue in the logs. Here loading is refused outright, with the error pointing at that exact string
//	injection   the values of these two fields go into systemd unit properties unchanged, and not checking them would let a manifest
//	            put arbitrary strings into the D-Bus call
//
// # The real constraint is elsewhere
//
// Only packages from the system image get these declarations (see service.buildStartReq). Dynamically
// installed packages that fill them in are ignored: handing capabilities to an arbitrary third-party package means the sandbox doesn't exist.
package pkgregistry

import (
	"fmt"
	"slices"
	"strings"
)

// allowedCapabilities are the capabilities a Component may request: the full Linux set.
//
// Ordered as in capabilities(7), for easy comparison with the man page.
//
// The ones marked WARNING are equivalent to root: a process holding any of them can turn itself into root,
// or rewrite kernel behaviour directly. Think hard about whether the package deserves it before granting; they aren't refused here,
// but shouldn't be treated as "a little more permission" either.
var allowedCapabilities = map[string]struct{}{
	// ---- Files and ownership ----
	"CAP_CHOWN":           {}, // change the owner of any file
	"CAP_DAC_OVERRIDE":    {}, // WARNING bypass all file read/write permission checks
	"CAP_DAC_READ_SEARCH": {}, // bypass file read and directory search permissions
	"CAP_FOWNER":          {}, // bypass "must be the owner" checks
	"CAP_FSETID":          {}, // don't clear setuid/setgid bits when changing files
	"CAP_LINUX_IMMUTABLE": {}, // set immutable / append-only attributes
	"CAP_MKNOD":           {}, // WARNING create device nodes: you could create /dev/sda and read the raw disk
	"CAP_LEASE":           {}, // take leases on files
	"CAP_SETFCAP":         {}, // WARNING set capabilities on files, usable for persistent privilege escalation

	// ---- Processes and identity ----
	"CAP_KILL":       {}, // send signals to any process
	"CAP_SETGID":     {}, // WARNING become any GID
	"CAP_SETUID":     {}, // WARNING become any UID, root included
	"CAP_SETPCAP":    {}, // WARNING change your own / others' capability sets
	"CAP_SYS_PTRACE": {}, // WARNING ptrace any process: can read other packages' memory

	// ---- Network and radio ----
	"CAP_NET_ADMIN":        {}, // network interface configuration, rfkill, hci up/down (needed to switch Bluetooth)
	"CAP_NET_RAW":          {}, // raw sockets: HCI raw, ICMP, packet capture
	"CAP_NET_BIND_SERVICE": {}, // bind ports below 1024
	"CAP_NET_BROADCAST":    {}, // broadcast and multicast

	// ---- IPC ----
	"CAP_IPC_LOCK":  {}, // mlock / mlockall
	"CAP_IPC_OWNER": {}, // bypass System V IPC permission checks

	// ---- System ----
	"CAP_SYS_ADMIN":      {}, // WARNING "the new root": mount, namespaces, quotas and a large batch of operations with no clear boundary
	"CAP_SYS_MODULE":     {}, // WARNING load/unload kernel modules, which amounts to rewriting the kernel
	"CAP_SYS_RAWIO":      {}, // WARNING direct I/O ports and /dev/mem
	"CAP_SYS_CHROOT":     {}, // chroot
	"CAP_SYS_PACCT":      {}, // switch process accounting
	"CAP_SYS_BOOT":       {}, // reboot / kexec
	"CAP_SYS_NICE":       {}, // real-time priority and nice
	"CAP_SYS_RESOURCE":   {}, // exceed resource limits
	"CAP_SYS_TIME":       {}, // change the system clock
	"CAP_SYS_TTY_CONFIG": {}, // tty configuration and vhangup

	// ---- Audit and observability ----
	"CAP_AUDIT_WRITE":   {}, // write records to the audit log
	"CAP_AUDIT_CONTROL": {}, // WARNING change audit rules: can remove its own actions from the audit
	"CAP_AUDIT_READ":    {}, // read the audit log
	"CAP_SYSLOG":        {}, // read the kernel ring buffer, control dmesg

	// ---- Mandatory access control ----
	"CAP_MAC_OVERRIDE": {}, // WARNING bypass MAC (SELinux/AppArmor)
	"CAP_MAC_ADMIN":    {}, // WARNING change MAC policy

	// ---- Power and newer features ----
	"CAP_WAKE_ALARM":         {}, // set wake-up timers
	"CAP_BLOCK_SUSPEND":      {}, // block system suspend
	"CAP_PERFMON":            {}, // performance monitoring
	"CAP_BPF":                {}, // WARNING load BPF programs
	"CAP_CHECKPOINT_RESTORE": {}, // CRIU checkpoint/restore
}

// allowedAddressFamilies are the socket address families a Component may additionally ask to be allowed.
//
// AF_UNIX / AF_INET / AF_INET6 are the baseline every component has unconditionally (see
// systemd.BuildProperties) and aren't in this table: making the baseline declarable would only make
// every manifest copy it.
//
// This wall has nothing to do with capabilities: RestrictAddressFamilies is at the seccomp layer,
// and no amount of capabilities gets round it. With capabilities but no address family, Bluetooth still won't open,
// and the error looks like "protocol not supported" (EAFNOSUPPORT), nothing to do with permissions.
var allowedAddressFamilies = map[string]struct{}{
	"AF_BLUETOOTH": {}, // Bluetooth: HCI / L2CAP / RFCOMM
	"AF_CAN":       {}, // CAN bus: a common channel for robot actuators
	"AF_NETLINK":   {}, // kernel events and network configuration (rfkill goes through netlink too)
	"AF_PACKET":    {}, // raw link-layer frames
	"AF_ALG":       {}, // kernel crypto interface
	"AF_VSOCK":     {}, // virtual machine host/guest channel
	"AF_XDP":       {}, // high-performance packet receive
}

// EffectiveCapabilities gives the capability set that finally takes effect for a Component.
//
// Privileged expands to the full set; otherwise it's what it declared itself.
//
// It returns a sorted slice rather than map order: it goes into systemd unit properties and into audit
// logs, and an unstable order would make two identical starts look different, with diffs full of noise.
func EffectiveCapabilities(c Component) []string {
	if c.Privileged {
		return sortedKeys(allowedCapabilities)
	}
	if len(c.Capabilities) == 0 {
		return nil
	}
	return append([]string(nil), c.Capabilities...)
}

// EffectiveAddressFamilies gives the extra address families that finally take effect for a Component.
//
// Excludes AF_UNIX/AF_INET/AF_INET6: those three are every component's baseline, added unconditionally
// by the systemd layer.
func EffectiveAddressFamilies(c Component) []string {
	if c.Privileged {
		return sortedKeys(allowedAddressFamilies)
	}
	if len(c.AddressFamilies) == 0 {
		return nil
	}
	return append([]string(nil), c.AddressFamilies...)
}

func sortedKeys(m map[string]struct{}) []string {
	out := keysOf(m)
	slices.Sort(out)
	return out
}

// validateComponentPrivileges checks the capabilities and address families a Component requests.
//
// Privileged needs no check: what it expands to is this file's allowlist itself, so it can't go out of bounds.
func validateComponentPrivileges(c Component) error {
	for _, capName := range c.Capabilities {
		if _, ok := allowedCapabilities[capName]; !ok {
			return fmt.Errorf("%w: component %q requests unknown capability %q",
				ErrInvalidCapability, c.ID, capName)
		}
	}
	for _, af := range c.AddressFamilies {
		if _, ok := allowedAddressFamilies[af]; !ok {
			return fmt.Errorf("%w: component %q requests unknown address family %q",
				ErrInvalidAddressFamily, c.ID, af)
		}
	}
	return nil
}

// KnownCapabilities returns every capability on the allowlist, for diagnostics and configuration tools.
// The order isn't guaranteed; callers wanting stable output must sort it themselves.
func KnownCapabilities() []string { return keysOf(allowedCapabilities) }

// KnownAddressFamilies returns every address family on the allowlist.
func KnownAddressFamilies() []string { return keysOf(allowedAddressFamilies) }

func keysOf(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// DescribePrivileges gives a one-line summary for audit.
func DescribePrivileges(caps, afs []string) string {
	var b strings.Builder
	if len(caps) > 0 {
		b.WriteString("caps=")
		b.WriteString(strings.Join(caps, ","))
	}
	if len(afs) > 0 {
		if b.Len() > 0 {
			b.WriteByte(' ')
		}
		b.WriteString("af=")
		b.WriteString(strings.Join(afs, ","))
	}
	return b.String()
}
