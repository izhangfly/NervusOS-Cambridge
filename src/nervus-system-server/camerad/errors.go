//go:build linux

// This file builds failures that carry a typed error_detail.
//
// # Why not just return fmt.Errorf
//
// The SDK normalises any Go error to INTERNAL and only logs the original text locally, which is right:
// handing internal strings (device node paths, driver error codes) to the caller would leak implementation details across
// the process boundary.
//
// The cost is that callers get INTERNAL for everything and can't tell "this resolution isn't supported" (change the parameters and retry
// and it works) from "the camera dropped off" (retrying never helps). CameraErrorDetail brings that distinction
// back: the code says whether to retry, the detail says why.
package main

import (
	camerav1 "github.com/nervus-os/nervus-ipc/protocol/interface/camerav1"
	ipcv1 "github.com/nervus-os/nervus-ipc/protocol/ipcv1"
	"github.com/nervus-os/nervus-ipc/sdk"
	"google.golang.org/protobuf/proto"
)

// cameraError builds a failure carrying a CameraErrorDetail.
//
// control only matters for the CONTROL_* reasons; pass UNSPECIFIED otherwise.
func cameraError(
	code ipcv1.StatusCode,
	reason camerav1.CameraReason,
	control camerav1.ControlKind,
) error {
	detail, err := proto.Marshal(&camerav1.CameraErrorDetail{
		Reason:  reason,
		Control: control,
	})
	if err != nil {
		// Failing to encode the detail shouldn't turn the whole call into an INTERNAL with no reason:
		// the code already carries the most important information (whether to retry), so keep it.
		return &sdk.StatusError{Code: code}
	}
	return &sdk.StatusError{
		Code: code,
		// [PublicMessage stays empty]: the protocol forbids nervud from forwarding a Provider's free text,
		// so filling it in would never reach the caller and would only make people think it does something.
		Detail: detail,
	}
}

// deviceUnavailable is the single shape for "the device can't be used right now": not plugged in, dropped off, or busy.
//
// Merging the three is deliberate: the caller can do exactly the same thing in each case (wait and retry, or ask the user to check
// the connection), and telling them apart would expose "does this machine have this device", which is
// the job of nervus.interface.resource.directory and shouldn't leak as a side channel from a failed
// OpenStream.
func deviceUnavailable() error {
	return cameraError(
		ipcv1.StatusCode_STATUS_CODE_UNAVAILABLE,
		camerav1.CameraReason_CAMERA_REASON_DEVICE_UNAVAILABLE,
		camerav1.ControlKind_CONTROL_KIND_UNSPECIFIED)
}
