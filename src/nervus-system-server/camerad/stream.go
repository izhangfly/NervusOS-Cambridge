//go:build linux

// This file is the lifecycle of a stream: from OpenStream building the pipe to the frame pump moving V4L2 buffers into the shared-memory ring.
//
// # The data plane goes through neither nervud nor this process's control plane
//
//	App --Request(OpenStream)--> nervud --Dispatch--> camerad
//	camerad --BeginTransfer--> nervud                      build the pipe
//	camerad --AttachTransfer--> nervud-transfer.sock       attach the producer end ourselves
//	camerad --CommitTransfer--> nervud                     release the consumer end
//	camerad --OpenStreamResponse(caller handle)--> App     hand over the credential
//	App --AttachTransfer--> nervud-transfer.sock           attach the consumer end
//	                    ↓
//	         after that frames flow in the shared-memory ring, with neither nervud nor this file's control plane on the data path
//
// [The order can't change]. The caller ticket can't attach before Commit, and Commit must come after the business response
// is certain to succeed. Committing first and then failing would leave the App without a handle while the Transfer
// was already released, a dangling pipe nobody can close.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	camerav1 "github.com/nervus-os/nervus-ipc/protocol/interface/camerav1"
	ipcv1 "github.com/nervus-os/nervus-ipc/protocol/ipcv1"
	"github.com/nervus-os/nervus-ipc/sdk"
	"google.golang.org/protobuf/proto"
)

// v4l2BufferCount is the number of capture buffers requested from the driver.
//
// Four: too few (2) and one GC pause makes the driver drop frames; too many just wastes kernel memory
// and [doesn't reduce latency]: extra buffers only deepen the backlog, and the consumer still gets old frames.
const v4l2BufferCount = 4

// frameWaitMillis caps how long one poll of the frame pump waits.
//
// It decides "how soon we notice the device has stopped producing frames". One second: far longer than any sensible frame period,
// so no false alarms, yet short enough that a camera that dropped off enters FAULT within a second instead of leaving the App waiting.
const frameWaitMillis = 1000

// stream is one stream in progress.
type stream struct {
	id     uint64
	device *v4l2Device
	conn   *sdk.TransferConn
	format *camerav1.StreamFormat
	rate   uint32

	// ctx / cancel belong only to the frame pump. CloseStream cancels and waits for done rather than issuing
	// STREAMOFF itself: the device handle isn't concurrency-safe, so only one goroutine may touch it.
	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}
}

// cameraEndpoint is the endpoint bound to one camera: its handlers and state.
type cameraEndpoint struct {
	host    *sdk.ServiceHost
	ep      *sdk.EndpointHost
	role    string
	device  Device
	log     *slog.Logger
	formats []formatOption

	mu       sync.Mutex
	streams  map[uint64]*stream
	nextID   uint64
	transfer sdk.Endpoint
	// transferResolved records whether the Transfer Control endpoint has been resolved.
	transferResolved bool
}

// maxStreamsPerCamera matches transfer.max_streams for OpenStream in camera.proto.
//
// [The two must be equal]: nervud tightens to the MethodMeta, and stopping it again here only serves to give
// an error with a typed detail (TOO_MANY_STREAMS) instead of handing the caller
// a generic RESOURCE_EXHAUSTED.
const maxStreamsPerCamera = 4

func newCameraEndpoint(
	host *sdk.ServiceHost, role string, device Device, log *slog.Logger,
) (*cameraEndpoint, error) {
	dev, err := openDevice(device.Node)
	if err != nil {
		return nil, err
	}
	// [Enumerate once, never again]: ENUM_FMT may fail or block while streaming, and
	// DescribeStream is the first method an App must call before setting up a stream. Ask everything at start-up and
	// afterwards only answer, so one driver hiccup can't turn into "the App can't use the camera at all".
	formats, err := dev.enumerateFormats()
	closeErr := dev.Close()
	if err != nil {
		return nil, err
	}
	if closeErr != nil {
		log.Warn("camerad: closing the device handle used for enumeration failed", "node", device.Node, "err", closeErr)
	}
	if len(formats) == 0 {
		return nil, fmt.Errorf("camerad: %s reports no discrete streaming formats", device.Node)
	}

	return &cameraEndpoint{
		host:    host,
		role:    role,
		device:  device,
		log:     log.With("role", role, "node", device.Node),
		formats: formats,
		streams: make(map[uint64]*stream),
	}, nil
}

// install attaches the three methods to the endpoint and registers it.
func (c *cameraEndpoint) install(ctx context.Context, schemaHash []byte) error {
	c.ep = c.host.NewEndpoint(sdk.RegisterRequest{
		InterfaceID:    interfaceCamera,
		Major:          1,
		SchemaHash:     schemaHash,
		ResourceHandle: c.role,
	})
	for id, fn := range map[uint32]sdk.Handler{
		uint32(camerav1.CameraMethod_CAMERA_METHOD_DESCRIBE_STREAM): c.describeStream,
		uint32(camerav1.CameraMethod_CAMERA_METHOD_OPEN_STREAM):     c.openStream,
		uint32(camerav1.CameraMethod_CAMERA_METHOD_CLOSE_STREAM):    c.closeStream,
	} {
		if err := c.ep.Handle(id, fn); err != nil {
			return err
		}
	}
	endpointID, err := c.ep.Register(ctx)
	if err != nil {
		return fmt.Errorf("camerad: register %s: %w", c.role, err)
	}
	c.log.Info("camerad: endpoint registered", "endpoint_id", endpointID)
	return nil
}

func (c *cameraEndpoint) describeStream(cc sdk.CallContext, _ []byte) ([]byte, error) {
	out := &camerav1.StreamDescription{
		Formats: make([]*camerav1.StreamFormat, 0, len(c.formats)),
	}
	for _, option := range c.formats {
		pixel, ok := pixelFormatOf(option.PixelFormat)
		if !ok {
			// A fourcc the platform doesn't know. [Skip it rather than report UNSPECIFIED]: reporting it
			// would give the App a format whose layout it can't interpret, which it would then use to parse memory.
			continue
		}
		out.Formats = append(out.Formats, &camerav1.StreamFormat{
			PixelFormat:   pixel,
			Width:         option.Width,
			Height:        option.Height,
			FrameRates:    option.FrameRates,
			MaxFrameBytes: option.MaxBytes,
		})
	}
	return proto.Marshal(out)
}

func (c *cameraEndpoint) openStream(cc sdk.CallContext, payload []byte) ([]byte, error) {
	var req camerav1.OpenStreamRequest
	if err := proto.Unmarshal(payload, &req); err != nil {
		return nil, cameraError(ipcv1.StatusCode_STATUS_CODE_INVALID_ARGUMENT,
			camerav1.CameraReason_CAMERA_REASON_UNSPECIFIED, camerav1.ControlKind_CONTROL_KIND_UNSPECIFIED)
	}

	option, ok := c.matchFormat(req.GetFormat(), req.GetFrameRate())
	if !ok {
		// [No nearest match]: silently giving a smaller resolution would make the App parse shared memory
		// with the wrong dimensions, and the torn picture would be untraceable.
		return nil, cameraError(ipcv1.StatusCode_STATUS_CODE_FAILED_PRECONDITION,
			camerav1.CameraReason_CAMERA_REASON_FORMAT_NOT_SUPPORTED,
			camerav1.ControlKind_CONTROL_KIND_UNSPECIFIED)
	}

	c.mu.Lock()
	if len(c.streams) >= maxStreamsPerCamera {
		c.mu.Unlock()
		return nil, cameraError(ipcv1.StatusCode_STATUS_CODE_RESOURCE_EXHAUSTED,
			camerav1.CameraReason_CAMERA_REASON_TOO_MANY_STREAMS,
			camerav1.ControlKind_CONTROL_KIND_UNSPECIFIED)
	}
	c.nextID++
	streamID := c.nextID
	c.mu.Unlock()

	begun, err := c.beginTransfer(cc, option.MaxBytes)
	if err != nil {
		return nil, err
	}

	// Any failure between here and Commit must Abort, or that PREPARED Transfer
	// holds on until the connection drops.
	committed := false
	defer func() {
		if !committed {
			c.abortTransfer(cc.Ctx, begun.GetProvider().GetTransferId())
		}
	}()

	st, err := c.startCapture(streamID, option, req.GetFrameRate(), begun.GetProvider())
	if err != nil {
		return nil, err
	}

	response, err := proto.Marshal(&camerav1.OpenStreamResponse{
		StreamId:  streamID,
		Transfer:  begun.GetCaller(),
		Format:    st.format,
		FrameRate: st.rate,
	})
	if err != nil {
		c.teardown(st)
		return nil, err
	}

	// [Commit only once the response is certain to be returned]. Committing first and then failing to serialise would leave the App without
	// a handle while the Transfer was already released: a dangling pipe nobody can close.
	if err := c.commitTransfer(cc.Ctx, begun.GetProvider().GetTransferId()); err != nil {
		c.teardown(st)
		return nil, err
	}
	committed = true

	c.mu.Lock()
	c.streams[streamID] = st
	c.mu.Unlock()

	// [Register event ownership]: both events are declared scoped, and without registration nobody can subscribe to them.
	//
	// Use cc.RouteID rather than a self-declared caller identity: it proves "this stream belongs to whoever is calling me now",
	// and nervud itself knows who that is.
	//
	// After Commit: any failure before that goes through teardown, which
	// withdraws the registration; registering too early would only mean a pointless withdrawal.
	if err := c.ep.BindEventScope(streamID, cc.RouteID); err != nil {
		// A failed registration shouldn't fail a stream that's already set up: frames still flow, only the state
		// events can't be subscribed to. Log a WARN so it's visible, rather than turning a working stream into a failure.
		c.log.Warn("camerad: registering event ownership failed; this stream's state events can't be subscribed to",
			"stream_id", streamID, "err", err)
	}

	c.publishState(streamID, camerav1.StreamPhase_STREAM_PHASE_STARTING)
	go c.pump(st)
	return response, nil
}

func (c *cameraEndpoint) closeStream(cc sdk.CallContext, payload []byte) ([]byte, error) {
	var req camerav1.CloseStreamRequest
	if err := proto.Unmarshal(payload, &req); err != nil {
		return nil, cameraError(ipcv1.StatusCode_STATUS_CODE_INVALID_ARGUMENT,
			camerav1.CameraReason_CAMERA_REASON_UNSPECIFIED,
			camerav1.ControlKind_CONTROL_KIND_UNSPECIFIED)
	}

	c.mu.Lock()
	st, ok := c.streams[req.GetStreamId()]
	if ok {
		delete(c.streams, req.GetStreamId())
	}
	c.mu.Unlock()

	if !ok {
		return nil, cameraError(ipcv1.StatusCode_STATUS_CODE_NOT_FOUND,
			camerav1.CameraReason_CAMERA_REASON_STREAM_NOT_FOUND,
			camerav1.ControlKind_CONTROL_KIND_UNSPECIFIED)
	}
	c.teardown(st)
	// [Send the state first, then withdraw ownership]: after withdrawing, subscribers get nothing, and STOPPED is the one
	// they need most.
	c.publishState(req.GetStreamId(), camerav1.StreamPhase_STREAM_PHASE_STOPPED)
	c.releaseScope(req.GetStreamId())
	return nil, nil
}

// releaseScope withdraws a stream's event ownership.
//
// [It must be withdrawn explicitly]: otherwise a camerad that keeps opening and closing streams would make nervud's ownership table
// grow without bound. Dropped connections and withdrawn endpoints clean up too, but that covers crashes, not normal closing.
func (c *cameraEndpoint) releaseScope(streamID uint64) {
	if c.ep == nil {
		return
	}
	if err := c.ep.ReleaseEventScope(streamID); err != nil {
		c.log.Debug("camerad: withdrawing event ownership failed", "stream_id", streamID, "err", err)
	}
}

// matchFormat looks for an [exactly matching] combination in the enumeration results.
func (c *cameraEndpoint) matchFormat(
	want *camerav1.StreamFormat, rate uint32,
) (formatOption, bool) {
	if want == nil {
		return formatOption{}, false
	}
	fourcc, ok := fourccOf(want.GetPixelFormat())
	if !ok {
		return formatOption{}, false
	}
	for _, option := range c.formats {
		if option.PixelFormat != fourcc ||
			option.Width != want.GetWidth() || option.Height != want.GetHeight() {
			continue
		}
		if rate == 0 {
			return option, true
		}
		for _, supported := range option.FrameRates {
			if supported == rate {
				return option, true
			}
		}
		return formatOption{}, false
	}
	return formatOption{}, false
}

// startCapture opens the device, sets the format, starts streaming and attaches the producer end of the data plane.
func (c *cameraEndpoint) startCapture(
	streamID uint64, option formatOption, rate uint32, handle *ipcv1.TransferHandle,
) (*stream, error) {
	dev, err := openDevice(c.device.Node)
	if err != nil {
		return nil, deviceUnavailable()
	}

	actual, err := dev.setFormat(option.PixelFormat, option.Width, option.Height)
	if err != nil {
		_ = dev.Close()
		return nil, deviceUnavailable()
	}
	// S_FMT is a negotiation, not a setting: the driver changes an unsupported request to the nearest supported value [without an error].
	// If the read-back doesn't match we must fail; carrying on would parse memory laid out at another size using the requested one.
	if actual.width != option.Width || actual.height != option.Height ||
		actual.pixelformat != option.PixelFormat {
		_ = dev.Close()
		c.log.Warn("camerad: the driver rewrote the requested format",
			"want_w", option.Width, "want_h", option.Height,
			"got_w", actual.width, "got_h", actual.height)
		return nil, cameraError(ipcv1.StatusCode_STATUS_CODE_FAILED_PRECONDITION,
			camerav1.CameraReason_CAMERA_REASON_FORMAT_NOT_SUPPORTED,
			camerav1.ControlKind_CONTROL_KIND_UNSPECIFIED)
	}
	dev.setFrameRate(rate)

	if err := dev.startStreaming(v4l2BufferCount); err != nil {
		_ = dev.Close()
		c.log.Warn("camerad: starting the stream failed", "err", err)
		return nil, deviceUnavailable()
	}

	attachCtx, cancelAttach := context.WithTimeout(context.Background(), 5*time.Second)
	conn, err := sdk.AttachTransfer(attachCtx, handle)
	cancelAttach()
	if err != nil {
		_ = dev.Close()
		c.log.Warn("camerad: attaching the data plane failed", "err", err)
		return nil, cameraError(ipcv1.StatusCode_STATUS_CODE_UNAVAILABLE,
			camerav1.CameraReason_CAMERA_REASON_DRIVER_FAULT,
			camerav1.ControlKind_CONTROL_KIND_UNSPECIFIED)
	}

	pixel, _ := pixelFormatOf(option.PixelFormat)
	ctx, cancel := context.WithCancel(context.Background())
	return &stream{
		id:     streamID,
		device: dev,
		conn:   conn,
		format: &camerav1.StreamFormat{
			PixelFormat:   pixel,
			Width:         option.Width,
			Height:        option.Height,
			FrameRates:    option.FrameRates,
			MaxFrameBytes: option.MaxBytes,
		},
		rate:   rate,
		ctx:    ctx,
		cancel: cancel,
		done:   make(chan struct{}),
	}, nil
}

// pump is the frame pump: it moves V4L2 buffers into the shared-memory ring.
//
// # It's the only goroutine that touches the device
//
// v4l2Device isn't concurrency-safe, and DQBUF/QBUF must be serial. Letting control-plane handlers touch the device too
// would have two threads working on the same fd, and V4L2's response to that is undefined. CloseStream
// cancels and waits for done rather than issuing STREAMOFF itself.
//
// # Dropping frames
//
// When the ring is full, [drop the new frame] and carry on, never blocking. Video is lossy by nature: blocking here would quickly drain
// V4L2's buffer pool, and the driver would start dropping frames in the kernel, where which frames and how many
// are completely invisible to user space. Dropping here at least keeps the dropped count accurate.
func (c *cameraEndpoint) pump(st *stream) {
	defer close(st.done)

	var dropped uint64

	for {
		select {
		case <-st.ctx.Done():
			return
		default:
		}

		if err := st.device.waitFrame(frameWaitMillis); err != nil {
			if errors.Is(err, os.ErrDeadlineExceeded) {
				// No frame for a second. Could just be a low-frame-rate device; keep waiting.
				continue
			}
			c.log.Warn("camerad: waiting for a frame failed", "stream_id", st.id, "err", err)
			c.publishDeviceError(st.id,
				camerav1.CameraDeviceErrorKind_CAMERA_DEVICE_ERROR_KIND_DISCONNECTED, dropped)
			c.publishState(st.id, camerav1.StreamPhase_STREAM_PHASE_FAULT)
			c.releaseScope(st.id)
			return
		}

		frame, err := st.device.dequeue()
		if errors.Is(err, errNoFrame) {
			continue
		}
		if err != nil {
			c.log.Warn("camerad: dequeuing a frame failed", "stream_id", st.id, "err", err)
			c.publishDeviceError(st.id,
				camerav1.CameraDeviceErrorKind_CAMERA_DEVICE_ERROR_KIND_DRIVER_FAULT, dropped)
			c.publishState(st.id, camerav1.StreamPhase_STREAM_PHASE_FAULT)
			return
		}

		// [Pass the timestamp through unchanged]: it's the moment the sensor produced the frame, without any of the queueing delay after it.
		// Replacing it with time.Now() would give a plausible-looking number with scheduling jitter mixed in.
		writeErr := st.conn.WriteRing(frame.Data, 0, frame.MonotonicNanos)
		if releaseErr := st.device.release(frame); releaseErr != nil {
			// The buffer can't go back, and once the pool is drained no more frames can be taken. This path can't continue.
			c.log.Error("camerad: returning the buffer failed", "stream_id", st.id, "err", releaseErr)
			c.publishDeviceError(st.id,
				camerav1.CameraDeviceErrorKind_CAMERA_DEVICE_ERROR_KIND_DRIVER_FAULT, dropped)
			c.publishState(st.id, camerav1.StreamPhase_STREAM_PHASE_FAULT)
			return
		}
		if writeErr != nil {
			dropped++
			if dropped%100 == 1 {
				// Log once every 100 dropped frames: logging every frame would let an App that can't keep up
				// fill the log disk, which is far worse than dropping frames.
				c.log.Warn("camerad: the consumer can't keep up; dropping frames",
					"stream_id", st.id, "dropped", dropped, "err", writeErr)
			}
			continue
		}
		if dropped > 0 {
			c.publishDeviceError(st.id,
				camerav1.CameraDeviceErrorKind_CAMERA_DEVICE_ERROR_KIND_BUFFER_OVERRUN, dropped)
			dropped = 0
		}
	}
}

// teardown stops a stream and releases every resource. Idempotent.
func (c *cameraEndpoint) teardown(st *stream) {
	st.cancel()
	select {
	case <-st.done:
	case <-time.After(3 * time.Second):
		// The frame pump didn't exit in time. Closing the device anyway would have it ioctl on a closed fd.
		// Log it, then close anyway: leaking a goroutine beats leaking a camera.
		c.log.Error("camerad: the frame pump didn't exit in time", "stream_id", st.id)
	}
	if err := st.conn.Close(); err != nil {
		c.log.Warn("camerad: closing the data plane failed", "stream_id", st.id, "err", err)
	}
	if err := st.device.Close(); err != nil {
		c.log.Warn("camerad: closing the device failed", "stream_id", st.id, "err", err)
	}
}

// closeAll closes every stream at shutdown.
func (c *cameraEndpoint) closeAll() {
	c.mu.Lock()
	streams := make([]*stream, 0, len(c.streams))
	for _, st := range c.streams {
		streams = append(streams, st)
	}
	c.streams = make(map[uint64]*stream)
	c.mu.Unlock()

	for _, st := range streams {
		c.teardown(st)
		c.releaseScope(st.id)
	}
}
