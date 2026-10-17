#!/usr/bin/env python3
"""
Dump a GO2's BLE GATT table — services, characteristics, handles, properties.

Why: dimos' go2tool provisioning writes to WRITE_CHAR_UUID (0000ffe2-…) and
subscribes to NOTIFY_CHAR_UUID (0000ffe1-…), falling back to raw handle 13.
If a unit's firmware exposes different characteristics/handles, the handshake
write goes nowhere and the notify never arrives — which surfaces only as a
bare `TimeoutError` waiting for the response. This tells you which it is.

Run with the dimos venv (it has bleak):
    ~/dimos/.venv/bin/python scripts/go2_ble_probe.py                 # scan, list GO2s
    ~/dimos/.venv/bin/python scripts/go2_ble_probe.py <BLE-ADDRESS>   # dump that one

On macOS the "address" is a CoreBluetooth UUID, not a MAC — take it from the
SERIAL/MAC column of `dimos go2tool discover --ble`.
"""

from __future__ import annotations

import asyncio
import sys

from bleak import BleakClient, BleakScanner

# What dimos' provisioning code expects to find.
EXPECTED = {
    "0000ffe0-0000-1000-8000-00805f9b34fb": "UNITREE_SERVICE",
    "0000ffe1-0000-1000-8000-00805f9b34fb": "NOTIFY_CHAR (subscribed)",
    "0000ffe2-0000-1000-8000-00805f9b34fb": "WRITE_CHAR  (handshake written here)",
}


async def scan() -> None:
    print("scanning 10s for BLE devices ...")
    devices = await BleakScanner.discover(timeout=10.0)
    hits = [d for d in devices if (d.name or "").startswith("Go2")]
    if not hits:
        print(f"no Go2_* found ({len(devices)} BLE devices seen total)")
        for d in devices[:10]:
            print(f"  {d.address}  {d.name!r}")
        return
    for d in hits:
        print(f"  {d.name}  ->  {d.address}")
    print("\nre-run with an address to dump its GATT table.")


async def dump(address: str) -> None:
    print(f"connecting to {address} ...")
    async with BleakClient(address, timeout=25.0) as client:
        print(f"connected: {client.is_connected}\n")
        found: set[str] = set()
        for service in client.services:
            found.add(service.uuid.lower())  # services count too, not just chars
            print(f"[service] {service.uuid}")
            for char in service.characteristics:
                found.add(char.uuid.lower())
                props = ",".join(char.properties)
                print(f"    char {char.uuid}  handle={char.handle}  props=[{props}]")
                for desc in char.descriptors:
                    print(f"        descriptor {desc.uuid}  handle={desc.handle}")

        print("\n--- what dimos provisioning needs ---")
        for uuid, role in EXPECTED.items():
            mark = "OK  " if uuid in found else "MISS"
            print(f"  [{mark}] {uuid}  {role}")
        if not all(u in found for u in EXPECTED):
            print("\n=> Missing characteristics: this firmware does NOT speak the")
            print("   BLE provisioning protocol go2tool implements. Provision via")
            print("   the Unitree app (or the dog's own AP) instead.")
        else:
            print("\n=> All expected characteristics present, so the GATT layer is")
            print("   fine — the dog is refusing/ignoring the handshake payload,")
            print("   which points at a firmware protocol change (different key).")


async def main() -> None:
    if len(sys.argv) > 1:
        await dump(sys.argv[1])
    else:
        await scan()


if __name__ == "__main__":
    try:
        asyncio.run(main())
    except KeyboardInterrupt:
        sys.exit("\ninterrupted.")
