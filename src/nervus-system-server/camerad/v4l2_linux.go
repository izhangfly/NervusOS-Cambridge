// This file is the V4L2 compatibility layer: it wraps Linux's videodev2 ioctls in a shape camerad can use.
//
// # Why raw ioctls instead of binding a C library
//
// libv4l2 converts formats (decoding MJPEG to RGB), which is exactly what this service [must not] do:
// frames go into the shared-memory ring as they are, and the consumer decides how to decode them. A conversion in between means one more
// full-frame copy on the CPU, while robots mostly feed raw frames to neural networks.
//
// Binding a C library would also need cgo, and cross-compiling to arm64 would then need a sysroot. The ioctl struct layouts
// are a stable ABI, and hard-coding them is cheaper than pulling in a toolchain dependency.
//
// # Struct layouts and ioctl numbers
//
// [The numbers are computed, not copied]. _IOC packs the direction, type, number and struct size into one 32-bit
// number, with the size from unsafe.Sizeof. Copying a hex constant would hide a wrong struct definition:
// the ioctl would return ENOTTY, an error code pointing at "driver doesn't support it", not at
// "your struct is 4 bytes short".
package main

import (
	"errors"
	"fmt"
	"os"
	"sort"
	"unsafe"

	"golang.org/x/sys/unix"
)

// ---- ioctl encoding -----------------------------------------------------------

const (
	iocNone  = 0
	iocWrite = 1
	iocRead  = 2

	iocNRBits   = 8
	iocTypeBits = 8
	iocSizeBits = 14

	iocNRShift   = 0
	iocTypeShift = iocNRShift + iocNRBits
	iocSizeShift = iocTypeShift + iocTypeBits
	iocDirShift  = iocSizeShift + iocSizeBits
)

func ioc(dir, typ, nr, size uintptr) uintptr {
	return dir<<iocDirShift | size<<iocSizeShift | typ<<iocTypeShift | nr<<iocNRShift
}

const v4l2IOCType = 'V'

func iorV(nr, size uintptr) uintptr  { return ioc(iocRead, v4l2IOCType, nr, size) }
func iowV(nr, size uintptr) uintptr  { return ioc(iocWrite, v4l2IOCType, nr, size) }
func iowrV(nr, size uintptr) uintptr { return ioc(iocRead|iocWrite, v4l2IOCType, nr, size) }

// ---- videodev2 structs ----------------------------------------------------------
//
// Field order follows include/uapi/linux/videodev2.h exactly. Padding fields are written out explicitly instead of
// relying on the compiler: Go doesn't guarantee struct layout, and only explicit padding can claim to "match C".

type v4l2Capability struct {
	driver       [16]byte
	card         [32]byte
	busInfo      [32]byte
	version      uint32
	capabilities uint32
	deviceCaps   uint32
	reserved     [3]uint32
}

const (
	v4l2CapVideoCapture = 0x00000001
	v4l2CapStreaming    = 0x04000000
	v4l2CapDeviceCaps   = 0x80000000
)

type v4l2Fmtdesc struct {
	index       uint32
	typ         uint32
	flags       uint32
	description [32]byte
	pixelformat uint32
	reserved    [4]uint32
}

type v4l2Frmsizeenum struct {
	index       uint32
	pixelFormat uint32
	typ         uint32
	// The union of discrete and stepwise, 24 bytes. Only the first two u32 of discrete are read.
	union    [24]byte
	reserved [2]uint32
}

const v4l2FrmsizeTypeDiscrete = 1

type v4l2Frmivalenum struct {
	index       uint32
	pixelFormat uint32
	width       uint32
	height      uint32
	typ         uint32
	union       [24]byte
	reserved    [2]uint32
}

const v4l2FrmivalTypeDiscrete = 1

// v4l2Format's union has a pointer member (v4l2_window.clips), so it's 8-byte aligned:
// type must be followed by 4 bytes of padding, or the whole struct is 4 bytes short and the ioctl number comes out wrong.
type v4l2Format struct {
	typ  uint32
	_    uint32
	data [200]byte
}

type v4l2PixFormat struct {
	width        uint32
	height       uint32
	pixelformat  uint32
	field        uint32
	bytesperline uint32
	sizeimage    uint32
	colorspace   uint32
	priv         uint32
	flags        uint32
	encoding     uint32
	quantization uint32
	xferFunc     uint32
}

const (
	v4l2BufTypeVideoCapture = 1
	v4l2FieldNone           = 1
	v4l2MemoryMMAP          = 1
)

type v4l2RequestBuffers struct {
	count        uint32
	typ          uint32
	memory       uint32
	capabilities uint32
	flags        uint8
	reserved     [3]uint8
}

type v4l2Timeval struct {
	sec  int64
	usec int64
}

type v4l2Buffer struct {
	index     uint32
	typ       uint32
	bytesused uint32
	flags     uint32
	field     uint32
	_         uint32 // timestamp needs 8-byte alignment
	timestamp v4l2Timeval
	timecode  [16]byte
	sequence  uint32
	memory    uint32
	// The m union: in MMAP mode the first 4 bytes are the offset. On 64-bit the union takes 8 bytes.
	m         uint64
	length    uint32
	reserved2 uint32
	requestFD int32
	_         uint32
}

type v4l2CaptureParm struct {
	capability   uint32
	capturemode  uint32
	numerator    uint32
	denominator  uint32
	extendedmode uint32
	readbuffers  uint32
	reserved     [4]uint32
}

type v4l2StreamParm struct {
	typ  uint32
	data [200]byte
}

const v4l2CapTimePerFrame = 0x1000

type v4l2Control struct {
	id    uint32
	value int32
}

type v4l2QueryCtrl struct {
	id           uint32
	typ          uint32
	name         [32]byte
	minimum      int32
	maximum      int32
	step         int32
	defaultValue int32
	flags        uint32
	reserved     [2]uint32
}

const (
	v4l2CtrlFlagDisabled = 0x0001
	v4l2CtrlFlagReadOnly = 0x0004
	v4l2CtrlFlagGrabbed  = 0x0002
)

// ioctl numbers. All computed from struct sizes; see the top of the file.
var (
	vidiocQueryCap           = iorV(0, unsafe.Sizeof(v4l2Capability{}))
	vidiocEnumFmt            = iowrV(2, unsafe.Sizeof(v4l2Fmtdesc{}))
	vidiocGFmt               = iowrV(4, unsafe.Sizeof(v4l2Format{}))
	vidiocSFmt               = iowrV(5, unsafe.Sizeof(v4l2Format{}))
	vidiocReqbufs            = iowrV(8, unsafe.Sizeof(v4l2RequestBuffers{}))
	vidiocQuerybuf           = iowrV(9, unsafe.Sizeof(v4l2Buffer{}))
	vidiocQbuf               = iowrV(15, unsafe.Sizeof(v4l2Buffer{}))
	vidiocDqbuf              = iowrV(17, unsafe.Sizeof(v4l2Buffer{}))
	vidiocStreamon           = iowV(18, unsafe.Sizeof(int32(0)))
	vidiocStreamoff          = iowV(19, unsafe.Sizeof(int32(0)))
	vidiocSParm              = iowrV(22, unsafe.Sizeof(v4l2StreamParm{}))
	vidiocGCtrl              = iowrV(27, unsafe.Sizeof(v4l2Control{}))
	vidiocSCtrl              = iowrV(28, unsafe.Sizeof(v4l2Control{}))
	vidiocQueryctrl          = iowrV(36, unsafe.Sizeof(v4l2QueryCtrl{}))
	vidiocEnumFramesizes     = iowrV(74, unsafe.Sizeof(v4l2Frmsizeenum{}))
	vidiocEnumFrameintervals = iowrV(75, unsafe.Sizeof(v4l2Frmivalenum{}))
)

// ioctlPtr issues one ioctl.
//
// [EINTR must be retried]: the Go runtime sends threads pre-emption signals, and an interrupted ioctl returns
// EINTR. Without the retry you get random, unreproducible capture failures, especially under GC pressure.
func ioctlPtr(fd int, request uintptr, arg unsafe.Pointer) error {
	for {
		_, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), request, uintptr(arg))
		if errno == 0 {
			return nil
		}
		if errno == unix.EINTR {
			continue
		}
		return errno
	}
}

// ---- device -----------------------------------------------------------------

// v4l2Device is an open capture device. Not concurrency-safe; used serially by stream.
type v4l2Device struct {
	fd   int
	node string

	// buffers are the mmapped capture buffers. Each must be munmapped after STREAMOFF.
	buffers [][]byte
	// streaming records whether STREAMON was issued, to avoid EINVAL from a repeated STREAMOFF.
	streaming bool
}

func openDevice(node string) (*v4l2Device, error) {
	// O_NONBLOCK: DQBUF returns EAGAIN at once when there's no frame, and the layer above waits with poll.
	// In blocking mode a camera that dropped off would leave the goroutine stuck in DQBUF forever,
	// where even CloseStream couldn't move it.
	fd, err := unix.Open(node, unix.O_RDWR|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("camerad: open %s: %w", node, err)
	}
	return &v4l2Device{fd: fd, node: node}, nil
}

func (d *v4l2Device) Close() error {
	if d == nil || d.fd < 0 {
		return nil
	}
	_ = d.stopStreaming()
	err := unix.Close(d.fd)
	d.fd = -1
	return err
}

// capabilities returns the device's effective capability bits.
//
// [Prefer device_caps over capabilities]: the latter is the union for the whole physical device,
// so a UVC camera that registered both a capture node and a metadata node reports identical
// capabilities on both. Only device_caps says what "this particular node can do".
func (d *v4l2Device) capabilities() (uint32, error) {
	var cap v4l2Capability
	if err := ioctlPtr(d.fd, vidiocQueryCap, unsafe.Pointer(&cap)); err != nil {
		return 0, fmt.Errorf("camerad: QUERYCAP %s: %w", d.node, err)
	}
	if cap.capabilities&v4l2CapDeviceCaps != 0 {
		return cap.deviceCaps, nil
	}
	return cap.capabilities, nil
}

// canCapture reports whether a node can really stream.
//
// UVC devices often register two nodes: one for capture, one for metadata. Without filtering, half the devices would
// open fine and never produce a frame, and the symptom would be "the camera is broken"; nobody would think the wrong node was picked.
func canCapture(node string) bool {
	dev, err := openDevice(node)
	if err != nil {
		return false
	}
	defer func() { _ = dev.Close() }()

	caps, err := dev.capabilities()
	if err != nil {
		return false
	}
	return caps&v4l2CapVideoCapture != 0 && caps&v4l2CapStreaming != 0
}

// ---- format enumeration -----------------------------------------------------

// formatOption is one usable streaming configuration.
type formatOption struct {
	PixelFormat uint32
	Width       uint32
	Height      uint32
	FrameRates  []uint32
	MaxBytes    uint32
}

// enumerateFormats lists every discrete configuration the device supports.
//
// [Discrete sizes only, no stepwise]. Stepwise devices (a few webcams, most CSI bridges)
// allow any resolution, and expanding that into a list would produce thousands of entries. Negotiating any size needs a "request →
// driver aligns → report the actual value" round trip, and this interface deliberately doesn't negotiate sizes (see camera.proto's
// OpenStreamRequest: no match means failure, with no silent downgrade).
//
// The cost is that stepwise devices can only be supported through nervus.camerad's private configuration. That's a known trade-off,
// not an omission.
func (d *v4l2Device) enumerateFormats() ([]formatOption, error) {
	var options []formatOption

	for index := uint32(0); ; index++ {
		desc := v4l2Fmtdesc{index: index, typ: v4l2BufTypeVideoCapture}
		if err := ioctlPtr(d.fd, vidiocEnumFmt, unsafe.Pointer(&desc)); err != nil {
			if errors.Is(err, unix.EINVAL) {
				break // end of the enumeration: a normal stop condition, not an error
			}
			return nil, fmt.Errorf("camerad: ENUM_FMT %s: %w", d.node, err)
		}
		sizes, err := d.enumerateSizes(desc.pixelformat)
		if err != nil {
			return nil, err
		}
		options = append(options, sizes...)
	}

	sort.Slice(options, func(i, j int) bool {
		if options[i].PixelFormat != options[j].PixelFormat {
			return options[i].PixelFormat < options[j].PixelFormat
		}
		if options[i].Width != options[j].Width {
			return options[i].Width < options[j].Width
		}
		return options[i].Height < options[j].Height
	})
	return options, nil
}

func (d *v4l2Device) enumerateSizes(pixelFormat uint32) ([]formatOption, error) {
	var options []formatOption
	for index := uint32(0); ; index++ {
		enum := v4l2Frmsizeenum{index: index, pixelFormat: pixelFormat}
		if err := ioctlPtr(d.fd, vidiocEnumFramesizes, unsafe.Pointer(&enum)); err != nil {
			if errors.Is(err, unix.EINVAL) {
				break
			}
			return nil, fmt.Errorf("camerad: ENUM_FRAMESIZES %s: %w", d.node, err)
		}
		if enum.typ != v4l2FrmsizeTypeDiscrete {
			break // stepwise/continuous: see enumerateFormats
		}
		width := *(*uint32)(unsafe.Pointer(&enum.union[0]))
		height := *(*uint32)(unsafe.Pointer(&enum.union[4]))

		rates, err := d.enumerateRates(pixelFormat, width, height)
		if err != nil {
			return nil, err
		}
		options = append(options, formatOption{
			PixelFormat: pixelFormat,
			Width:       width,
			Height:      height,
			FrameRates:  rates,
			MaxBytes:    maxFrameBytes(pixelFormat, width, height),
		})
	}
	return options, nil
}

func (d *v4l2Device) enumerateRates(pixelFormat, width, height uint32) ([]uint32, error) {
	var rates []uint32
	for index := uint32(0); ; index++ {
		enum := v4l2Frmivalenum{
			index: index, pixelFormat: pixelFormat, width: width, height: height,
		}
		if err := ioctlPtr(d.fd, vidiocEnumFrameintervals, unsafe.Pointer(&enum)); err != nil {
			if errors.Is(err, unix.EINVAL) {
				break
			}
			return nil, fmt.Errorf("camerad: ENUM_FRAMEINTERVALS %s: %w", d.node, err)
		}
		if enum.typ != v4l2FrmivalTypeDiscrete {
			break
		}
		// The interval is numerator/denominator seconds; the frame rate is its inverse.
		numerator := *(*uint32)(unsafe.Pointer(&enum.union[0]))
		denominator := *(*uint32)(unsafe.Pointer(&enum.union[4]))
		if numerator == 0 {
			continue
		}
		rates = append(rates, denominator/numerator)
	}
	sort.Slice(rates, func(i, j int) bool { return rates[i] < rates[j] })
	return rates, nil
}

// maxFrameBytes gives the upper bound of one frame in bytes, used to size the shared-memory slots.
//
// Compressed formats (MJPEG) have no fixed frame size, so half the uncompressed size is the bound: JPEG at this
// resolution practically never fails to compress 2:1, and [better too much than not enough]: a frame that doesn't fit a slot
// can only be dropped, which shows up as random stutter.
func maxFrameBytes(pixelFormat, width, height uint32) uint32 {
	pixels := width * height
	switch pixelFormat {
	case v4l2PixFmtNV12:
		return pixels * 3 / 2
	case v4l2PixFmtYUYV:
		return pixels * 2
	case v4l2PixFmtRGB24:
		return pixels * 3
	case v4l2PixFmtZ16:
		return pixels * 2
	case v4l2PixFmtMJPEG:
		return pixels * 3 / 2
	default:
		return pixels * 4
	}
}

// V4L2 fourcc, built with the v4l2_fourcc macro's algorithm rather than copied as hex.
const (
	v4l2PixFmtNV12  = 'N' | 'V'<<8 | '1'<<16 | '2'<<24
	v4l2PixFmtYUYV  = 'Y' | 'U'<<8 | 'Y'<<16 | 'V'<<24
	v4l2PixFmtMJPEG = 'M' | 'J'<<8 | 'P'<<16 | 'G'<<24
	v4l2PixFmtRGB24 = 'R' | 'G'<<8 | 'B'<<16 | '3'<<24
	v4l2PixFmtZ16   = 'Z' | '1'<<8 | '6'<<16 | ' '<<24
)

// ---- streaming ----------------------------------------------------------------

// setFormat sets the streaming format and returns what the driver actually accepted.
//
// [It must read back]: S_FMT is a negotiation, not a setting. The driver changes an unsupported request to the nearest
// supported value and writes it back into the same struct [without an error]. Without reading back we'd parse memory
// laid out at another size using the requested one.
func (d *v4l2Device) setFormat(pixelFormat, width, height uint32) (v4l2PixFormat, error) {
	format := v4l2Format{typ: v4l2BufTypeVideoCapture}
	pix := v4l2PixFormat{
		width:       width,
		height:      height,
		pixelformat: pixelFormat,
		field:       v4l2FieldNone,
	}
	*(*v4l2PixFormat)(unsafe.Pointer(&format.data[0])) = pix

	if err := ioctlPtr(d.fd, vidiocSFmt, unsafe.Pointer(&format)); err != nil {
		return v4l2PixFormat{}, fmt.Errorf("camerad: S_FMT %s: %w", d.node, err)
	}
	return *(*v4l2PixFormat)(unsafe.Pointer(&format.data[0])), nil
}

// setFrameRate sets the frame rate. If the driver doesn't support S_PARM, it's silently skipped.
//
// [Not supported isn't an error]: many CSI bridges have a fixed frame rate and accept no setting. Refusing to stream over that would make
// a whole class of devices unusable when they'd otherwise stream fine. The caller learns the actual frame rate from
// DescribeStream.
func (d *v4l2Device) setFrameRate(fps uint32) {
	if fps == 0 {
		return
	}
	parm := v4l2StreamParm{typ: v4l2BufTypeVideoCapture}
	capture := v4l2CaptureParm{numerator: 1, denominator: fps}
	*(*v4l2CaptureParm)(unsafe.Pointer(&parm.data[0])) = capture
	_ = ioctlPtr(d.fd, vidiocSParm, unsafe.Pointer(&parm))
}

// startStreaming requests buffers, mmaps them, queues them and starts streaming.
func (d *v4l2Device) startStreaming(bufferCount uint32) error {
	req := v4l2RequestBuffers{
		count:  bufferCount,
		typ:    v4l2BufTypeVideoCapture,
		memory: v4l2MemoryMMAP,
	}
	if err := ioctlPtr(d.fd, vidiocReqbufs, unsafe.Pointer(&req)); err != nil {
		return fmt.Errorf("camerad: REQBUFS %s: %w", d.node, err)
	}
	if req.count < 2 {
		// A single buffer means we'd be reading the same memory the driver is writing.
		return fmt.Errorf("camerad: %s only gave %d buffers; at least 2 are needed", d.node, req.count)
	}

	for i := uint32(0); i < req.count; i++ {
		buf := v4l2Buffer{index: i, typ: v4l2BufTypeVideoCapture, memory: v4l2MemoryMMAP}
		if err := ioctlPtr(d.fd, vidiocQuerybuf, unsafe.Pointer(&buf)); err != nil {
			d.unmapAll()
			return fmt.Errorf("camerad: QUERYBUF %s[%d]: %w", d.node, i, err)
		}
		// In MMAP mode the first 4 bytes of the m union are the offset.
		offset := int64(uint32(buf.m))
		mem, err := unix.Mmap(d.fd, offset, int(buf.length),
			unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
		if err != nil {
			d.unmapAll()
			return fmt.Errorf("camerad: mmap %s[%d]: %w", d.node, i, err)
		}
		d.buffers = append(d.buffers, mem)

		if err := ioctlPtr(d.fd, vidiocQbuf, unsafe.Pointer(&buf)); err != nil {
			d.unmapAll()
			return fmt.Errorf("camerad: QBUF %s[%d]: %w", d.node, i, err)
		}
	}

	typ := int32(v4l2BufTypeVideoCapture)
	if err := ioctlPtr(d.fd, vidiocStreamon, unsafe.Pointer(&typ)); err != nil {
		d.unmapAll()
		return fmt.Errorf("camerad: STREAMON %s: %w", d.node, err)
	}
	d.streaming = true
	return nil
}

func (d *v4l2Device) stopStreaming() error {
	if !d.streaming {
		d.unmapAll()
		return nil
	}
	typ := int32(v4l2BufTypeVideoCapture)
	err := ioctlPtr(d.fd, vidiocStreamoff, unsafe.Pointer(&typ))
	d.streaming = false
	// [munmap whether or not STREAMOFF succeeded]: on failure the mappings still exist,
	// and leaking them would slowly exhaust the address space of a process that keeps opening and closing streams.
	d.unmapAll()
	if err != nil {
		return fmt.Errorf("camerad: STREAMOFF %s: %w", d.node, err)
	}
	return nil
}

func (d *v4l2Device) unmapAll() {
	for _, mem := range d.buffers {
		_ = unix.Munmap(mem)
	}
	d.buffers = nil
}

// capturedFrame is a captured frame not yet returned to the driver.
//
// Data points at the mmapped buffer and [is valid until release]. Don't let go before copying it elsewhere:
// after release the driver may write the next frame into the same memory at any moment.
type capturedFrame struct {
	Data []byte
	// MonotonicNanos is the capture time stamped by the driver, in the CLOCK_MONOTONIC domain.
	//
	// It's the only meaningful timestamp: [the moment the sensor produced the frame], without any of the queueing
	// delay after it. camerad passes it through unchanged into the frame header in the shared-memory ring.
	MonotonicNanos uint64
	Sequence       uint32
	index          uint32
}

// errNoFrame means there's no frame to take right now; not an error state.
var errNoFrame = errors.New("camerad: no frame ready")

// dequeue takes one frame. Returns errNoFrame when there's none.
func (d *v4l2Device) dequeue() (capturedFrame, error) {
	buf := v4l2Buffer{typ: v4l2BufTypeVideoCapture, memory: v4l2MemoryMMAP}
	if err := ioctlPtr(d.fd, vidiocDqbuf, unsafe.Pointer(&buf)); err != nil {
		if errors.Is(err, unix.EAGAIN) {
			return capturedFrame{}, errNoFrame
		}
		return capturedFrame{}, fmt.Errorf("camerad: DQBUF %s: %w", d.node, err)
	}
	if int(buf.index) >= len(d.buffers) {
		// The driver gave an index we never mapped. Using it would read out of bounds.
		return capturedFrame{}, fmt.Errorf(
			"camerad: DQBUF %s returned out-of-range index %d (%d mapped)",
			d.node, buf.index, len(d.buffers))
	}
	mem := d.buffers[buf.index]
	if int(buf.bytesused) > len(mem) {
		return capturedFrame{}, fmt.Errorf(
			"camerad: DQBUF %s claims %d bytes, but the buffer only has %d",
			d.node, buf.bytesused, len(mem))
	}
	return capturedFrame{
		Data:           mem[:buf.bytesused],
		MonotonicNanos: uint64(buf.timestamp.sec)*1e9 + uint64(buf.timestamp.usec)*1e3,
		Sequence:       buf.sequence,
		index:          buf.index,
	}, nil
}

// release returns the buffer to the driver. [It must be called], or once the buffer pool is drained no more frames can be taken.
func (d *v4l2Device) release(frame capturedFrame) error {
	buf := v4l2Buffer{
		index:  frame.index,
		typ:    v4l2BufTypeVideoCapture,
		memory: v4l2MemoryMMAP,
	}
	if err := ioctlPtr(d.fd, vidiocQbuf, unsafe.Pointer(&buf)); err != nil {
		return fmt.Errorf("camerad: QBUF %s[%d]: %w", d.node, frame.index, err)
	}
	return nil
}

// waitFrame waits until there's a frame to take, or the timeout.
func (d *v4l2Device) waitFrame(timeoutMillis int) error {
	fds := []unix.PollFd{{Fd: int32(d.fd), Events: unix.POLLIN}}
	for {
		n, err := unix.Poll(fds, timeoutMillis)
		if err != nil {
			if errors.Is(err, unix.EINTR) {
				continue
			}
			return fmt.Errorf("camerad: poll %s: %w", d.node, err)
		}
		if n == 0 {
			return os.ErrDeadlineExceeded
		}
		if fds[0].Revents&(unix.POLLERR|unix.POLLHUP|unix.POLLNVAL) != 0 {
			// The device dropped off. More DQBUF calls would only return meaningless error codes.
			return fmt.Errorf("camerad: %s was disconnected", d.node)
		}
		return nil
	}
}

// ---- controls -----------------------------------------------------------------

type controlInfo struct {
	ID       uint32
	Min      int32
	Max      int32
	Step     int32
	Default  int32
	ReadOnly bool
}

// queryControl reads a control's range. Returns ok=false if unsupported.
func (d *v4l2Device) queryControl(id uint32) (controlInfo, bool) {
	query := v4l2QueryCtrl{id: id}
	if err := ioctlPtr(d.fd, vidiocQueryctrl, unsafe.Pointer(&query)); err != nil {
		return controlInfo{}, false
	}
	if query.flags&v4l2CtrlFlagDisabled != 0 {
		return controlInfo{}, false
	}
	return controlInfo{
		ID:      id,
		Min:     query.minimum,
		Max:     query.maximum,
		Step:    query.step,
		Default: query.defaultValue,
		// GRABBED counts as read-only too: it means streaming is in progress and it can't be changed.
		ReadOnly: query.flags&(v4l2CtrlFlagReadOnly|v4l2CtrlFlagGrabbed) != 0,
	}, true
}

func (d *v4l2Device) getControl(id uint32) (int32, error) {
	ctrl := v4l2Control{id: id}
	if err := ioctlPtr(d.fd, vidiocGCtrl, unsafe.Pointer(&ctrl)); err != nil {
		return 0, fmt.Errorf("camerad: G_CTRL %s id=%#x: %w", d.node, id, err)
	}
	return ctrl.value, nil
}

func (d *v4l2Device) setControl(id uint32, value int32) error {
	ctrl := v4l2Control{id: id, value: value}
	if err := ioctlPtr(d.fd, vidiocSCtrl, unsafe.Pointer(&ctrl)); err != nil {
		return fmt.Errorf("camerad: S_CTRL %s id=%#x: %w", d.node, id, err)
	}
	return nil
}
