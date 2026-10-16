package sdk

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	ipcv1 "github.com/nervus-os/nervus-ipc/protocol/ipcv1"
)

func TestTransferFrameCodec(t *testing.T) {
	want := TransferFrame{
		Sequence:                7,
		MonotonicTimestampNanos: 123456789,
		Payload:                 []byte("frame"),
	}
	var wire bytes.Buffer
	if err := writeTransferFrame(&wire, want, 1024); err != nil {
		t.Fatalf("writeTransferFrame: %v", err)
	}
	if got := wire.Bytes()[:4]; !bytes.Equal(got, []byte("NVT1")) {
		t.Fatalf("magic = %q", got)
	}
	got, err := readTransferFrame(&wire, 1024)
	if err != nil {
		t.Fatalf("readTransferFrame: %v", err)
	}
	if got.Sequence != want.Sequence ||
		got.MonotonicTimestampNanos != want.MonotonicTimestampNanos ||
		!bytes.Equal(got.Payload, want.Payload) {
		t.Fatalf("round trip = %+v", got)
	}

	if err := writeTransferFrame(&wire, TransferFrame{Flags: 1}, 1024); !errors.Is(err, ErrProtocol) {
		t.Fatalf("unknown flags error = %v", err)
	}
	if err := writeTransferFrame(&wire, TransferFrame{Payload: make([]byte, 2)}, 1); !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("oversize write error = %v", err)
	}

	badMagic := make([]byte, transferFrameHeaderBytes)
	copy(badMagic, "BAD!")
	if _, err := readTransferFrame(bytes.NewReader(badMagic), 1024); !errors.Is(err, ErrProtocol) {
		t.Fatalf("bad magic error = %v", err)
	}
}

func TestAttachTransferFramedRelay(t *testing.T) {
	dir, err := os.MkdirTemp("", "ntr-")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path := filepath.Join(dir, "s")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	serverDone := make(chan error, 1)
	go func() {
		co, err := listener.Accept()
		if err != nil {
			serverDone <- err
			return
		}
		defer func() { _ = co.Close() }()

		n, err := readFrameHeader(co)
		if err != nil {
			serverDone <- err
			return
		}
		buf := make([]byte, n)
		wire, err := readFrameBody(co, buf, n)
		if err != nil {
			serverDone <- err
			return
		}
		var attach ipcv1.AttachTransfer
		if err := proto.Unmarshal(wire, &attach); err != nil {
			serverDone <- err
			return
		}
		if len(attach.GetTransferId()) != 16 ||
			len(attach.GetAttachTicket()) != 32 ||
			attach.GetRole() != ipcv1.TransferRole_TRANSFER_ROLE_PEER {
			serverDone <- fmt.Errorf("bad attach: %+v", &attach)
			return
		}
		resultWire, err := proto.Marshal(&ipcv1.AttachTransferResult{
			Outcome: &ipcv1.AttachTransferResult_Success{
				Success: &ipcv1.AttachTransferSuccess{
					Mode:              ipcv1.TransferMode_TRANSFER_MODE_FRAMED_RELAY,
					MaxPacketBytes:    1024,
					MaxBytesPerSecond: 4096,
				},
			},
		})
		if err != nil {
			serverDone <- err
			return
		}
		if err := writeFrame(co, resultWire); err != nil {
			serverDone <- err
			return
		}

		frame, err := readTransferFrame(co, 1024)
		if err != nil {
			serverDone <- err
			return
		}
		if frame.Sequence != 1 || string(frame.Payload) != "provider-frame" {
			serverDone <- fmt.Errorf("unexpected frame: %+v", frame)
			return
		}
		serverDone <- writeTransferFrame(co, TransferFrame{
			Sequence: 2, Payload: []byte("kernel-frame"),
		}, 1024)
	}()

	handle := &ipcv1.TransferHandle{
		TransferId:              bytes.Repeat([]byte{0x11}, 16),
		AttachTicket:            bytes.Repeat([]byte{0x22}, 32),
		Role:                    ipcv1.TransferRole_TRANSFER_ROLE_PEER,
		Mode:                    ipcv1.TransferMode_TRANSFER_MODE_FRAMED_RELAY,
		ExpiresAtMonotonicNanos: 1,
		DataPlaneEndpoint:       path,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	transfer, err := AttachTransfer(ctx, handle)
	if err != nil {
		t.Fatalf("AttachTransfer: %v", err)
	}
	defer func() { _ = transfer.Close() }()
	if transfer.MaxPacketBytes() != 1024 || transfer.MaxBytesPerSecond() != 4096 {
		t.Fatalf("limits = %d/%d", transfer.MaxPacketBytes(), transfer.MaxBytesPerSecond())
	}
	if err := transfer.WriteFrame(TransferFrame{
		Sequence: 1, Payload: []byte("provider-frame"),
	}); err != nil {
		t.Fatalf("WriteFrame: %v", err)
	}
	frame, err := transfer.ReadFrame()
	if err != nil {
		t.Fatalf("ReadFrame: %v", err)
	}
	if frame.Sequence != 2 || string(frame.Payload) != "kernel-frame" {
		t.Fatalf("received frame = %+v", frame)
	}
	if err := <-serverDone; err != nil {
		t.Fatalf("server: %v", err)
	}
}

func TestTransferRoleEnforcement(t *testing.T) {
	consumer := &TransferConn{role: ipcv1.TransferRole_TRANSFER_ROLE_CONSUMER}
	if err := consumer.WriteFrame(TransferFrame{}); err == nil {
		t.Fatal("consumer was allowed to write")
	}
	producer := &TransferConn{role: ipcv1.TransferRole_TRANSFER_ROLE_PRODUCER}
	if _, err := producer.ReadFrame(); err == nil {
		t.Fatal("producer was allowed to read")
	}
}
