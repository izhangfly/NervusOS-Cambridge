//go:build linux

// This file maps both ways between V4L2 fourcc codes and the platform's PixelFormat.
//
// # Why it must be both ways, and explicit
//
// One way only (V4L2→platform) would leave OpenStream unable to translate the caller's requested format back, so it could only
// guess by name. Both ways with one table looked up in reverse requires the mapping to be a bijection, and it is: a platform format
// is one definite memory layout, and so is a fourcc. Two tables would let "added a format but only
// half of it" pass unnoticed at compile time, so a test below checks both sides entry by entry.
//
// # Unknown formats are always [skipped], never mapped to UNSPECIFIED
//
// Reporting an unknown fourcc as UNSPECIFIED would give the App a format whose layout it can't interpret,
// and it would parse shared memory by it: garbage on screen, while DescribeStream "succeeded".
// Skipped, the App just doesn't see the format, which is predictable.
package main

import camerav1 "github.com/nervus-os/nervus-ipc/protocol/interface/camerav1"

// pixelFormatTable is the single source of truth for the mapping; lookups in both directions derive from it.
var pixelFormatTable = []struct {
	fourcc uint32
	proto  camerav1.PixelFormat
}{
	{v4l2PixFmtNV12, camerav1.PixelFormat_PIXEL_FORMAT_NV12},
	{v4l2PixFmtYUYV, camerav1.PixelFormat_PIXEL_FORMAT_YUYV},
	{v4l2PixFmtMJPEG, camerav1.PixelFormat_PIXEL_FORMAT_MJPEG},
	{v4l2PixFmtRGB24, camerav1.PixelFormat_PIXEL_FORMAT_RGB888},
	{v4l2PixFmtZ16, camerav1.PixelFormat_PIXEL_FORMAT_DEPTH16},
}

// pixelFormatOf translates a V4L2 fourcc to a platform format. Returns ok=false if unknown.
func pixelFormatOf(fourcc uint32) (camerav1.PixelFormat, bool) {
	for _, entry := range pixelFormatTable {
		if entry.fourcc == fourcc {
			return entry.proto, true
		}
	}
	return camerav1.PixelFormat_PIXEL_FORMAT_UNSPECIFIED, false
}

// fourccOf translates a platform format back to a V4L2 fourcc. Returns ok=false if unknown.
//
// UNSPECIFIED [must] return false: it means "the caller didn't fill it in", not a format.
// Letting it through would turn one missing field into an S_FMT call with a zero fourcc.
func fourccOf(format camerav1.PixelFormat) (uint32, bool) {
	if format == camerav1.PixelFormat_PIXEL_FORMAT_UNSPECIFIED {
		return 0, false
	}
	for _, entry := range pixelFormatTable {
		if entry.proto == format {
			return entry.fourcc, true
		}
	}
	return 0, false
}
