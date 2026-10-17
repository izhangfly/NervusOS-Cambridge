#!/usr/bin/env python3
"""Use python-can's NATIVE GsUsbBus (no shim) — does IT reconnect cross-proc?"""
import sys
import time
import platform
from usb.backend import libusb1

# No piper_mac_can import — use native gs_usb backend directly.
import can

bus = can.interface.Bus(channel=0, bustype="gs_usb", bitrate=1000000, index=0)
time.sleep(0.5)
n = 0
t0 = time.time()
while time.time() - t0 < 2.0:
    m = bus.recv(timeout=0.1)
    if m:
        n += 1
print("native frames in 2s:", n)
bus.shutdown()
