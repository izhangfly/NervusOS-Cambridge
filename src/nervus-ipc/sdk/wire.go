// Package sdk is the Go SDK for Nervus OS control-plane IPC v1: Client (the consumer side) and
// ServiceHost (the provider side).
//
// Its users are system services and OEM Providers written in Go (nervus-system-server and others). nervud
// itself does **not** use this package: it's the server, with its own implementation in internal/ipc; this package is the client.
//
// # Red line: never hand-assemble envelopes
//
// This package reuses protocol/ipcv1's generated types to encode and decode Envelopes, never assembling bytes itself. The same as the Kotlin
// SDK (nervus-app-sdk). The cost of hand-assembling isn't hassle but that when "the schema changed and one side
// didn't keep up", both sides still run and only behave differently on some field; that kind of drift is very hard to spot.
//
// # Which bodies this package does **not** implement
//
// nervud doesn't support Cancel(32) yet; on receipt it closes the connection and
// audits it as UnsupportedBody. So this package **deliberately doesn't provide** that API: providing a method that
// "compiles but disconnects as soon as it's called" is worse than not providing it.
//
// The subscription group (40-45) and PublishEvent(53) are wired in the kernel; see subscribe.go for the API.
//
// The authority is the "Implementation status" table in the repository README. Once the kernel wires an item, update that table first,
// then add the API here.
//
// # Framing
//
// This file is the framing layer, corresponding byte for byte with nervud's internal/ipc/frame.go:
//
//	a 4-byte big-endian uint32 N + N bytes of Protobuf Envelope
//
// N excludes the prefix itself; N == 0 and N > 128 KiB are both protocol errors.
package sdk

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// MaxFrameBytes is the absolute hard limit of a control-plane Frame: 128 KiB = 131072 bytes.
//
// It must match nervud's ipc.MaxFrameBytes and the
// ConnectionLimits.max_frame_bytes sent at the handshake. It's the Frame limit, not a method quota;
// per-method quotas are smaller, and the layer above takes the minimum.
const MaxFrameBytes = 128 << 10

const lengthPrefixBytes = 4

// Every framing layer error is a protocol violation: the connection must be closed on sight.
//
// Once the length field can't be trusted, later Frame boundaries can't be determined, and the byte stream on this connection has
// lost its meaning; even "replying with an error" is impossible, because the error response itself needs a length prefix to be read.
var (
	// ErrZeroLength: the length prefix is 0. An empty Envelope has no legitimate use.
	ErrZeroLength = errors.New("sdk: frame length is zero")
	// ErrFrameTooLarge: the length prefix exceeds MaxFrameBytes.
	ErrFrameTooLarge = errors.New("sdk: frame length exceeds hard limit")
)

// readFrameHeader reads the full 4-byte length prefix, checks it, and returns the body length N.
//
// It's split from readFrameBody into two functions for a reason: the moment in between is the only time to set the **body deadline**.
// The idle deadline (waiting for the next Frame) can be long, while the body deadline (N bytes have been announced
// as on their way) must be short. Covering both with one deadline would hand the idle tolerance to
// slowloris: after sending the 4-byte length, squeezing out one byte a second would hold the connection for a long time.
func readFrameHeader(r io.Reader) (uint32, error) {
	var hdr [lengthPrefixBytes]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return 0, err // including io.EOF: the peer closed normally, which callers use to tell normal disconnects apart
	}

	n := binary.BigEndian.Uint32(hdr[:])
	switch {
	case n == 0:
		return 0, ErrZeroLength
	case n > MaxFrameBytes:
		// Return at once, without allocating and without draining the body the peer claims; that's exactly the free bandwidth
		// an attacker wants. Not one body byte has been touched yet.
		return 0, fmt.Errorf("%w: %d > %d", ErrFrameTooLarge, n, MaxFrameBytes)
	}
	return n, nil
}

// readFrameBody reads the full n-byte body into buf and returns buf[:n].
//
// buf is reused per connection by the caller, so the steady state allocates nothing. n must come from a readFrameHeader that just
// returned successfully on the same connection.
//
// ReadFull is required: UDS is a byte stream, and one read may bring back half a Frame, or several at once;
// read boundaries aren't message boundaries.
func readFrameBody(r io.Reader, buf []byte, n uint32) ([]byte, error) {
	if int(n) > len(buf) {
		// A wiring error on this side (buf smaller than the hard limit), not the peer's fault
		return nil, fmt.Errorf("sdk: read buffer too small: need %d, have %d", n, len(buf))
	}
	if _, err := io.ReadFull(r, buf[:n]); err != nil {
		// ReadFull returns EOF after reading 0 bytes, and ErrUnexpectedEOF after reading part
		return nil, err
	}
	return buf[:n], nil
}

// writeFrame writes the length prefix + body.
//
// **Calling constraint**: never call it concurrently on the same connection. This function takes no lock: two goroutines writing
// the same stream concurrently, even with one full write each, would interleave the bytes of two Frames, and the receiver could never
// find the boundaries again. Serialisation is the connection layer's job (see writeMu in conn.writeEnvelope).
//
// When it returns an error you **must close the connection and can't retry this frame**: the length may have been written but not the body,
// the peer's parser is stuck waiting for N bytes, and the stream can't be recovered.
func writeFrame(w io.Writer, payload []byte) error {
	n := len(payload)
	switch {
	case n == 0:
		return ErrZeroLength // a bug on this side: don't send violating frames to the peer
	case n > MaxFrameBytes:
		return fmt.Errorf("%w: %d > %d", ErrFrameTooLarge, n, MaxFrameBytes)
	}

	var hdr [lengthPrefixBytes]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(n))

	if _, err := w.Write(hdr[:]); err != nil {
		return fmt.Errorf("sdk: write frame header: %w", err)
	}
	if _, err := w.Write(payload); err != nil {
		return fmt.Errorf("sdk: write frame body: %w", err)
	}
	return nil
}
