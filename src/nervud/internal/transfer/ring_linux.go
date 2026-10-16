//go:build linux

package transfer

import (
	"errors"
	"fmt"
	"net"
	"os"
	"syscall"

	"github.com/nervus-os/nervus-ipc/protocol/ipcv1"
	ipcsdk "github.com/nervus-os/nervus-ipc/sdk"
	"golang.org/x/sys/unix"
)

// The kernel side of SHARED_MEMORY_RING: create a memfd + eventfd and hand them to both ends with SCM_RIGHTS.
//
// The ABI is defined in nervus-ipc's transfer.proto (TransferRingConfig) and sdk/ring.go.
// This file only creates and distributes; it takes no part in the data flow. In ring mode nervud isn't on the hot path,
// which is the whole point of it compared with FRAMED_RELAY.
//
// # Why the kernel must create it rather than the Provider
//
// Letting the Provider create the memfd and hand it to the kernel to forward would let the authorised party decide the size and content of what's authorised.
// With the kernel creating, sealing and distributing it, the geometry both ends get is bound to match what the control plane issued.

// ringSlotCount is the number of slots in the ring.
//
// 8 balances "absorbing jitter" against "memory use": an encoded 1080p stream is tens of KB per frame, and 8 frames
// easily absorb one consumer scheduling delay; raw 4K frames are 12 MiB x 8 = 96 MiB, already more than anything other than
// FRAMED_RELAY should try to carry. It must be a power of two (masked modulo).
const ringSlotCount = 8

// ringResources are the kernel-side resources of one ring transfer.
type ringResources struct {
	memFD   *os.File
	eventFD *os.File
	config  *ipcv1.TransferRingConfig
}

func (r *ringResources) close() {
	if r == nil {
		return
	}
	if r.memFD != nil {
		_ = r.memFD.Close()
	}
	if r.eventFD != nil {
		_ = r.eventFD.Close()
	}
}

// newRingResources creates the memfd and eventfd a ring needs and writes the ring header.
//
// slotSize takes the effective maxPacket; the two must be equal, or a Provider writing a packet of legal size
// would overflow the slot.
func newRingResources(slotSize uint32) (*ringResources, error) {
	geometry := ipcsdk.RingGeometry{
		SlotCount:       ringSlotCount,
		SlotSize:        slotSize,
		HeaderBytes:     ipcsdk.RingHeaderBytes,
		DescriptorBytes: ipcsdk.RingDescriptorBytes,
	}
	total, err := geometry.TotalBytes()
	if err != nil {
		return nil, err
	}

	// MFD_ALLOW_SEALING: once sealed not even the kernel can change the size, so both ends can trust
	// the invariant "mapping length == the geometry the control plane issued"
	fd, err := unix.MemfdCreate("nervud-transfer-ring", unix.MFD_CLOEXEC|unix.MFD_ALLOW_SEALING)
	if err != nil {
		return nil, fmt.Errorf("transfer: memfd_create: %w", err)
	}
	memFD := os.NewFile(uintptr(fd), "nervud-transfer-ring")

	if err := unix.Ftruncate(int(memFD.Fd()), int64(total)); err != nil {
		_ = memFD.Close()
		return nil, fmt.Errorf("transfer: ftruncate ring: %w", err)
	}

	// Write the header first, then seal: after sealing, mmap writes are still allowed (F_SEAL_WRITE would stop them),
	// so only the size is sealed, not writes; both ends need to write their own cursors into the ring
	mem, err := unix.Mmap(int(memFD.Fd()), 0, int(total),
		unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
	if err != nil {
		_ = memFD.Close()
		return nil, fmt.Errorf("transfer: mmap ring: %w", err)
	}
	initErr := ipcsdk.InitHeader(mem, geometry)
	munmapErr := unix.Munmap(mem)
	if initErr != nil {
		_ = memFD.Close()
		return nil, initErr
	}
	if munmapErr != nil {
		_ = memFD.Close()
		return nil, fmt.Errorf("transfer: munmap ring: %w", munmapErr)
	}

	// Only seal SHRINK|GROW: the size is fixed from now on, while both ends still need to write their cursors.
	// Sealing WRITE would stop the consumer from writing even its own consumer_cursor
	if _, err := unix.FcntlInt(memFD.Fd(), unix.F_ADD_SEALS,
		unix.F_SEAL_SHRINK|unix.F_SEAL_GROW|unix.F_SEAL_SEAL); err != nil {
		_ = memFD.Close()
		return nil, fmt.Errorf("transfer: seal ring: %w", err)
	}

	efd, err := unix.Eventfd(0, unix.EFD_CLOEXEC|unix.EFD_NONBLOCK)
	if err != nil {
		_ = memFD.Close()
		return nil, fmt.Errorf("transfer: eventfd: %w", err)
	}

	return &ringResources{
		memFD:   memFD,
		eventFD: os.NewFile(uintptr(efd), "nervud-transfer-ring-event"),
		config: &ipcv1.TransferRingConfig{
			SlotCount:       geometry.SlotCount,
			SlotSize:        geometry.SlotSize,
			HeaderBytes:     geometry.HeaderBytes,
			DescriptorBytes: geometry.DescriptorBytes,
		},
	}, nil
}

// sendRingFDs sends the memfd and eventfd on conn with SCM_RIGHTS.
//
// The order is fixed as memfd, eventfd, as agreed in transfer.proto. The peer receiving a count
// other than 2 must treat it as a protocol violation: a mismatched count means the two sides' understanding of the ABI has diverged.
//
// It must be in the same sendmsg as the one-byte payload: SCM_RIGHTS rides on the datagram,
// and sending them separately could let the peer read the data first and the fds later, or not receive them at all.
func sendRingFDs(conn net.Conn, res *ringResources) error {
	unixConn, ok := conn.(*net.UnixConn)
	if !ok {
		return errors.New("transfer: ring requires a unix connection")
	}
	if res == nil || res.memFD == nil || res.eventFD == nil {
		return errors.New("transfer: ring resources are incomplete")
	}
	rights := unix.UnixRights(int(res.memFD.Fd()), int(res.eventFD.Fd()))
	// A one-byte payload: SCM_RIGHTS can't ride on a zero-length sendmsg
	n, oobn, err := unixConn.WriteMsgUnix([]byte{0}, rights, nil)
	if err != nil {
		return fmt.Errorf("transfer: send ring fds: %w", err)
	}
	if n != 1 || oobn != len(rights) {
		return fmt.Errorf("transfer: partial ring fd send (payload %d, oob %d/%d)",
			n, oobn, len(rights))
	}
	return nil
}

// ringModeSupported reports whether this machine supports every system call ring mode needs.
//
// Probed once rather than tried on every Begin: memfd_create exists from Linux 3.17,
// and F_ADD_SEALS needs MFD_ALLOW_SEALING; both always hold on the target platforms (RK3588 / modern kernels),
// but not necessarily in development containers.
func ringModeSupported() bool {
	fd, err := unix.MemfdCreate("nervud-probe", unix.MFD_CLOEXEC|unix.MFD_ALLOW_SEALING)
	if err != nil {
		return false
	}
	_ = syscall.Close(fd)
	return true
}
