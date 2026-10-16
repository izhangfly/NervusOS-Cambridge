//go:build linux

// This file is the config endpoint: nervus.interface.camera.config@1.
//
// # Why it's a separate endpoint
//
// "Watching the picture" and "changing settings" have opposite resource semantics: the former is shared (navigation, recording and AI detection
// can all watch the same stream), while the latter must be exclusive (with two processes changing exposure at once, who wins depends on
// timing, and the picture flips between the two values).
//
// Kernel leases are issued per resource and one endpoint binds one resource, so the two must be two resources and
// two endpoints. Split apart, an App that only watches never needs a lease, and so is never
// refused because someone else is adjusting settings.
//
// # The device is reopened on every call
//
// The config side holds no device handle, because the capture side [may be using it]: a V4L2 device handle isn't
// concurrency-safe, and two goroutines issuing ioctls on the same fd is undefined behaviour.
//
// V4L2 allows the same node to be opened several times, and control ioctls (G_CTRL/S_CTRL) act on the device
// rather than the handle, so changing settings through a separate handle is safe; it's what v4l2-ctl does.
// The cost is an open/close per call; configuration is infrequent, and in exchange
// "the config side never blocks the capture side".
package main

import (
	"context"
	"fmt"
	"log/slog"

	camerav1 "github.com/nervus-os/nervus-ipc/protocol/interface/camerav1"
	ipcv1 "github.com/nervus-os/nervus-ipc/protocol/ipcv1"
	"github.com/nervus-os/nervus-ipc/sdk"
	"google.golang.org/protobuf/proto"

	"github.com/nervus-os/nervus-system-server/camerad/boardprofile"
)

// V4L2 control IDs. base is V4L2_CID_BASE = V4L2_CTRL_CLASS_USER | 0x900.
const (
	v4l2CIDBase = 0x00980000 | 0x900

	v4l2CIDBrightness       = v4l2CIDBase + 0
	v4l2CIDContrast         = v4l2CIDBase + 1
	v4l2CIDSaturation       = v4l2CIDBase + 2
	v4l2CIDAutoWhiteBalance = v4l2CIDBase + 12
	v4l2CIDGain             = v4l2CIDBase + 19
	v4l2CIDWhiteBalanceTemp = v4l2CIDBase + 26
	v4l2CIDRotate           = v4l2CIDBase + 34
	v4l2CIDExposureAuto     = 0x009a0000 + 1 // V4L2_CID_CAMERA_CLASS_BASE
	v4l2CIDExposureAbsolute = 0x009a0000 + 2
	v4l2CIDFocusAbsolute    = 0x009a0000 + 10
	v4l2CIDFocusAuto        = 0x009a0000 + 12
)

// controlMap maps the platform's adjustable items to V4L2 control IDs.
//
// [An enum rather than free string keys]: string keys would let every vendor use its own favourite name
// ("exposure" / "exposure_time" / "ExposureAbsolute"), and Apps couldn't write code that works across
// devices. Vendor-specific controls go through the vendor's own interface instead of being squeezed into this table.
var controlMap = map[camerav1.ControlKind]struct {
	id   uint32
	unit string
}{
	camerav1.ControlKind_CONTROL_KIND_BRIGHTNESS:         {v4l2CIDBrightness, ""},
	camerav1.ControlKind_CONTROL_KIND_CONTRAST:           {v4l2CIDContrast, ""},
	camerav1.ControlKind_CONTROL_KIND_SATURATION:         {v4l2CIDSaturation, ""},
	camerav1.ControlKind_CONTROL_KIND_EXPOSURE_TIME:      {v4l2CIDExposureAbsolute, "100us"},
	camerav1.ControlKind_CONTROL_KIND_AUTO_EXPOSURE:      {v4l2CIDExposureAuto, ""},
	camerav1.ControlKind_CONTROL_KIND_GAIN:               {v4l2CIDGain, ""},
	camerav1.ControlKind_CONTROL_KIND_WHITE_BALANCE:      {v4l2CIDWhiteBalanceTemp, "K"},
	camerav1.ControlKind_CONTROL_KIND_AUTO_WHITE_BALANCE: {v4l2CIDAutoWhiteBalance, ""},
	camerav1.ControlKind_CONTROL_KIND_FOCUS:              {v4l2CIDFocusAbsolute, ""},
	camerav1.ControlKind_CONTROL_KIND_AUTO_FOCUS:         {v4l2CIDFocusAuto, ""},
	camerav1.ControlKind_CONTROL_KIND_ROTATION:           {v4l2CIDRotate, "deg"},
}

// controlOrder fixes the output order of ListControls / GetControls.
//
// Map iteration order is random, and iterating directly would make settings in the UI jump around on every refresh,
// a problem almost nobody would report as a bug.
var controlOrder = []camerav1.ControlKind{
	camerav1.ControlKind_CONTROL_KIND_BRIGHTNESS,
	camerav1.ControlKind_CONTROL_KIND_CONTRAST,
	camerav1.ControlKind_CONTROL_KIND_SATURATION,
	camerav1.ControlKind_CONTROL_KIND_AUTO_EXPOSURE,
	camerav1.ControlKind_CONTROL_KIND_EXPOSURE_TIME,
	camerav1.ControlKind_CONTROL_KIND_GAIN,
	camerav1.ControlKind_CONTROL_KIND_AUTO_WHITE_BALANCE,
	camerav1.ControlKind_CONTROL_KIND_WHITE_BALANCE,
	camerav1.ControlKind_CONTROL_KIND_AUTO_FOCUS,
	camerav1.ControlKind_CONTROL_KIND_FOCUS,
	camerav1.ControlKind_CONTROL_KIND_ROTATION,
}

// configEndpoint is the config side of one camera.
type configEndpoint struct {
	host *sdk.ServiceHost
	ep   *sdk.EndpointHost
	role string
	node string
	log  *slog.Logger
}

func newConfigEndpoint(
	host *sdk.ServiceHost, captureRole string, device Device, log *slog.Logger,
) *configEndpoint {
	role := boardprofile.ConfigRole(captureRole)
	return &configEndpoint{
		host: host,
		role: role,
		node: device.Node,
		log:  log.With("role", role, "node", device.Node),
	}
}

func (c *configEndpoint) install(ctx context.Context, schemaHash []byte) error {
	c.ep = c.host.NewEndpoint(sdk.RegisterRequest{
		InterfaceID:    interfaceCameraConfig,
		Major:          1,
		SchemaHash:     schemaHash,
		ResourceHandle: c.role,
	})
	for id, fn := range map[uint32]sdk.Handler{
		uint32(camerav1.CameraConfigMethod_CAMERA_CONFIG_METHOD_LIST_CONTROLS): c.listControls,
		uint32(camerav1.CameraConfigMethod_CAMERA_CONFIG_METHOD_GET_CONTROLS):  c.getControls,
		uint32(camerav1.CameraConfigMethod_CAMERA_CONFIG_METHOD_SET_CONTROLS):  c.setControls,
	} {
		if err := c.ep.Handle(id, fn); err != nil {
			return err
		}
	}
	endpointID, err := c.ep.Register(ctx)
	if err != nil {
		return fmt.Errorf("camerad: register config endpoint %s: %w", c.role, err)
	}
	c.log.Info("camerad: config endpoint registered", "endpoint_id", endpointID)
	return nil
}

// withDevice opens the device, runs fn and closes it. See "The device is reopened on every call" at the top.
func (c *configEndpoint) withDevice(fn func(*v4l2Device) error) error {
	dev, err := openDevice(c.node)
	if err != nil {
		return deviceUnavailable()
	}
	defer func() {
		if closeErr := dev.Close(); closeErr != nil {
			c.log.Warn("camerad: closing the config device handle failed", "err", closeErr)
		}
	}()
	return fn(dev)
}

func (c *configEndpoint) listControls(_ sdk.CallContext, _ []byte) ([]byte, error) {
	out := &camerav1.ControlDescriptions{}
	err := c.withDevice(func(dev *v4l2Device) error {
		for _, kind := range controlOrder {
			entry := controlMap[kind]
			info, ok := dev.queryControl(entry.id)
			if !ok {
				// This device doesn't have this control. [Skip it rather than report an all-zero range]:
				// an all-zero range would make the UI draw a slider that won't move.
				continue
			}
			out.Controls = append(out.Controls, &camerav1.ControlDescription{
				Kind:         kind,
				Min:          int64(info.Min),
				Max:          int64(info.Max),
				Step:         int64(info.Step),
				DefaultValue: int64(info.Default),
				Unit:         entry.unit,
				ReadOnly:     info.ReadOnly,
			})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return proto.Marshal(out)
}

func (c *configEndpoint) getControls(_ sdk.CallContext, payload []byte) ([]byte, error) {
	var req camerav1.GetControlsRequest
	if err := proto.Unmarshal(payload, &req); err != nil {
		return nil, cameraError(ipcv1.StatusCode_STATUS_CODE_INVALID_ARGUMENT,
			camerav1.CameraReason_CAMERA_REASON_UNSPECIFIED,
			camerav1.ControlKind_CONTROL_KIND_UNSPECIFIED)
	}

	wanted := req.GetKinds()
	if len(wanted) == 0 {
		wanted = controlOrder
	}

	out := &camerav1.ControlValues{}
	err := c.withDevice(func(dev *v4l2Device) error {
		for _, kind := range wanted {
			entry, known := controlMap[kind]
			if !known {
				continue
			}
			if _, ok := dev.queryControl(entry.id); !ok {
				continue
			}
			value, err := dev.getControl(entry.id)
			if err != nil {
				// One control failing to read shouldn't fail the whole query: when a device has one broken
				// control, the UI should still be able to show the rest.
				c.log.Debug("camerad: reading control failed", "kind", kind.String(), "err", err)
				continue
			}
			out.Values = append(out.Values, &camerav1.ControlValue{
				Kind: kind, Value: int64(value),
			})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return proto.Marshal(out)
}

// setControls changes settings. Needs an exclusive lease (camera.proto declares requires_control_lease).
//
// # Validate every item, fail the whole batch
//
// One item out of range refuses the whole batch, with no "set what can be set". Partial success would hand the caller a failure
// without knowing what state the device is in: exposure changed, gain not, a picture neither old nor new.
//
// # The response is [the value read back], not an echo of the request
//
// The device may align 105 to the step at 100. Echoing the requested value would make the UI show a number the device doesn't
// have, and the next GetControls would jump back.
func (c *configEndpoint) setControls(cc sdk.CallContext, payload []byte) ([]byte, error) {
	var req camerav1.SetControlsRequest
	if err := proto.Unmarshal(payload, &req); err != nil {
		return nil, cameraError(ipcv1.StatusCode_STATUS_CODE_INVALID_ARGUMENT,
			camerav1.CameraReason_CAMERA_REASON_UNSPECIFIED,
			camerav1.ControlKind_CONTROL_KIND_UNSPECIFIED)
	}
	// [Re-check the lease, fail closed]: nervud has already decided (requires_control_lease),
	// and this looks once more at whether the ExecutionContext really carries a lease. It isn't a second decision;
	// it stops a silent pass if "this service is connected to an old kernel without the execution gate".
	if cc.Execution.GetLeaseId() == 0 {
		return nil, cameraError(ipcv1.StatusCode_STATUS_CODE_FAILED_PRECONDITION,
			camerav1.CameraReason_CAMERA_REASON_UNSPECIFIED,
			camerav1.ControlKind_CONTROL_KIND_UNSPECIFIED)
	}

	out := &camerav1.ControlValues{}
	err := c.withDevice(func(dev *v4l2Device) error {
		// Validate everything first, then apply everything, to avoid partial success.
		type pending struct {
			kind  camerav1.ControlKind
			id    uint32
			value int32
		}
		batch := make([]pending, 0, len(req.GetValues()))

		for _, want := range req.GetValues() {
			entry, known := controlMap[want.GetKind()]
			if !known {
				return cameraError(ipcv1.StatusCode_STATUS_CODE_FAILED_PRECONDITION,
					camerav1.CameraReason_CAMERA_REASON_CONTROL_NOT_SUPPORTED, want.GetKind())
			}
			info, ok := dev.queryControl(entry.id)
			if !ok {
				return cameraError(ipcv1.StatusCode_STATUS_CODE_FAILED_PRECONDITION,
					camerav1.CameraReason_CAMERA_REASON_CONTROL_NOT_SUPPORTED, want.GetKind())
			}
			if info.ReadOnly {
				return cameraError(ipcv1.StatusCode_STATUS_CODE_FAILED_PRECONDITION,
					camerav1.CameraReason_CAMERA_REASON_CONTROL_READ_ONLY, want.GetKind())
			}
			value := want.GetValue()
			if value < int64(info.Min) || value > int64(info.Max) {
				// [Out of range must be an error, not clamped]. After clamping the UI shows the value the user set
				// while the device has another, and the two never reconcile on their own.
				return cameraError(ipcv1.StatusCode_STATUS_CODE_INVALID_ARGUMENT,
					camerav1.CameraReason_CAMERA_REASON_CONTROL_OUT_OF_RANGE, want.GetKind())
			}
			batch = append(batch, pending{kind: want.GetKind(), id: entry.id, value: int32(value)})
		}

		for _, item := range batch {
			if err := dev.setControl(item.id, item.value); err != nil {
				c.log.Warn("camerad: writing control failed", "kind", item.kind.String(), "err", err)
				return cameraError(ipcv1.StatusCode_STATUS_CODE_INTERNAL,
					camerav1.CameraReason_CAMERA_REASON_DRIVER_FAULT, item.kind)
			}
		}

		// Read back: the device may have aligned the value to its step.
		for _, item := range batch {
			actual, err := dev.getControl(item.id)
			if err != nil {
				continue
			}
			out.Values = append(out.Values, &camerav1.ControlValue{
				Kind: item.kind, Value: int64(actual),
			})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	response, err := proto.Marshal(out)
	if err != nil {
		return nil, err
	}
	// The effective values changed; tell the other subscribers (a UI may have the settings page open).
	c.publishChanged(out)
	return response, nil
}

func (c *configEndpoint) publishChanged(values *camerav1.ControlValues) {
	if c.ep == nil {
		return
	}
	payload, err := proto.Marshal(values)
	if err != nil {
		return
	}
	if err := c.ep.PublishEvent(
		uint32(camerav1.CameraConfigEvent_CAMERA_CONFIG_EVENT_CONTROLS_CHANGED), payload,
	); err != nil {
		c.log.Debug("camerad: publishing ControlsChanged failed", "err", err)
	}
}
