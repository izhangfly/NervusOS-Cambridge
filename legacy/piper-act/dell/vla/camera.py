"""
Orbbec Dabai DC1 capture.

The Dabai family speaks Orbbec's legacy OpenNI protocol, so on Windows only the
RGB module enumerates as a standard UVC device — that is the `VID_2BC5&PID_0557
&MI_00` node Windows names "Dabai DC1". Depth comes over a separate interface
(the `PID_0657` node, which sits in an Error state until a driver is bound) and
needs OrbbecSDK **v1**; SDK v2 dropped OpenNI devices entirely. RGB alone is
what OpenVLA / SmolVLA / pi0 / ACT / Diffusion Policy all consume, so this
module deliberately only does RGB and stays dependency-light.

Devices are looked up by NAME, never by index: index 0 is the laptop's
"Integrated Webcam" and the Dabai lands at index 1 here, but that order changes
whenever a USB device is added, and silently training on the wrong camera is a
miserable bug to find.
"""

import base64
import time

import cv2

DEFAULT_NAME = "Dabai DC1"


def list_cameras():
    """[(index, name)] for every DirectShow video device."""
    try:
        from pygrabber.dshow_graph import FilterGraph
        return list(enumerate(FilterGraph().get_input_devices()))
    except Exception:
        # pygrabber is Windows/DirectShow only; fall back to bare indices.
        out = []
        for i in range(8):
            cap = cv2.VideoCapture(i, cv2.CAP_DSHOW)
            if cap.isOpened():
                out.append((i, "camera %d" % i))
            cap.release()
        return out


def find_camera(name=DEFAULT_NAME):
    """Index of the first camera whose name contains `name` (case-insensitive)."""
    want = name.lower()
    for idx, dev in list_cameras():
        if want in dev.lower():
            return idx
    raise RuntimeError(
        "no camera matching %r. Present: %s"
        % (name, ", ".join("%d=%s" % (i, n) for i, n in list_cameras())))


class Camera:
    """A single RGB stream, opened by device name."""

    def __init__(self, name=DEFAULT_NAME, width=640, height=480, mjpg=True):
        self.index = find_camera(name)
        self.name = name
        self.cap = cv2.VideoCapture(self.index, cv2.CAP_DSHOW)
        if not self.cap.isOpened():
            raise RuntimeError("could not open %s (index %d)" % (name, self.index))
        # MJPG lets the camera push full frame rate over USB; the default YUY2
        # is uncompressed and measured only ~13 fps at 640x480 on this link.
        if mjpg:
            self.cap.set(cv2.CAP_PROP_FOURCC, cv2.VideoWriter_fourcc(*"MJPG"))
        self.cap.set(cv2.CAP_PROP_FRAME_WIDTH, width)
        self.cap.set(cv2.CAP_PROP_FRAME_HEIGHT, height)
        for _ in range(5):      # let auto-exposure settle before anyone looks
            self.cap.read()

    def read(self):
        """Latest BGR frame as a numpy array.

        Grabs twice: OpenCV buffers frames, and a policy acting on a stale image
        is the kind of bug that looks like bad model behaviour.
        """
        self.cap.grab()
        ok, frame = self.cap.read()
        if not ok or frame is None:
            raise RuntimeError("frame grab failed on %s" % self.name)
        return frame

    def read_jpeg_b64(self, quality=85, max_width=768):
        """Latest frame as base64 JPEG, sized for a vision model's input."""
        frame = self.read()
        h, w = frame.shape[:2]
        if w > max_width:
            frame = cv2.resize(frame, (max_width, int(h * max_width / w)),
                               interpolation=cv2.INTER_AREA)
        ok, buf = cv2.imencode(".jpg", frame,
                               [int(cv2.IMWRITE_JPEG_QUALITY), quality])
        if not ok:
            raise RuntimeError("JPEG encode failed")
        return base64.b64encode(buf.tobytes()).decode("ascii")

    def measure_fps(self, seconds=2.0):
        n, t0 = 0, time.time()
        while time.time() - t0 < seconds:
            if self.cap.read()[0]:
                n += 1
        return n / seconds

    def close(self):
        try:
            self.cap.release()
        except Exception:
            pass

    def __enter__(self):
        return self

    def __exit__(self, *_):
        self.close()


if __name__ == "__main__":
    print("cameras:", list_cameras())
    with Camera() as cam:
        print("opened %s at index %d" % (cam.name, cam.index))
        f = cam.read()
        print("frame", f.shape, "| fps", round(cam.measure_fps(), 1))
        cv2.imwrite("dabai_test.jpg", f)
        print("wrote dabai_test.jpg")
