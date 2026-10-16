//go:build linux

// This file publishes control-plane events.
//
// # There are [no] per-frame events on the control plane
//
// The shared-memory ring has its own eventfd wake-up, and consumers can block on that. Also pushing
// "new frame" on the control plane would move once-per-frame traffic back onto it, the very thing the shared-memory data plane exists to avoid.
//
// So there are only two kinds of event here, neither per frame:
//
//	StreamStateChanged  STATE     the stream opened / stopped / broke. Coalescing intermediate states loses nothing
//	DeviceError         RELIABLE  dropped off, buffer overrun, driver error. Can't be coalesced or dropped
//
// # Why a failed publish is only logged
//
// PublishEvent is one-way and has no result. The returned error only means the local write failed (the connection is gone),
// and by then the whole service is already shutting down. nervud refusing an event (event_id not in the contract,
// rate exceeded) produces no reply at all, only a PublishEventRejected entry in the kernel audit.
package main

import (
	camerav1 "github.com/nervus-os/nervus-ipc/protocol/interface/camerav1"
	"google.golang.org/protobuf/proto"
)

// publishState publishes a stream state change.
func (c *cameraEndpoint) publishState(streamID uint64, phase camerav1.StreamPhase) {
	payload, err := proto.Marshal(&camerav1.StreamState{
		StreamId: streamID,
		Phase:    phase,
	})
	if err != nil {
		c.log.Warn("camerad: building StreamState failed", "stream_id", streamID, "err", err)
		return
	}
	// [No timestamp]: what matters about a state change is that it happened; arrival order is all the timing
	// information there is. Forcing in time.Now() would make consumers think it's a time on the device.
	c.publish(uint32(camerav1.CameraEvent_CAMERA_EVENT_STREAM_STATE_CHANGED), payload, 0)
}

// publishDeviceError publishes a device-level error.
//
// droppedFrames only matters for BUFFER_OVERRUN; pass 0 otherwise.
func (c *cameraEndpoint) publishDeviceError(
	streamID uint64, kind camerav1.CameraDeviceErrorKind, droppedFrames uint64,
) {
	payload, err := proto.Marshal(&camerav1.CameraDeviceError{
		StreamId:      streamID,
		Kind:          kind,
		DroppedFrames: droppedFrames,
	})
	if err != nil {
		c.log.Warn("camerad: building CameraDeviceError failed", "stream_id", streamID, "err", err)
		return
	}
	c.publish(uint32(camerav1.CameraEvent_CAMERA_EVENT_DEVICE_ERROR), payload, 0)
}

func (c *cameraEndpoint) publish(eventID uint32, payload []byte, monotonicNanos uint64) {
	if c.ep == nil {
		// The endpoint hasn't registered yet (start-up) or has been withdrawn (shutting down).
		return
	}
	if err := c.ep.PublishEventAt(eventID, payload, monotonicNanos); err != nil {
		c.log.Debug("camerad: publishing event failed", "event_id", eventID, "err", err)
	}
}
