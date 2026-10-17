#!/usr/bin/env python3
"""Find a Unitree GO2 on the LAN by serial number and print its IP.

Usage:
    ./.venv/bin/python find_go2.py                 # look for GO2_SN below
    ./.venv/bin/python find_go2.py B42D2000XXXXXX  # look for a specific SN
    ./.venv/bin/python find_go2.py --any           # list every Unitree device found

How it works: sends a UDP multicast query ({"name": "unitree_dapengche"}) to
231.1.1.1:10131 and listens on :10134 for robots announcing their sn + ip.
This is the same discovery the WebRTC LocalSTA path uses internally.

Requires: same L2 network as the robot (STA mode), and NO tun/VPN owning the
default route -- multicast does not traverse V2Box's PacketTunnel.
"""

import subprocess
import sys

from unitree_webrtc_connect.multicast_scanner import discover_ip_sn

GO2_SN = "B42D2000NEWXXXXX"


def default_route_iface() -> str:
    try:
        out = subprocess.run(
            ["route", "-n", "get", "default"], capture_output=True, text=True, timeout=5
        ).stdout
        for line in out.splitlines():
            if "interface:" in line:
                return line.split(":", 1)[1].strip()
    except Exception:
        pass
    return "?"


def main() -> int:
    args = [a for a in sys.argv[1:]]
    show_any = "--any" in args
    wanted = next((a for a in args if not a.startswith("-")), GO2_SN)

    iface = default_route_iface()
    if iface.startswith("utun"):
        print(f"!! default route is on {iface} (a VPN/proxy tunnel).")
        print("!! Multicast discovery will almost certainly fail.")
        print("!! Disconnect V2Box, then re-run. Continuing anyway...\n")

    found = discover_ip_sn(timeout=3)

    if not found:
        print("\nNo devices answered.")
        print("Checklist:")
        print("  1. V2Box / any VPN disconnected?   route -n get default | grep interface")
        print("  2. Robot powered on and booted (wait ~45s after power-up)?")
        print("  3. Mac and robot on the SAME wifi/LAN (STA mode)?")
        print("  4. In AP mode there is no discovery needed -- the robot is 192.168.12.1")
        return 1

    print(f"\nFound {len(found)} device(s):")
    for sn, ip in found.items():
        mark = "  <-- yours" if sn == wanted else ""
        print(f"  {sn}  ->  {ip}{mark}")

    if wanted in found:
        ip = found[wanted]
        print("\nReady to paste:\n")
        print(f"  export ROBOT_IP={ip}")
        print("  cd /Users/ianzhang/NervusOS/robots/dimos")
        print("  source .venv/bin/activate")
        print("  dimos run unitree-go2\n")
        return 0

    if not show_any:
        print(f"\nSN {wanted} not among the responders (listed above).")
        print("Re-run with --any if you want to use whatever did answer.")
    return 1


if __name__ == "__main__":
    sys.exit(main())
