package sdk

import (
	"bytes"
	"encoding/binary"
	"errors"
	"testing"

	ipcv1 "github.com/nervus-os/nervus-ipc/protocol/ipcv1"
)

func testGeometry() RingGeometry {
	return RingGeometry{
		SlotCount:       4,
		SlotSize:        64,
		HeaderBytes:     RingHeaderBytes,
		DescriptorBytes: RingDescriptorBytes,
	}
}

// newRingPair creates a piece of shared memory and opens producer and consumer views on it,
// simulating two processes mmapping the same memfd.
func newRingPair(t *testing.T) (producer, consumer *Ring, mem []byte) {
	t.Helper()
	g := testGeometry()
	total, err := g.TotalBytes()
	if err != nil {
		t.Fatalf("TotalBytes: %v", err)
	}
	mem = make([]byte, total)
	if err := InitHeader(mem, g); err != nil {
		t.Fatalf("InitHeader: %v", err)
	}
	if producer, err = NewRing(mem, g, true); err != nil {
		t.Fatalf("NewRing producer: %v", err)
	}
	if consumer, err = NewRing(mem, g, false); err != nil {
		t.Fatalf("NewRing consumer: %v", err)
	}
	return producer, consumer, mem
}

func TestRing_WriteReadRoundTrip(t *testing.T) {
	producer, consumer, _ := newRingPair(t)

	payload := []byte("frame-0")
	if err := producer.Write(payload, 0x11, 12345); err != nil {
		t.Fatalf("Write: %v", err)
	}
	frame, err := consumer.Read()
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if !bytes.Equal(frame.Payload, payload) {
		t.Fatalf("payload = %q, want %q", frame.Payload, payload)
	}
	if frame.Flags != 0x11 || frame.Timestamp != 12345 || frame.Sequence != 0 {
		t.Fatalf("frame = %+v", frame)
	}
	consumer.Advance()
	if _, err := consumer.Read(); !errors.Is(err, ErrRingEmpty) {
		t.Fatalf("Read after Advance = %v, want ErrRingEmpty", err)
	}
}

// Frame numbers must start at 0 and be contiguous. The seqlock's ready value is 2N+2, so a one-bit decoding mistake shifts everything,
// and that kind of error only shows when comparing across frames.
func TestRing_SequenceIsContiguousFromZero(t *testing.T) {
	producer, consumer, _ := newRingPair(t)

	for round := 0; round < 3; round++ {
		for i := 0; i < int(testGeometry().SlotCount); i++ {
			want := uint64(round*int(testGeometry().SlotCount) + i)
			if err := producer.Write([]byte{byte(want)}, 0, 0); err != nil {
				t.Fatalf("Write %d: %v", want, err)
			}
			frame, err := consumer.Read()
			if err != nil {
				t.Fatalf("Read %d: %v", want, err)
			}
			if frame.Sequence != want {
				t.Fatalf("frame %d Sequence = %d", want, frame.Sequence)
			}
			consumer.Advance()
		}
	}
}

func TestRing_FullAndDrain(t *testing.T) {
	producer, consumer, _ := newRingPair(t)
	g := testGeometry()

	for i := 0; i < int(g.SlotCount); i++ {
		if err := producer.Write([]byte{byte(i)}, 0, 0); err != nil {
			t.Fatalf("Write %d: %v", i, err)
		}
	}
	if err := producer.Write([]byte{9}, 0, 0); !errors.Is(err, ErrRingFull) {
		t.Fatalf("write %d = %v, want ErrRingFull", g.SlotCount+1, err)
	}

	for i := 0; i < int(g.SlotCount); i++ {
		frame, err := consumer.Read()
		if err != nil {
			t.Fatalf("Read %d: %v", i, err)
		}
		if len(frame.Payload) != 1 || frame.Payload[0] != byte(i) {
			t.Fatalf("frame %d = %v", i, frame.Payload)
		}
		consumer.Advance()
	}
	// Writable again after draining
	if err := producer.Write([]byte{9}, 0, 0); err != nil {
		t.Fatalf("write after draining failed: %v", err)
	}
}

// **Core safety assertion**: when a malicious producer declares a length beyond the slot, the consumer must refuse,
// not clamp and keep reading. Clamping would turn a bug into silent data corruption.
func TestRing_RejectsOversizedPayloadLength(t *testing.T) {
	producer, consumer, mem := newRingPair(t)
	g := testGeometry()

	if err := producer.Write([]byte("ok"), 0, 0); err != nil {
		t.Fatalf("Write: %v", err)
	}
	// Rewrite the length in the descriptor directly, simulating a malicious peer
	descOff := g.descriptorOffset(0)
	binary.LittleEndian.PutUint32(mem[descOff+ringDescOffLength:], g.SlotSize+1)

	_, err := consumer.Read()
	if !errors.Is(err, ErrRingCorrupt) {
		t.Fatalf("Read = %v, want ErrRingCorrupt", err)
	}
}

// The producer can't write an oversized payload either.
func TestRing_WriteRejectsOversizedPayload(t *testing.T) {
	producer, _, _ := newRingPair(t)
	if err := producer.Write(make([]byte, testGeometry().SlotSize+1), 0, 0); !errors.Is(
		err, ErrRingCorrupt,
	) {
		t.Fatalf("Write oversized = %v, want ErrRingCorrupt", err)
	}
}

// A slot the producer is writing (odd sequence) must be skipped; a half-written frame must never be read.
func TestRing_SkipsInFlightSlot(t *testing.T) {
	producer, consumer, mem := newRingPair(t)
	if err := producer.Write([]byte("x"), 0, 0); err != nil {
		t.Fatalf("Write: %v", err)
	}
	// Make the sequence odd, simulating "being written"
	descOff := testGeometry().descriptorOffset(0)
	binary.LittleEndian.PutUint64(mem[descOff+ringDescOffSequence:], 1)

	if _, err := consumer.Read(); !errors.Is(err, ErrRingEmpty) {
		t.Fatalf("Read in-flight slot = %v, want ErrRingEmpty", err)
	}
}

// **The peer's cursor isn't trusted**: when the producer writes an impossible cursor, the consumer must report lapping rather than
// compute an out-of-bounds slot from it.
func TestRing_RejectsImpossibleProducerCursor(t *testing.T) {
	_, consumer, mem := newRingPair(t)
	binary.LittleEndian.PutUint64(mem[ringOffProducerCursor:], 1<<40)

	if _, err := consumer.Read(); !errors.Is(err, ErrRingLapped) {
		t.Fatalf("Read = %v, want ErrRingLapped", err)
	}
}

// After being lapped, skip to the latest lap rather than read torn content from overwritten slots.
func TestRing_SkipToLatest(t *testing.T) {
	producer, consumer, _ := newRingPair(t)
	g := testGeometry()

	// Fill two laps without the consumer advancing
	for i := 0; i < int(g.SlotCount); i++ {
		if err := producer.Write([]byte{byte(i)}, 0, 0); err != nil {
			t.Fatalf("Write %d: %v", i, err)
		}
	}
	consumer.SkipToLatest()
	// No lapping should be reported after skipping
	if _, err := consumer.Read(); errors.Is(err, ErrRingLapped) {
		t.Fatalf("still reports lapping after SkipToLatest")
	}
}

// Geometry in the header that disagrees with the control plane = a protocol violation, not a negotiable parameter.
func TestRing_HeaderMustMatchControlPlane(t *testing.T) {
	g := testGeometry()
	total, _ := g.TotalBytes()
	mem := make([]byte, total)
	if err := InitHeader(mem, g); err != nil {
		t.Fatalf("InitHeader: %v", err)
	}
	// Tamper with slot_size in the header
	binary.LittleEndian.PutUint32(mem[ringOffSlotSize:], g.SlotSize*2)

	if _, err := NewRing(mem, g, false); !errors.Is(err, ErrRingGeometry) {
		t.Fatalf("NewRing = %v, want ErrRingGeometry", err)
	}
}

func TestRing_RejectsBadMagicAndVersion(t *testing.T) {
	g := testGeometry()
	total, _ := g.TotalBytes()

	bad := make([]byte, total)
	_ = InitHeader(bad, g)
	copy(bad[ringOffMagic:], "XXXX")
	if _, err := NewRing(bad, g, false); !errors.Is(err, ErrRingGeometry) {
		t.Fatalf("bad magic = %v, want ErrRingGeometry", err)
	}

	badVersion := make([]byte, total)
	_ = InitHeader(badVersion, g)
	binary.LittleEndian.PutUint32(badVersion[ringOffVersion:], 99)
	if _, err := NewRing(badVersion, g, false); !errors.Is(err, ErrRingGeometry) {
		t.Fatalf("bad version = %v, want ErrRingGeometry", err)
	}
}

// A mapping length that doesn't match the geometry must be refused: it means the memfd size disagrees with what the control plane sent.
func TestRing_RejectsWrongMappingSize(t *testing.T) {
	g := testGeometry()
	total, _ := g.TotalBytes()
	mem := make([]byte, total)
	_ = InitHeader(mem, g)

	if _, err := NewRing(mem[:total-1], g, false); !errors.Is(err, ErrRingGeometry) {
		t.Fatalf("short mapping = %v, want ErrRingGeometry", err)
	}
}

// The control plane parameters themselves must be self-consistent: a slot_count that isn't a power of 2 breaks the mask modulo,
// with the symptom of randomly reading the wrong slot, extremely hard to track down.
func TestValidateRingConfig(t *testing.T) {
	base := func() *ipcv1.TransferRingConfig {
		return &ipcv1.TransferRingConfig{
			SlotCount: 4, SlotSize: 64,
			HeaderBytes: RingHeaderBytes, DescriptorBytes: RingDescriptorBytes,
		}
	}
	if _, err := ValidateRingConfig(base()); err != nil {
		t.Fatalf("valid config refused: %v", err)
	}

	for _, tc := range []struct {
		name   string
		mutate func(*ipcv1.TransferRingConfig)
	}{
		{"slot_count not a power of 2", func(c *ipcv1.TransferRingConfig) { c.SlotCount = 3 }},
		{"slot_count is 0", func(c *ipcv1.TransferRingConfig) { c.SlotCount = 0 }},
		{"slot_size is 0", func(c *ipcv1.TransferRingConfig) { c.SlotSize = 0 }},
		{"header_bytes wrong", func(c *ipcv1.TransferRingConfig) { c.HeaderBytes = 8192 }},
		{"descriptor_bytes wrong", func(c *ipcv1.TransferRingConfig) { c.DescriptorBytes = 64 }},
		{"nil", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var cfg *ipcv1.TransferRingConfig
			if tc.mutate != nil {
				cfg = base()
				tc.mutate(cfg)
			}
			if _, err := ValidateRingConfig(cfg); !errors.Is(err, ErrRingGeometry) {
				t.Fatalf("err = %v, want ErrRingGeometry", err)
			}
		})
	}
}

// Roles can't be mixed: the consumer can't write and the producer can't read.
func TestRing_RoleIsEnforced(t *testing.T) {
	producer, consumer, _ := newRingPair(t)
	if err := consumer.Write([]byte("x"), 0, 0); err == nil {
		t.Error("consumer write was allowed")
	}
	if _, err := producer.Read(); err == nil {
		t.Error("producer read was allowed")
	}
}
