#!/usr/bin/env python3
"""
Fetch per-device AES-128 keys for Go2 robots from your Unitree account.

WHY
---
Go2 firmware >= 1.1.15 (and G1 >= 1.5.1) speaks "data2=3" and encrypts the LAN
WebRTC handshake. Without the robot's own 32-hex key you get:

    AesKeyRequiredError: This robot speaks data2=3 ... the per-device AES-128
    key is required to decrypt the LAN handshake.

Confirmed on 10.72.10.70 (2026-07-25). The older Air (10.72.10.40) does not
need one, which is why only one dog connects out of the box.

The key is PER DEVICE — each robot has its own. They are cached to
~/.cache/go2_fleet/aes_keys.json and go2_fleet.py sets UNITREE_AES_128_KEY
per launched instance.

CREDENTIALS
    Your Unitree password is read with getpass (never echoed, never stored,
    never passed on the command line where it would land in shell history).
    Only the resulting device keys are written to disk, chmod 600.

USAGE
    ./go2_fetch_aes_keys.py                 # prompt, list devices, save keys
    ./go2_fetch_aes_keys.py --show          # show cached keys (masked)
    ./go2_fetch_aes_keys.py --email you@example.com
"""

import argparse
import getpass
import json
import os
import sys
from pathlib import Path

CACHE = Path.home() / ".cache/go2_fleet/aes_keys.json"


def load_cache() -> dict:
    if CACHE.exists():
        try:
            return json.loads(CACHE.read_text())
        except Exception:
            return {}
    return {}


def save_cache(d: dict) -> None:
    CACHE.parent.mkdir(parents=True, exist_ok=True)
    CACHE.write_text(json.dumps(d, indent=2))
    os.chmod(CACHE, 0o600)          # keys are secrets
    print(f"\nsaved -> {CACHE}  (mode 600)")


def mask(k: str) -> str:
    return f"{k[:6]}…{k[-4:]}" if k and len(k) > 12 else "(short)"


def main() -> int:
    ap = argparse.ArgumentParser(description="Fetch Go2 per-device AES-128 keys")
    ap.add_argument("--email", help="Unitree account email (password is prompted)")
    ap.add_argument("--show", action="store_true", help="print cached keys and exit")
    ap.add_argument("--region", choices=("cn", "global"),
                    help="Unitree cloud region. Omit to try cn then global. "
                         "Accounts registered in China live on 'cn'.")
    ap.add_argument("--device-type", default="Go2", choices=("Go2", "G1"),
                    help="app-name header the cloud expects (default Go2)")
    args = ap.parse_args()

    if args.show:
        c = load_cache()
        if not c:
            print("no cached keys yet")
            return 1
        print(f"{'SERIAL':<20} {'KEY':<16} NAME")
        for sn, d in c.items():
            print(f"{sn:<20} {mask(d.get('key','')):<16} {d.get('name','')}")
        return 0

    try:
        from unitree_webrtc_connect.unitree_cloud import UnitreeCloud
    except ImportError:
        sys.exit("unitree_webrtc_connect not importable — run with the dimos venv:\n"
                 "  ~/NervusOS/robots/vendor/dimos/.venv/bin/python "
                 "~/NervusOS/robots/scripts/go2_fetch_aes_keys.py")

    email = args.email or input("Unitree account email: ").strip()
    password = getpass.getpass("Unitree password (not echoed): ")
    if not email or not password:
        sys.exit("email and password are both required")

    # Unitree runs two separate clouds and an account exists in exactly one:
    #   global -> global-robot-api.unitree.com
    #   cn     -> robot-api.unitree.com   (accounts registered in China)
    # Logging into the wrong one returns code=1000 "email not register", which
    # looks like a bad account but is really a wrong-server error.
    regions = [args.region] if args.region else ["cn", "global"]

    cloud = devices = None
    for region in regions:
        print(f"\nlogging in (region={region}) ...")
        try:
            c = UnitreeCloud(region=region, device_type=args.device_type)
            c.login_email(email, password)
        except Exception as e:
            msg = str(e)
            if "not register" in msg.lower():
                print(f"  not on the {region} cloud")
                continue
            print(f"  login failed: {type(e).__name__}: {e}")
            continue
        print(f"  logged in on {region}.")
        try:
            devices = c.list_devices()
            cloud = c
            break
        except Exception as e:
            print(f"  list_devices failed: {type(e).__name__}: {e}")
            continue

    if cloud is None:
        print("\nCould not log in on either cloud.")
        print("  - Confirm the account is the one the robot is bound to in the app.")
        print("  - If you registered with a PHONE NUMBER rather than an email,")
        print("    add an email in the app first: this API only does email login.")
        print("  - Try an explicit region: --region cn   or   --region global")
        return 1

    if not devices:
        print("\nNo devices on this account. The robot must be bound to it in the "
              "Unitree app before its key can be issued.")
        return 1

    cache = load_cache()
    print(f"\n{'SERIAL':<20} {'KEY':<16} NAME")
    n = 0
    for dev in devices:
        # Field names vary by version; probe defensively rather than guess.
        sn = (getattr(dev, "sn", None) or getattr(dev, "serial", None)
              or getattr(dev, "device_id", None) or "?")
        key = (getattr(dev, "aes_128_key", None) or getattr(dev, "key", None)
               or getattr(dev, "secret", None))
        name = getattr(dev, "name", "") or getattr(dev, "nickname", "") or ""
        if not key:
            print(f"{sn:<20} {'(no key field)':<16} {name}")
            print(f"    raw: {vars(dev) if hasattr(dev, '__dict__') else dev}")
            continue
        cache[sn] = {"key": key, "name": name}
        print(f"{sn:<20} {mask(key):<16} {name}")
        n += 1

    if n:
        save_cache(cache)
        print(f"\n{n} key(s) cached. go2_fleet.py will apply them per robot.")
        print("Keys are per-device secrets — do not commit this file.")
    return 0 if n else 1


if __name__ == "__main__":
    sys.exit(main())
