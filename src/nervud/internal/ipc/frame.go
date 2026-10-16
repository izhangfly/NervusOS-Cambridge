// This file is the framing layer: reading and writing the byte layout of control-plane Frames.
//
// Frame layout: 4-byte big-endian uint32 N, followed by N bytes of Protobuf Envelope.
//
// This layer doesn't decode Envelopes or know about endpoints/methods; it only cuts the byte stream into complete
// Envelope byte blocks. That layering lets it be fully tested over net.Pipe, without a real socket
package ipc

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// MaxFrameBytes is the absolute hard limit of a whole control-plane Frame:
// 128 KiB = 131072 bytes, not an ambiguous "128 kb".
//
// This is the Frame limit, not a method quota: ordinary RPCs default to 16 KiB, Safety methods 4 KiB,
// configuration/manifests 64 KiB, declared per method in the Registry, with the layer above taking the smallest of the three.
// System Services can't bypass this limit either.
//
// This value must match ConnectionLimits.max_frame_bytes sent to clients at the handshake
const MaxFrameBytes = 128 << 10

// lengthPrefixBytes is the size of the length prefix itself. N doesn't include these 4 bytes
const lengthPrefixBytes = 4

// Every error in this layer is a protocol violation: the connection is closed on sight, with no Response produced.
//
// The reason: once the length field can't be trusted, later Frame boundaries can't be determined, the byte stream on this connection
// has lost its meaning, and an error response would have nowhere to go; it too would need a length prefix to be read
var (
	ErrZeroLength    = errors.New("ipc: frame length is zero")
	ErrFrameTooLarge = errors.New("ipc: frame length exceeds hard limit")
)

// ReadFrameHeader reads the full 4-byte length prefix, checks it, and returns the body length N.
//
// It must be called in a pair with ReadFrameBody, and the moment in between is the only time to set the body deadline,
// which is the whole reason this layer is split into two functions:
//
//	idle deadline   waiting for the next Frame to arrive; can be long (kept alive by Ping/Pong)
//	body deadline   N bytes have been announced as on their way; must be short
//
// Covering both with one deadline would hand the idle tolerance to slowloris: after sending the
// 4-byte length, an attacker squeezing out one byte a second could hold the connection, its goroutine and read buffer for a long time
func ReadFrameHeader(r io.Reader) (uint32, error) {
	var hdr [lengthPrefixBytes]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		// Including io.EOF: the peer closed normally. Callers use it to tell normal disconnects from anomalies
		return 0, err
	}

	n := binary.BigEndian.Uint32(hdr[:])
	switch {
	case n == 0:
		return 0, ErrZeroLength
	case n > MaxFrameBytes:
		// Over the limit returns at once, without allocating, and without draining the body the attacker claims just to read the connection clean;
		// that's exactly the free bandwidth and CPU they want. Not one body byte has been touched yet
		return 0, fmt.Errorf("%w: %d > %d", ErrFrameTooLarge, n, MaxFrameBytes)
	}
	return n, nil
}

// ReadFrameBody reads the full n-byte body into buf and returns the usable slice buf[:n].
//
// buf is provided by the caller and reused per connection, so this function allocates nothing in the steady state.
// n must come from a ReadFrameHeader that just returned successfully on the same connection.
//
// One read may bring back half a Frame, or several at once: UDS is a byte stream,
// and read boundaries aren't message boundaries, so ReadFull is required
func ReadFrameBody(r io.Reader, buf []byte, n uint32) ([]byte, error) {
	if int(n) > len(buf) {
		// The buf the caller gave is smaller than the hard limit: a wiring error on this side, not the peer's fault
		return nil, fmt.Errorf("ipc: read buffer too small: need %d, have %d", n, len(buf))
	}
	if _, err := io.ReadFull(r, buf[:n]); err != nil {
		// The length was read but the body wasn't completed = the peer truncated it or the body deadline expired.
		// ReadFull returns EOF after reading 0 bytes, and ErrUnexpectedEOF after reading part
		return nil, err
	}
	return buf[:n], nil
}

// WriteFrame writes out the length + body.
//
// Calling constraint: only one writer goroutine per connection may call this function. It takes no lock,
// and isn't meant to: two goroutines writing the same stream concurrently, even with one full Write each, would interleave
// the bytes of two Frames, and the receiver could never find the boundaries again. Serialisation is the connection layer's job.
//
// w should be the writer goroutine's buffered writer, so the length and body merge into one system call.
// UDS has no Nagle, so splitting it into two wouldn't cause latency problems, just one wasted syscall.
//
// When an error is returned the caller must close the connection and can't retry this frame: the length may have been written but not the body,
// the peer's parser is stuck waiting for N bytes, and the stream can't be recovered
func WriteFrame(w io.Writer, payload []byte) error {
	n := len(payload)
	switch {
	case n == 0:
		// A bug on this side: don't send violating frames to the peer
		return ErrZeroLength
	case n > MaxFrameBytes:
		return fmt.Errorf("%w: %d > %d", ErrFrameTooLarge, n, MaxFrameBytes)
	}

	var hdr [lengthPrefixBytes]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(n))

	// io.Writer's contract already requires a non-nil error when n < len(p),
	// so no hand-written retry loop is needed; just don't swallow the error
	if _, err := w.Write(hdr[:]); err != nil {
		return fmt.Errorf("ipc: write frame header: %w", err)
	}
	if _, err := w.Write(payload); err != nil {
		return fmt.Errorf("ipc: write frame body: %w", err)
	}
	return nil
}
