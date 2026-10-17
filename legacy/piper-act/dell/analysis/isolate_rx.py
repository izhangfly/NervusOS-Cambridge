#!/usr/bin/env python3
"""Which step of the shim's open sequence kills RX?

Raw open reads 2130 frames/s. Through the shim, RX dies after ~11 frames.
The shim does three extra things before handing the device over: _drain(),
_tx_probe() (a bulk-OUT write), and starting an I/O thread. Run each prefix
and see which one stops the frames.
"""
import sys
import time

import usb.core
import usb.util
from gs_usb.gs_usb import GsUsb
from gs_usb.gs_usb_frame import GsUsbFrame, GS_USB_NONE_ECHO_ID
from gs_usb.constants import GS_CAN_MODE_HW_TIMESTAMP

BITRATE = 1_000_000
TX_TIMEOUT_MS = 30


def fresh():
    devs = GsUsb.scan()
    if not devs:
        print("no adapter")
        sys.exit(1)
    d = devs[0]
    d.set_bitrate(BITRATE)
    d.start(0)
    return d


def read_for(dev, seconds, label):
    f = GsUsbFrame()
    n = err = echo = 0
    t0 = time.time()
    while time.time() - t0 < seconds:
        try:
            if not dev.read(f, 100):
                continue
        except Exception as e:
            err += 1
            if err <= 2:
                print(f"      exc: {type(e).__name__}: {e}")
            continue
        if f.echo_id != GS_USB_NONE_ECHO_ID:
            echo += 1
            continue
        n += 1
    print(f"    {label}: {n} frames in {seconds}s ({n/seconds:.0f}/s), "
          f"echo={echo} exc={err}")
    return n


def drain(dev, seconds):
    f = GsUsbFrame()
    n = 0
    t0 = time.time()
    while time.time() - t0 < seconds:
        try:
            if not dev.read(f, 20):
                break
        except Exception:
            break
        n += 1
    print(f"    drained {n} frames")
    return n


def tx_probe(dev):
    hw = bool((dev.device_flags or 0) & GS_CAN_MODE_HW_TIMESTAMP)
    probe = GsUsbFrame(can_id=0x4AF, data=[0x01, 0, 0, 0, 0, 0, 0, 0])
    for _ in range(2):
        try:
            dev.gs_usb.write(0x02, probe.pack(hw), TX_TIMEOUT_MS)
            print(f"    tx_probe: write OK (hw_ts={hw})")
            return True
        except Exception as e:
            print(f"    tx_probe: {type(e).__name__}: {e}")
            time.sleep(0.05)
    return False


def release(dev):
    try:
        dev.stop()
    except Exception:
        pass
    usb.util.dispose_resources(dev.gs_usb)
    time.sleep(0.4)


print("A. start(0) then read  [known-good baseline]")
d = fresh()
read_for(d, 3, "read")
release(d)

print("\nB. start(0) + drain(0.4) then read")
d = fresh()
drain(d, 0.4)
read_for(d, 3, "read")
release(d)

print("\nC. start(0) + drain(0.4) + tx_probe then read  [full shim sequence]")
d = fresh()
drain(d, 0.4)
tx_probe(d)
read_for(d, 3, "read")
release(d)

print("\nD. start(0) + tx_probe only, then read")
d = fresh()
tx_probe(d)
read_for(d, 3, "read")
release(d)
