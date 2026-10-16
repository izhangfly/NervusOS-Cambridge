package sdk

import (
	"encoding/binary"
	"errors"
	"fmt"
	"sync/atomic"
	"unsafe"

	ipcv1 "github.com/nervus-os/nervus-ipc/protocol/ipcv1"
)

// The user-space implementation of SHARED_MEMORY_RING. The ABI is defined by TransferRingConfig in transfer.proto.
//
// # The core of this file is "trust no number in shared memory"
//
// Both ends can write the ring memory. If the consumer read slot_count / slot_size from the header and computed offsets from them,
// a malicious or buggy producer could make the other side read out of bounds just by rewriting the header, and that would happen in the other side's own
// address space, where nothing can stop it.
//
// So the geometry always comes from the TransferRingConfig sent over the control plane (through nervud, untamperable by the
// peer); the copy in the header is only checked once at attach time, and never read again.

const (
	ringMagic   = "NVR1"
	ringVersion = 1

	// RingHeaderBytes is the ring header length, fixed at one page, so the descriptor area is page aligned.
	RingHeaderBytes = 4096
	// RingDescriptorBytes is the length of each slot descriptor.
	RingDescriptorBytes = 32

	// Header field offsets (little-endian)
	ringOffMagic           = 0
	ringOffVersion         = 4
	ringOffSlotCount       = 8
	ringOffSlotSize        = 12
	ringOffDescriptorBytes = 16
	ringOffProducerCursor  = 24
	ringOffConsumerCursor  = 32

	// Descriptor field offsets (little-endian)
	ringDescOffSequence  = 0
	ringDescOffLength    = 8
	ringDescOffFlags     = 12
	ringDescOffTimestamp = 16
)

var (
	// ErrRingGeometry means the shared memory header disagrees with the geometry sent over the control plane.
	// This is a self-check point, not a negotiable parameter: a mismatch is a protocol violation.
	ErrRingGeometry = errors.New("sdk: shared memory ring geometry does not match the control plane")
	// ErrRingCorrupt means the peer wrote a value this ABI doesn't allow (an out-of-bounds length, an impossible cursor).
	ErrRingCorrupt = errors.New("sdk: shared memory ring contains an invalid value")
	// ErrRingLapped means the consumer was lapped by the producer, and the frames in between were overwritten.
	ErrRingLapped = errors.New("sdk: consumer was lapped by the producer")
	// ErrRingEmpty means there's no readable slot right now.
	ErrRingEmpty = errors.New("sdk: ring is empty")
	// ErrRingFull means the ring is full, and the producer must wait for the consumer to advance.
	ErrRingFull = errors.New("sdk: ring is full")
)

// RingGeometry is the validated ring geometry, all taken from the control plane.
type RingGeometry struct {
	SlotCount       uint32
	SlotSize        uint32
	HeaderBytes     uint32
	DescriptorBytes uint32
}

// ValidateRingConfig checks that the geometry sent over the control plane is self-consistent.
//
// Done **before** mapping any memory: a configuration with slot_count=0 or not a power of 2 would break every later
// mask-based modulo, an error that shows up as randomly reading the wrong slot, very hard to locate.
func ValidateRingConfig(cfg *ipcv1.TransferRingConfig) (RingGeometry, error) {
	if cfg == nil {
		return RingGeometry{}, fmt.Errorf("%w: nil ring config", ErrRingGeometry)
	}
	g := RingGeometry{
		SlotCount:       cfg.GetSlotCount(),
		SlotSize:        cfg.GetSlotSize(),
		HeaderBytes:     cfg.GetHeaderBytes(),
		DescriptorBytes: cfg.GetDescriptorBytes(),
	}
	if g.SlotCount == 0 || g.SlotCount&(g.SlotCount-1) != 0 {
		return RingGeometry{}, fmt.Errorf("%w: slot_count %d is not a power of two",
			ErrRingGeometry, g.SlotCount)
	}
	if g.SlotSize == 0 {
		return RingGeometry{}, fmt.Errorf("%w: slot_size is zero", ErrRingGeometry)
	}
	if g.HeaderBytes != RingHeaderBytes {
		return RingGeometry{}, fmt.Errorf("%w: header_bytes %d, want %d",
			ErrRingGeometry, g.HeaderBytes, RingHeaderBytes)
	}
	if g.DescriptorBytes != RingDescriptorBytes {
		return RingGeometry{}, fmt.Errorf("%w: descriptor_bytes %d, want %d",
			ErrRingGeometry, g.DescriptorBytes, RingDescriptorBytes)
	}
	if _, err := g.TotalBytes(); err != nil {
		return RingGeometry{}, err
	}
	return g, nil
}

// TotalBytes is the ring memory's total length, which must exactly equal the memfd's actual size.
//
// Overflow is checked step by step rather than after computing: on 32-bit, slot_count*slot_size can silently wrap to
// a very small number, and then the mapping succeeds, bounds checks pass, and reads and writes land elsewhere.
func (g RingGeometry) TotalBytes() (uint64, error) {
	payload := uint64(g.SlotCount) * uint64(g.SlotSize)
	descriptors := uint64(g.SlotCount) * uint64(g.DescriptorBytes)
	total := uint64(g.HeaderBytes) + descriptors + payload
	if total < uint64(g.HeaderBytes) || total > 1<<40 {
		return 0, fmt.Errorf("%w: ring size overflow", ErrRingGeometry)
	}
	return total, nil
}

func (g RingGeometry) descriptorOffset(slot uint32) uint64 {
	return uint64(g.HeaderBytes) + uint64(slot)*uint64(g.DescriptorBytes)
}

func (g RingGeometry) payloadOffset(slot uint32) uint64 {
	return uint64(g.HeaderBytes) +
		uint64(g.SlotCount)*uint64(g.DescriptorBytes) +
		uint64(slot)*uint64(g.SlotSize)
}

// Ring is one side's view of a mapped shared memory ring.
//
// **It doesn't hold the fd**: the fd's lifecycle is managed by TransferConn; Ring only operates on the already mmapped
// memory plus a trusted copy of the geometry.
type Ring struct {
	mem      []byte
	geometry RingGeometry
	producer bool
}

// NewRing sets up a ring view over already mapped memory and self-checks the header once.
//
// mem must be exactly geometry.TotalBytes() long. When producer is true this side is the producer
// (writing producer_cursor and payloads), otherwise the consumer.
func NewRing(mem []byte, geometry RingGeometry, producer bool) (*Ring, error) {
	total, err := geometry.TotalBytes()
	if err != nil {
		return nil, err
	}
	if uint64(len(mem)) != total {
		return nil, fmt.Errorf("%w: mapped %d bytes, geometry needs %d",
			ErrRingGeometry, len(mem), total)
	}
	r := &Ring{mem: mem, geometry: geometry, producer: producer}
	if err := r.verifyHeader(); err != nil {
		return nil, err
	}
	return r, nil
}

// verifyHeader compares the geometry copy in the header with the one sent over the control plane.
//
// **It's a self-check, not a source of values.** A mismatch means an implementation bug or trampled memory, and either must stop at once,
// rather than trusting one side and carrying on.
func (r *Ring) verifyHeader() error {
	if string(r.mem[ringOffMagic:ringOffMagic+4]) != ringMagic {
		return fmt.Errorf("%w: bad magic", ErrRingGeometry)
	}
	if v := binary.LittleEndian.Uint32(r.mem[ringOffVersion:]); v != ringVersion {
		return fmt.Errorf("%w: version %d, want %d", ErrRingGeometry, v, ringVersion)
	}
	for _, check := range []struct {
		name   string
		offset int
		want   uint32
	}{
		{"slot_count", ringOffSlotCount, r.geometry.SlotCount},
		{"slot_size", ringOffSlotSize, r.geometry.SlotSize},
		{"descriptor_bytes", ringOffDescriptorBytes, r.geometry.DescriptorBytes},
	} {
		if got := binary.LittleEndian.Uint32(r.mem[check.offset:]); got != check.want {
			return fmt.Errorf("%w: header %s = %d, control plane says %d",
				ErrRingGeometry, check.name, got, check.want)
		}
	}
	return nil
}

// InitHeader is written by the creator (nervud) before handing out fds.
func InitHeader(mem []byte, geometry RingGeometry) error {
	total, err := geometry.TotalBytes()
	if err != nil {
		return err
	}
	if uint64(len(mem)) != total {
		return fmt.Errorf("%w: mapped %d bytes, geometry needs %d",
			ErrRingGeometry, len(mem), total)
	}
	copy(mem[ringOffMagic:], ringMagic)
	binary.LittleEndian.PutUint32(mem[ringOffVersion:], ringVersion)
	binary.LittleEndian.PutUint32(mem[ringOffSlotCount:], geometry.SlotCount)
	binary.LittleEndian.PutUint32(mem[ringOffSlotSize:], geometry.SlotSize)
	binary.LittleEndian.PutUint32(mem[ringOffDescriptorBytes:], geometry.DescriptorBytes)
	binary.LittleEndian.PutUint64(mem[ringOffProducerCursor:], 0)
	binary.LittleEndian.PutUint64(mem[ringOffConsumerCursor:], 0)
	return nil
}

func (r *Ring) cursorPtr(offset int) *uint64 {
	return (*uint64)(unsafe.Pointer(&r.mem[offset]))
}

func (r *Ring) loadProducerCursor() uint64 {
	return atomic.LoadUint64(r.cursorPtr(ringOffProducerCursor))
}

func (r *Ring) loadConsumerCursor() uint64 {
	return atomic.LoadUint64(r.cursorPtr(ringOffConsumerCursor))
}

// Frame is the result of one read. Payload points into ring memory and **becomes invalid after the next Advance**;
// keep it by copying it yourself. That's the price of zero-copy, and its whole point.
type Frame struct {
	Payload   []byte
	Sequence  uint64
	Flags     uint32
	Timestamp uint64
}

// Write writes one frame into the ring. Producer only.
//
// Order is the key to this ABI: write the payload and metadata first, and write sequence last with release semantics.
// The other way round, the consumer could read a slot that's "ready" but whose payload isn't fully written yet.
func (r *Ring) Write(payload []byte, flags uint32, timestampNanos uint64) error {
	if !r.producer {
		return errors.New("sdk: Write on a consumer ring")
	}
	if uint32(len(payload)) > r.geometry.SlotSize {
		return fmt.Errorf("%w: payload %d exceeds slot size %d",
			ErrRingCorrupt, len(payload), r.geometry.SlotSize)
	}

	produced := r.loadProducerCursor()
	consumed := r.loadConsumerCursor()
	// The peer's cursor is untrusted: if the consumer wrote a future value, produced-consumed would wrap into a huge
	// number. Only refuse when it "looks legal and full"; treating other cases as full is safer
	if produced-consumed >= uint64(r.geometry.SlotCount) {
		return ErrRingFull
	}

	slot := uint32(produced & uint64(r.geometry.SlotCount-1))
	desc := r.mem[r.geometry.descriptorOffset(slot):]

	// Odd = being written. A consumer reading an odd value skips this slot
	seq := produced*2 + 1
	atomic.StoreUint64((*uint64)(unsafe.Pointer(&desc[ringDescOffSequence])), seq)

	payloadStart := r.geometry.payloadOffset(slot)
	copy(r.mem[payloadStart:payloadStart+uint64(r.geometry.SlotSize)], payload)
	binary.LittleEndian.PutUint32(desc[ringDescOffLength:], uint32(len(payload)))
	binary.LittleEndian.PutUint32(desc[ringDescOffFlags:], flags)
	binary.LittleEndian.PutUint64(desc[ringDescOffTimestamp:], timestampNanos)

	// release: even = ready. Must come after the payload and metadata are fully written
	atomic.StoreUint64((*uint64)(unsafe.Pointer(&desc[ringDescOffSequence])), seq+1)
	atomic.StoreUint64(r.cursorPtr(ringOffProducerCursor), produced+1)
	return nil
}

// Read takes the next frame. Consumer only.
//
// ErrRingEmpty means no data yet; ErrRingLapped means it was lapped, and the caller should
// SkipToLatest and retry: the frames in between were overwritten, and reading them would only give torn content.
func (r *Ring) Read() (Frame, error) {
	if r.producer {
		return Frame{}, errors.New("sdk: Read on a producer ring")
	}
	consumed := r.loadConsumerCursor()
	produced := r.loadProducerCursor()
	if produced == consumed {
		return Frame{}, ErrRingEmpty
	}

	// **The peer's cursor is untrusted.** produced behind consumed means the producer wrote an illegal value
	available := produced - consumed
	if available > uint64(r.geometry.SlotCount) {
		return Frame{}, ErrRingLapped
	}

	slot := uint32(consumed & uint64(r.geometry.SlotCount-1))
	desc := r.mem[r.geometry.descriptorOffset(slot):]

	// seqlock: set odd before writing (being written), even after writing (ready).
	// Frame N's ready value is 2N+2, so 0 means this slot was never written.
	seq := atomic.LoadUint64((*uint64)(unsafe.Pointer(&desc[ringDescOffSequence])))
	if seq == 0 || seq%2 != 0 {
		return Frame{}, ErrRingEmpty
	}

	length := binary.LittleEndian.Uint32(desc[ringDescOffLength:])
	// **It must be checked here, without clamping.** Clamping and reading on would turn a bug into silent
	// data corruption: the caller getting data of the right length but wrong content is far harder to diagnose than getting an error
	if length > r.geometry.SlotSize {
		return Frame{}, fmt.Errorf("%w: slot %d declares length %d > slot size %d",
			ErrRingCorrupt, slot, length, r.geometry.SlotSize)
	}

	payloadStart := r.geometry.payloadOffset(slot)
	frame := Frame{
		Payload: r.mem[payloadStart : payloadStart+uint64(length)],
		// Ready value 2N+2 → frame number N
		Sequence:  seq/2 - 1,
		Flags:     binary.LittleEndian.Uint32(desc[ringDescOffFlags:]),
		Timestamp: binary.LittleEndian.Uint64(desc[ringDescOffTimestamp:]),
	}

	// Re-check sequence after reading: if the producer overwrote it midway, the two reads differ,
	// meaning what we got is torn content
	if after := atomic.LoadUint64(
		(*uint64)(unsafe.Pointer(&desc[ringDescOffSequence]))); after != seq {
		return Frame{}, ErrRingLapped
	}
	return frame, nil
}

// Advance confirms the previous frame has been consumed and advances the consumer cursor.
//
// It's separate from Read on purpose: Frame.Payload points into ring memory, and after advancing the producer may overwrite it at any time.
// So callers can finish using (or copy) the data before advancing.
func (r *Ring) Advance() {
	if r.producer {
		return
	}
	atomic.StoreUint64(r.cursorPtr(ringOffConsumerCursor), r.loadConsumerCursor()+1)
}

// SkipToLatest jumps the consumer cursor to the start of the latest lap after being lapped.
//
// For video streams the right behaviour is to drop the backlog and look at the latest frame, rather than reading torn content
// out of a batch of overwritten slots.
func (r *Ring) SkipToLatest() {
	if r.producer {
		return
	}
	produced := r.loadProducerCursor()
	target := produced
	if produced > uint64(r.geometry.SlotCount) {
		target = produced - uint64(r.geometry.SlotCount) + 1
	}
	atomic.StoreUint64(r.cursorPtr(ringOffConsumerCursor), target)
}
