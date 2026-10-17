#!/usr/bin/env python3
"""Why is the bus silent? Scan bitrates and classify what comes back.

`piper_can_check.py` answers "are there PiPER frames at 1 Mbit/s?". When the
answer is no, this says WHY, by separating causes that look identical from the
outside:

    frames at 1M                -> bus is healthy; the problem is above CAN
    frames only at another rate -> arm is not at 1 Mbit/s (bitrate mismatch)
    error frames, no data       -> wired and clocked, but no valid traffic:
                                   bitrate mismatch or CAN-H/CAN-L swapped
    absolutely nothing, any rate-> no power, or no electrical connection

Run with NOTHING else touching the adapter (stop the portal and the teleop
server first -- a second process holding it produces short reads that look
like corruption).
"""
import sys
import time

import usb.core
import usb.util
from gs_usb.gs_usb import GsUsb
from gs_usb.constants import GS_CAN_MODE_HW_TIMESTAMP

# python-can's error-frame marker in the CAN id field.
CAN_ERR_FLAG = 0x20000000
CAN_RTR_FLAG = 0x40000000
CAN_EFF_FLAG = 0x80000000

PIPER_IDS = {0x2A1, 0x2A2, 0x2A3, 0x2A4, 0x2A5, 0x2A6, 0x2A7, 0x2A8,
             0x251, 0x252, 0x253, 0x254, 0x255, 0x256}

RATES = [1000000, 500000, 250000, 125000]
LISTEN_S = 2.5


def scan_one(bitrate):
    devs = GsUsb.scan()
    if not devs:
        print("  no adapter (did another process grab it?)")
        return None
    dev = devs[0]
    try:
        if not dev.set_bitrate(bitrate):
            print(f"  {bitrate//1000:>4} kbit/s : set_bitrate FAILED")
            return None
        dev.start(0)                       # flags=0 -> 20-byte frames
    except Exception as e:
        print(f"  {bitrate//1000:>4} kbit/s : start failed {type(e).__name__}: {e}")
        return None

    hw = bool((dev.device_flags or 0) & GS_CAN_MODE_HW_TIMESTAMP)
    size = 24 if hw else 20

    data_ids, err, short, timeouts = {}, 0, 0, 0
    t0 = time.time()
    while time.time() - t0 < LISTEN_S:
        try:
            raw = dev.gs_usb.read(0x81, size, 100)
        except usb.core.USBError:
            timeouts += 1
            continue
        if len(raw) < size:
            short += 1
            continue
        # gs_usb frame: echo_id(4) can_id(4) can_dlc(1) channel(1) flags(1) ...
        can_id = int.from_bytes(bytes(raw[4:8]), "little")
        if can_id & CAN_ERR_FLAG:
            err += 1
        else:
            cid = can_id & 0x1FFFFFFF if can_id & CAN_EFF_FLAG else can_id & 0x7FF
            data_ids[cid] = data_ids.get(cid, 0) + 1

    try:
        dev.stop()
    except Exception:
        pass
    usb.util.dispose_resources(dev.gs_usb)

    total = sum(data_ids.values())
    piper = sum(v for k, v in data_ids.items() if k in PIPER_IDS)
    print(f"  {bitrate//1000:>4} kbit/s : data={total:<6} piper={piper:<6} "
          f"err_frames={err:<5} short={short:<4} timeouts={timeouts}")
    if data_ids:
        top = sorted(data_ids.items(), key=lambda kv: -kv[1])[:6]
        print("              ids: " + ", ".join(f"0x{k:03X}x{v}" for k, v in top))
    return {"bitrate": bitrate, "data": total, "piper": piper, "err": err}


def main():
    print(__doc__.split("Run with")[0].rstrip())
    devs = GsUsb.scan()
    if not devs:
        print("\nNO ADAPTER FOUND. Plug in the candleLight stick.")
        return 1
    d = devs[0]
    try:
        print(f"\nadapter: bus={d.bus} addr={d.address} serial={d.serial_number}")
    except Exception:
        print("\nadapter present but will not answer descriptor reads -> replug it.")
        return 1
    usb.util.dispose_resources(d.gs_usb)

    print(f"\nlistening {LISTEN_S}s per bitrate:")
    results = [r for r in (scan_one(b) for b in RATES) if r]

    print("\n=== verdict ===")
    hit = next((r for r in results if r["piper"]), None)
    anydata = next((r for r in results if r["data"]), None)
    anyerr = next((r for r in results if r["err"]), None)

    if hit and hit["bitrate"] == 1000000:
        print("  PiPER frames at 1 Mbit/s. The bus is healthy.")
    elif hit:
        print(f"  PiPER frames at {hit['bitrate']//1000} kbit/s, NOT 1 Mbit/s.")
        print("  -> bitrate mismatch. The arm is not configured for 1 Mbit/s.")
    elif anydata:
        print(f"  Traffic at {anydata['bitrate']//1000} kbit/s but no PiPER ids.")
        print("  -> something else is on this bus, or the arm uses other ids.")
    elif anyerr:
        print("  Error frames only, no valid data.")
        print("  -> electrically connected and clocked, but nothing decodes:")
        print("     bitrate mismatch, or CAN-H / CAN-L swapped.")
    else:
        print("  NOTHING at any bitrate -- not one frame, not one error.")
        print("  The adapter is fine, so the arm is not driving the bus at all:")
        print("     1. arm not powered (24 V brick in? base LED lit?)")
        print("     2. CAN cable not seated, or in the wrong socket on the arm")
        print("     3. emergency stop latched")
        print("     4. cable damaged -- try the other CAN port / another cable")
        print("  Note: a wrong bitrate or swapped H/L would still produce ERROR")
        print("  frames. Total silence points at power or a broken connection.")
    return 0


if __name__ == "__main__":
    sys.exit(main())
