//go:build linux

// The client of the generic high-speed data plane.
// It connects to nervud's /run/nervus/nervud-transfer.sock
package sdk

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"

	ipcv1 "github.com/nervus-os/nervus-ipc/protocol/ipcv1"
)

const (
	// DefaultTransferSockPath is the generic Transfer data plane UDS in the production image.
	DefaultTransferSockPath = "/run/nervus/nervud-transfer.sock"

	transferFrameHeaderBytes = 28
)

var transferFrameMagic = [4]byte{'N', 'V', 'T', '1'}

// TransferFrame is one frame in FRAMED_RELAY mode. This version defines no flags,
// so Flags must be 0; before adding a flag in future, its wire semantics must be frozen first.
type TransferFrame struct {
	Flags                   uint32
	Sequence                uint64
	MonotonicTimestampNanos uint64
	Payload                 []byte
}

// TransferConn is a FRAMED_RELAY connection that has completed ticket attachment.
type TransferConn struct {
	co                net.Conn
	role              ipcv1.TransferRole
	mode              ipcv1.TransferMode
	maxPacketBytes    uint32
	maxBytesPerSecond uint64
	readMu, writeMu   sync.Mutex

	// The following are only valid when mode == SHARED_MEMORY_RING.
	//
	// In ring mode nervud isn't on the data path: both ends mmap the same memory and exchange directly,
	// and the co socket is only used for the handshake, receiving fds and noticing the peer disconnecting.
	ring        *Ring
	ringMem     []byte
	ringMemFD   int
	ringEventFD int
}

// Ring returns the shared memory ring view. Only non-nil when mode == SHARED_MEMORY_RING.
//
// **Frame.Payload points into ring memory and becomes invalid after Advance**; keep it by copying it yourself.
// That's the price of zero-copy, and its whole point.
func (c *TransferConn) Ring() *Ring { return c.ring }

// WriteRing writes one frame and wakes the peer. Producer only.
func (c *TransferConn) WriteRing(payload []byte, flags uint32, timestampNanos uint64) error {
	if c.ring == nil {
		return fmt.Errorf("%w: transfer is not in shared-memory ring mode", ErrProtocol)
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if err := c.ring.Write(payload, flags, timestampNanos); err != nil {
		return err
	}
	return c.notifyRing()
}

// ReadRing takes one frame. Without data it waits timeoutMillis for the peer's notification (negative waits forever).
//
// ErrRingEmpty means still no data after the timeout. On ErrRingLapped the caller should
// SkipToLatest and retry: for video streams the right behaviour is to drop the backlog and look at the latest frame, rather than read
// torn content out of a batch of overwritten slots.
func (c *TransferConn) ReadRing(timeoutMillis int) (Frame, error) {
	if c.ring == nil {
		return Frame{}, fmt.Errorf("%w: transfer is not in shared-memory ring mode", ErrProtocol)
	}
	c.readMu.Lock()
	defer c.readMu.Unlock()
	frame, err := c.ring.Read()
	if err == nil || !errors.Is(err, ErrRingEmpty) {
		return frame, err
	}
	if err := c.waitRing(timeoutMillis); err != nil {
		return Frame{}, err
	}
	return c.ring.Read()
}

// AttachTransfer attaches to the generic data plane using the single-use ticket in the handle.
//
// This SDK currently only implements the protocol's baseline FRAMED_RELAY; SHARED_MEMORY_RING needs the
// memfd/eventfd ring ABI frozen first, and can't be guessed in the SDK.
func AttachTransfer(ctx context.Context, handle *ipcv1.TransferHandle) (*TransferConn, error) {
	if err := validateTransferHandle(handle); err != nil {
		return nil, err
	}

	var dialer net.Dialer
	raw, err := dialer.DialContext(ctx, "unix", handle.GetDataPlaneEndpoint())
	if err != nil {
		return nil, fmt.Errorf("sdk: dial transfer data plane: %w", err)
	}
	stopContext := context.AfterFunc(ctx, func() { _ = raw.Close() })
	defer stopContext()
	if deadline, ok := ctx.Deadline(); ok {
		_ = raw.SetDeadline(deadline)
	}
	fail := func(err error) (*TransferConn, error) {
		_ = raw.Close()
		return nil, err
	}

	attachWire, err := proto.Marshal(&ipcv1.AttachTransfer{
		TransferId:   handle.GetTransferId(),
		AttachTicket: handle.GetAttachTicket(),
		Role:         handle.GetRole(),
	})
	if err != nil {
		return fail(fmt.Errorf("sdk: marshal AttachTransfer: %w", err))
	}
	if err := writeFrame(raw, attachWire); err != nil {
		return fail(fmt.Errorf("sdk: send AttachTransfer: %w", err))
	}

	n, err := readFrameHeader(raw)
	if err != nil {
		return fail(fmt.Errorf("sdk: read AttachTransferResult header: %w", err))
	}
	buf := make([]byte, n)
	resultWire, err := readFrameBody(raw, buf, n)
	if err != nil {
		return fail(fmt.Errorf("sdk: read AttachTransferResult: %w", err))
	}
	var result ipcv1.AttachTransferResult
	if err := proto.Unmarshal(resultWire, &result); err != nil {
		return fail(fmt.Errorf("%w: malformed AttachTransferResult: %v", ErrProtocol, err))
	}
	if failure := result.GetFailure(); failure != nil {
		return fail(statusErrorFrom(failure))
	}
	success := result.GetSuccess()
	if success == nil {
		return fail(fmt.Errorf("%w: AttachTransferResult has neither outcome", ErrProtocol))
	}
	if success.GetMode() != handle.GetMode() {
		return fail(fmt.Errorf("%w: transfer mode changed from %s to %s",
			ErrProtocol, handle.GetMode(), success.GetMode()))
	}
	switch success.GetMode() {
	case ipcv1.TransferMode_TRANSFER_MODE_FRAMED_RELAY,
		ipcv1.TransferMode_TRANSFER_MODE_SHARED_MEMORY_RING:
	default:
		return fail(fmt.Errorf("%w: transfer mode %s is not implemented by Go SDK",
			ErrProtocol, success.GetMode()))
	}
	if success.GetMaxPacketBytes() == 0 || success.GetMaxBytesPerSecond() == 0 {
		return fail(fmt.Errorf("%w: transfer limits must be non-zero", ErrProtocol))
	}

	conn := &TransferConn{
		co:                raw,
		role:              handle.GetRole(),
		mode:              success.GetMode(),
		maxPacketBytes:    success.GetMaxPacketBytes(),
		maxBytesPerSecond: success.GetMaxBytesPerSecond(),
	}

	// Ring mode: after the result frame nervud sends the memfd and eventfd with SCM_RIGHTS.
	// **Receive them before clearing the deadline**: receiving fds is still part of the handshake, and the timeout must still apply.
	if success.GetMode() == ipcv1.TransferMode_TRANSFER_MODE_SHARED_MEMORY_RING {
		if err := conn.attachRing(raw, success.GetRing()); err != nil {
			return fail(err)
		}
	}

	if !stopContext() {
		return fail(ctx.Err())
	}
	if err := raw.SetDeadline(time.Time{}); err != nil {
		return fail(fmt.Errorf("sdk: clear transfer handshake deadline: %w", err))
	}
	return conn, nil
}

func validateTransferHandle(handle *ipcv1.TransferHandle) error {
	if handle == nil {
		return errors.New("sdk: nil TransferHandle")
	}
	if len(handle.GetTransferId()) != 16 {
		return fmt.Errorf("sdk: transfer id has %d bytes, want 16", len(handle.GetTransferId()))
	}
	if len(handle.GetAttachTicket()) < 32 {
		return fmt.Errorf("sdk: attach ticket has %d bytes, want at least 32", len(handle.GetAttachTicket()))
	}
	switch handle.GetRole() {
	case ipcv1.TransferRole_TRANSFER_ROLE_PRODUCER,
		ipcv1.TransferRole_TRANSFER_ROLE_CONSUMER,
		ipcv1.TransferRole_TRANSFER_ROLE_PEER:
	default:
		return fmt.Errorf("sdk: invalid transfer role %d", handle.GetRole())
	}
	switch handle.GetMode() {
	case ipcv1.TransferMode_TRANSFER_MODE_FRAMED_RELAY,
		ipcv1.TransferMode_TRANSFER_MODE_SHARED_MEMORY_RING:
	default:
		return fmt.Errorf("sdk: unsupported transfer mode %s", handle.GetMode())
	}
	if handle.GetExpiresAtMonotonicNanos() == 0 {
		return errors.New("sdk: transfer handle has zero expiry")
	}
	if handle.GetDataPlaneEndpoint() == "" {
		return errors.New("sdk: transfer handle has empty data plane endpoint")
	}
	return nil
}

// Role returns this side's role in the Transfer.
func (c *TransferConn) Role() ipcv1.TransferRole { return c.role }

// Mode returns the transfer mode in effect after the handshake.
func (c *TransferConn) Mode() ipcv1.TransferMode { return c.mode }

// MaxPacketBytes returns the per-frame payload limit after nervud tightened it.
func (c *TransferConn) MaxPacketBytes() uint32 { return c.maxPacketBytes }

// MaxBytesPerSecond returns the rate limit after nervud tightened it.
func (c *TransferConn) MaxBytesPerSecond() uint64 { return c.maxBytesPerSecond }

// Close closes the data plane connection, and in ring mode unmaps the memory and closes both fds.
//
// The order is unmap first, then close the socket: the other way round, the peer would see the disconnect first, followed by a short time when
// we still hold the memory mapping, a window whose behaviour is hard to reason about.
func (c *TransferConn) Close() error {
	c.closeRing()
	return c.co.Close()
}

func (c *TransferConn) SetDeadline(deadline time.Time) error {
	return c.co.SetDeadline(deadline)
}

func (c *TransferConn) SetReadDeadline(deadline time.Time) error {
	return c.co.SetReadDeadline(deadline)
}

func (c *TransferConn) SetWriteDeadline(deadline time.Time) error {
	return c.co.SetWriteDeadline(deadline)
}

// ReadFrame reads one frame. Pure PRODUCER handles may not read.
func (c *TransferConn) ReadFrame() (TransferFrame, error) {
	if c.role == ipcv1.TransferRole_TRANSFER_ROLE_PRODUCER {
		return TransferFrame{}, errors.New("sdk: producer transfer handle cannot read")
	}
	c.readMu.Lock()
	defer c.readMu.Unlock()
	return readTransferFrame(c.co, c.maxPacketBytes)
}

// WriteFrame writes one frame. Pure CONSUMER handles may not write.
func (c *TransferConn) WriteFrame(frame TransferFrame) error {
	if c.role == ipcv1.TransferRole_TRANSFER_ROLE_CONSUMER {
		return errors.New("sdk: consumer transfer handle cannot write")
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return writeTransferFrame(c.co, frame, c.maxPacketBytes)
}

func readTransferFrame(r io.Reader, maxPacketBytes uint32) (TransferFrame, error) {
	if maxPacketBytes == 0 {
		return TransferFrame{}, errors.New("sdk: zero transfer packet limit")
	}
	var header [transferFrameHeaderBytes]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return TransferFrame{}, err
	}
	if !bytes.Equal(header[0:4], transferFrameMagic[:]) {
		return TransferFrame{}, fmt.Errorf("%w: invalid transfer frame magic", ErrProtocol)
	}
	flags := binary.BigEndian.Uint32(header[4:8])
	if flags != 0 {
		return TransferFrame{}, fmt.Errorf("%w: unknown transfer frame flags %#x", ErrProtocol, flags)
	}
	payloadBytes := binary.BigEndian.Uint32(header[24:28])
	if payloadBytes > maxPacketBytes {
		return TransferFrame{}, fmt.Errorf("%w: transfer payload %d exceeds limit %d",
			ErrFrameTooLarge, payloadBytes, maxPacketBytes)
	}
	maxInt := uint64(^uint(0) >> 1)
	if uint64(payloadBytes) > maxInt {
		return TransferFrame{}, fmt.Errorf("%w: transfer payload does not fit platform int", ErrFrameTooLarge)
	}
	payload := make([]byte, int(payloadBytes))
	if _, err := io.ReadFull(r, payload); err != nil {
		return TransferFrame{}, err
	}
	return TransferFrame{
		Flags:                   flags,
		Sequence:                binary.BigEndian.Uint64(header[8:16]),
		MonotonicTimestampNanos: binary.BigEndian.Uint64(header[16:24]),
		Payload:                 payload,
	}, nil
}

func writeTransferFrame(w io.Writer, frame TransferFrame, maxPacketBytes uint32) error {
	if maxPacketBytes == 0 {
		return errors.New("sdk: zero transfer packet limit")
	}
	if frame.Flags != 0 {
		return fmt.Errorf("%w: unknown transfer frame flags %#x", ErrProtocol, frame.Flags)
	}
	if uint64(len(frame.Payload)) > uint64(maxPacketBytes) {
		return fmt.Errorf("%w: transfer payload %d exceeds limit %d",
			ErrFrameTooLarge, len(frame.Payload), maxPacketBytes)
	}
	var header [transferFrameHeaderBytes]byte
	copy(header[0:4], transferFrameMagic[:])
	binary.BigEndian.PutUint32(header[4:8], frame.Flags)
	binary.BigEndian.PutUint64(header[8:16], frame.Sequence)
	binary.BigEndian.PutUint64(header[16:24], frame.MonotonicTimestampNanos)
	binary.BigEndian.PutUint32(header[24:28], uint32(len(frame.Payload)))
	if err := writeAll(w, header[:]); err != nil {
		return err
	}
	return writeAll(w, frame.Payload)
}

func writeAll(w io.Writer, payload []byte) error {
	for len(payload) != 0 {
		n, err := w.Write(payload)
		if err != nil {
			return err
		}
		if n <= 0 {
			return io.ErrShortWrite
		}
		payload = payload[n:]
	}
	return nil
}
