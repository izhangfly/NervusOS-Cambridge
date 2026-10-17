#!/usr/bin/env python3
"""Which AgileX SDK is correct for THIS arm? Reads the firmware version.

AgileX split their stack by firmware:

    piper_sdk   only firmware BELOW SV1.8-8   (what this project uses today)
    pyAgxArm    all firmware versions         (the successor)

An arm at SV1.8-8 or newer is outside piper_sdk's supported range, so this
prints the version and says which SDK applies. Requires a live CAN bus --
run piper_can_check.py first; a silent bus reports "unknown" here, not a
firmware answer.
"""
import re
import sys
import time
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
import piper_mac_can
piper_mac_can.install()

from piper_sdk import C_PiperInterface_V2


def parse(ver):
    """('S-V1.8-8',) -> (1, 8, 8); None when it does not match the scheme."""
    m = re.search(r"S-V(\d+)\.(\d+)-(\d+)", ver)
    return tuple(int(g) for g in m.groups()) if m else None


def main():
    p = C_PiperInterface_V2("candle", judge_flag=False)
    p.ConnectPort(piper_init=False)      # ConnectPort already fires the query

    ver = None
    deadline = time.time() + 6.0
    while time.time() < deadline:
        ver = p.GetPiperFirmwareVersion()
        if ver and ver != -0x4AF:
            break
        time.sleep(0.2)
        p.SearchPiperFirmwareVersion()

    try:
        p.DisconnectPort()
    except Exception:
        pass

    if not ver or ver == -0x4AF:
        print("firmware: UNKNOWN — the arm did not answer.")
        print("  The bus is silent or the arm is not powered; this tells us")
        print("  nothing about firmware. Fix CAN first (piper_bus_scan.py).")
        return 1

    print(f"firmware: {ver}")
    v = parse(ver)
    if v is None:
        print("  Unrecognized version scheme — compare by hand against SV1.8-8.")
        return 0

    print(f"  parsed  : major={v[0]} minor={v[1]} patch={v[2]}")
    if v >= (1, 8, 8):
        print("\n  >= SV1.8-8  ->  piper_sdk does NOT officially cover this arm.")
        print("  Migrate to pyAgxArm (github.com/agilexrobotics/pyAgxArm).")
        print("  Note: pyAgxArm supports Windows natively via the")
        print("  python-can-agx-cando plugin, which may also retire the")
        print("  hand-written piper_mac_can.py shim.")
    else:
        print("\n  < SV1.8-8  ->  piper_sdk is correct. Keep the current stack.")
    return 0


if __name__ == "__main__":
    sys.exit(main())
