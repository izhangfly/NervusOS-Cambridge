//go:build linux

package sdk

import (
	"bytes"
	"errors"
	"os"
	"testing"

	ipcv1 "github.com/nervus-os/nervus-ipc/protocol/ipcv1"
	"golang.org/x/sys/unix"
)

// End to end: really create the memfd, really mmap, really send and receive across mappings.
//
// The earlier ring_test.go simulates shared memory with an ordinary []byte, testing the ABI logic;
// this file tests that **the system call layer really works**: the memfd can be created, ftruncated and sealed,
// and two independent mmaps see the same physical memory.
func TestRingOverRealMemfd(t *testing.T) {
	g := RingGeometry{
		SlotCount: 4, SlotSize: 128,
		HeaderBytes: RingHeaderBytes, DescriptorBytes: RingDescriptorBytes,
	}
	total, err := g.TotalBytes()
	if err != nil {
		t.Fatalf("TotalBytes: %v", err)
	}

	fd, err := unix.MemfdCreate("ring-e2e", unix.MFD_CLOEXEC|unix.MFD_ALLOW_SEALING)
	if err != nil {
		t.Skipf("memfd_create unavailable: %v", err)
	}
	memFile := os.NewFile(uintptr(fd), "ring-e2e")
	defer func() { _ = memFile.Close() }()

	if err := unix.Ftruncate(int(memFile.Fd()), int64(total)); err != nil {
		t.Fatalf("Ftruncate: %v", err)
	}

	// Two independent mappings, simulating two processes each mmapping the same memfd
	producerMem, err := unix.Mmap(int(memFile.Fd()), 0, int(total),
		unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
	if err != nil {
		t.Fatalf("mmap producer: %v", err)
	}
	defer func() { _ = unix.Munmap(producerMem) }()

	consumerMem, err := unix.Mmap(int(memFile.Fd()), 0, int(total),
		unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
	if err != nil {
		t.Fatalf("mmap consumer: %v", err)
	}
	defer func() { _ = unix.Munmap(consumerMem) }()

	if err := InitHeader(producerMem, g); err != nil {
		t.Fatalf("InitHeader: %v", err)
	}
	producer, err := NewRing(producerMem, g, true)
	if err != nil {
		t.Fatalf("NewRing producer: %v", err)
	}
	consumer, err := NewRing(consumerMem, g, false)
	if err != nil {
		t.Fatalf("NewRing consumer: %v", err)
	}

	// Send and receive across two independent mappings: content written into producerMem must appear in consumerMem
	for i := 0; i < 16; i++ {
		payload := bytes.Repeat([]byte{byte(i)}, 64)
		if err := producer.Write(payload, uint32(i), uint64(i)*1000); err != nil {
			t.Fatalf("Write %d: %v", i, err)
		}
		frame, err := consumer.Read()
		if err != nil {
			t.Fatalf("Read %d: %v", i, err)
		}
		if !bytes.Equal(frame.Payload, payload) {
			t.Fatalf("frame %d content mismatch", i)
		}
		if frame.Sequence != uint64(i) || frame.Flags != uint32(i) {
			t.Fatalf("frame %d metadata = %+v", i, frame)
		}
		consumer.Advance()
	}
}

// After sealing the size can't change, so both ends can trust "mapping length == control plane geometry".
func TestRingMemfdSealPreventsResize(t *testing.T) {
	fd, err := unix.MemfdCreate("ring-seal", unix.MFD_CLOEXEC|unix.MFD_ALLOW_SEALING)
	if err != nil {
		t.Skipf("memfd_create unavailable: %v", err)
	}
	f := os.NewFile(uintptr(fd), "ring-seal")
	defer func() { _ = f.Close() }()

	if err := unix.Ftruncate(int(f.Fd()), 8192); err != nil {
		t.Fatalf("Ftruncate: %v", err)
	}
	if _, err := unix.FcntlInt(f.Fd(), unix.F_ADD_SEALS,
		unix.F_SEAL_SHRINK|unix.F_SEAL_GROW|unix.F_SEAL_SEAL); err != nil {
		t.Fatalf("F_ADD_SEALS: %v", err)
	}
	if err := unix.Ftruncate(int(f.Fd()), 4096); err == nil {
		t.Fatal("memfd could still shrink after sealing")
	}
	if err := unix.Ftruncate(int(f.Fd()), 16384); err == nil {
		t.Fatal("memfd could still grow after sealing")
	}
}

// SCM_RIGHTS passes two fds, taken back in a fixed order. A mismatched count must be an error:
// a mismatched count means the two sides' understanding of the ABI has diverged.
func TestRingDescriptorHandover(t *testing.T) {
	pair, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatalf("Socketpair: %v", err)
	}
	sendFile := os.NewFile(uintptr(pair[0]), "send")
	recvFile := os.NewFile(uintptr(pair[1]), "recv")
	defer func() { _ = sendFile.Close(); _ = recvFile.Close() }()

	memFD, err := unix.MemfdCreate("ring-fd", unix.MFD_CLOEXEC)
	if err != nil {
		t.Skipf("memfd_create unavailable: %v", err)
	}
	defer func() { _ = unix.Close(memFD) }()
	eventFD, err := unix.Eventfd(0, unix.EFD_CLOEXEC|unix.EFD_NONBLOCK)
	if err != nil {
		t.Fatalf("Eventfd: %v", err)
	}
	defer func() { _ = unix.Close(eventFD) }()

	rights := unix.UnixRights(memFD, eventFD)
	if err := unix.Sendmsg(pair[0], []byte{0}, rights, nil, 0); err != nil {
		t.Fatalf("Sendmsg: %v", err)
	}

	oob := make([]byte, unix.CmsgSpace(2*4))
	payload := make([]byte, 1)
	_, oobn, _, _, err := unix.Recvmsg(pair[1], payload, oob, 0)
	if err != nil {
		t.Fatalf("Recvmsg: %v", err)
	}

	fds, err := parseUnixRights(oob[:oobn])
	if err != nil {
		t.Fatalf("parseUnixRights: %v", err)
	}
	defer closeFDs(fds)
	if len(fds) != 2 {
		t.Fatalf("received %d fds, want 2", len(fds))
	}
	// The received fd must really be usable: fstat the memfd once
	var st unix.Stat_t
	if err := unix.Fstat(fds[0], &st); err != nil {
		t.Fatalf("received memfd unusable: %v", err)
	}
}

// With no control message it must report a protocol error, not carry on as "zero fds".
func TestParseUnixRightsRejectsEmpty(t *testing.T) {
	if _, err := parseUnixRights(nil); !errors.Is(err, ErrProtocol) {
		t.Fatalf("err = %v, want ErrProtocol", err)
	}
}

// Invalid geometry shouldn't get as far as mapping.
func TestAttachRingRejectsBadGeometry(t *testing.T) {
	c := &TransferConn{role: ipcv1.TransferRole_TRANSFER_ROLE_CONSUMER}
	err := c.attachRing(nil, &ipcv1.TransferRingConfig{
		SlotCount: 3, SlotSize: 64, // 3 isn't a power of 2
		HeaderBytes: RingHeaderBytes, DescriptorBytes: RingDescriptorBytes,
	})
	if !errors.Is(err, ErrRingGeometry) {
		t.Fatalf("err = %v, want ErrRingGeometry", err)
	}
}
