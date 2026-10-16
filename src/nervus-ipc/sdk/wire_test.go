package sdk

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"testing"
)

// frameOf hand-assembles a valid Frame for feeding the reading side.
func frameOf(payload []byte) []byte {
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(len(payload)))
	return append(hdr[:], payload...)
}

func TestWriteFrame_LengthPrefixIsBigEndian(t *testing.T) {
	var buf bytes.Buffer
	if err := writeFrame(&buf, []byte{0xAA, 0xBB, 0xCC}); err != nil {
		t.Fatalf("writeFrame: %v", err)
	}
	got := buf.Bytes()
	// A 3-byte body → the prefix must be 00 00 00 03 (big-endian). Little-endian would write 03 00 00 00,
	// completely mismatching nervud's ReadFrameHeader; this assertion locks the byte order.
	want := []byte{0x00, 0x00, 0x00, 0x03, 0xAA, 0xBB, 0xCC}
	if !bytes.Equal(got, want) {
		t.Fatalf("frame bytes = % x, want % x", got, want)
	}
}

func TestWriteFrame_RejectsZeroAndOversize(t *testing.T) {
	var buf bytes.Buffer
	if err := writeFrame(&buf, nil); !errors.Is(err, ErrZeroLength) {
		t.Errorf("empty payload err = %v, want ErrZeroLength", err)
	}
	if err := writeFrame(&buf, make([]byte, MaxFrameBytes+1)); !errors.Is(err, ErrFrameTooLarge) {
		t.Errorf("oversize payload err = %v, want ErrFrameTooLarge", err)
	}
	// A bug on this side shouldn't send a violating frame to the peer: neither refusal should write any bytes
	if buf.Len() != 0 {
		t.Errorf("rejected frames must not emit bytes, got %d", buf.Len())
	}
}

func TestReadFrameHeader_ZeroLengthIsProtocolError(t *testing.T) {
	r := bytes.NewReader([]byte{0, 0, 0, 0})
	if _, err := readFrameHeader(r); !errors.Is(err, ErrZeroLength) {
		t.Fatalf("err = %v, want ErrZeroLength", err)
	}
}

func TestReadFrameHeader_OversizeRejectedBeforeReadingBody(t *testing.T) {
	// Only the length prefix, without a single byte of body. If the implementation tried to drain the body the peer claims,
	// this would block or report EOF rather than ErrFrameTooLarge, which is exactly the free bandwidth an attacker wants.
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], MaxFrameBytes+1)
	r := bytes.NewReader(hdr[:])
	if _, err := readFrameHeader(r); !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("err = %v, want ErrFrameTooLarge", err)
	}
}

func TestReadFrame_ExactBoundary(t *testing.T) {
	// Exactly 128 KiB must **pass**. Writing the boundary as >= instead of > is the most common bug in this kind of code.
	payload := bytes.Repeat([]byte{0x5A}, MaxFrameBytes)
	r := bytes.NewReader(frameOf(payload))
	buf := make([]byte, MaxFrameBytes)

	n, err := readFrameHeader(r)
	if err != nil {
		t.Fatalf("header at exact boundary: %v", err)
	}
	if n != MaxFrameBytes {
		t.Fatalf("n = %d, want %d", n, MaxFrameBytes)
	}
	body, err := readFrameBody(r, buf, n)
	if err != nil {
		t.Fatalf("body at exact boundary: %v", err)
	}
	if !bytes.Equal(body, payload) {
		t.Error("body mismatch at exact boundary")
	}
}

func TestReadFrame_Coalesced(t *testing.T) {
	// Coalesced packets: one read brings back several Frames. UDS is a byte stream, and read's return boundaries aren't
	// message boundaries: the implementation must split by the length prefix, not assume "one read, one message".
	var stream bytes.Buffer
	want := [][]byte{{1}, {2, 2}, {3, 3, 3}}
	for _, p := range want {
		stream.Write(frameOf(p))
	}

	buf := make([]byte, MaxFrameBytes)
	for i, w := range want {
		n, err := readFrameHeader(&stream)
		if err != nil {
			t.Fatalf("frame %d header: %v", i, err)
		}
		body, err := readFrameBody(&stream, buf, n)
		if err != nil {
			t.Fatalf("frame %d body: %v", i, err)
		}
		if !bytes.Equal(body, w) {
			t.Errorf("frame %d = % x, want % x", i, body, w)
		}
	}
}

// slowReader yields at most 1 byte at a time, simulating partial packets/slow reads.
type slowReader struct{ data []byte }

func (s *slowReader) Read(p []byte) (int, error) {
	if len(s.data) == 0 {
		return 0, io.EOF
	}
	if len(p) == 0 {
		return 0, nil
	}
	p[0] = s.data[0]
	s.data = s.data[1:]
	return 1, nil
}

func TestReadFrame_Fragmented(t *testing.T) {
	// Partial packets: the length prefix and body both arrive split into single bytes. io.ReadFull must assemble them,
	// or it would read a truncated Envelope and decode garbage.
	payload := []byte("fragmented-payload")
	r := &slowReader{data: frameOf(payload)}
	buf := make([]byte, MaxFrameBytes)

	n, err := readFrameHeader(r)
	if err != nil {
		t.Fatalf("header: %v", err)
	}
	body, err := readFrameBody(r, buf, n)
	if err != nil {
		t.Fatalf("body: %v", err)
	}
	if !bytes.Equal(body, payload) {
		t.Fatalf("body = %q, want %q", body, payload)
	}
}

func TestReadFrame_TruncatedBody(t *testing.T) {
	// Claiming 10 bytes but giving only 3: must be an error, never returning those 3 bytes as a complete message.
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], 10)
	r := bytes.NewReader(append(hdr[:], 1, 2, 3))
	buf := make([]byte, MaxFrameBytes)

	n, err := readFrameHeader(r)
	if err != nil {
		t.Fatalf("header: %v", err)
	}
	if _, err := readFrameBody(r, buf, n); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("err = %v, want io.ErrUnexpectedEOF", err)
	}
}

func TestReadFrameHeader_CleanEOF(t *testing.T) {
	// A normal close by the peer must be io.EOF; callers use it to tell "normal disconnect" from "abnormal truncation".
	if _, err := readFrameHeader(bytes.NewReader(nil)); !errors.Is(err, io.EOF) {
		t.Fatalf("err = %v, want io.EOF", err)
	}
}
