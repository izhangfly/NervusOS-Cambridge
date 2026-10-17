#!/usr/bin/env python3
"""bus + a daemon thread looping recv() — mimics the SDK's ReadCan thread."""
import sys
import threading
import time

sys.path.insert(0, str(__import__("pathlib").Path(__file__).resolve().parent))
import piper_mac_can
piper_mac_can.install()

import can

bus = can.Bus(channel="candle")

def read_loop():
    while True:
        try:
            bus.recv(timeout=0.1)
        except Exception:
            break

t = threading.Thread(target=read_loop, daemon=True)
t.start()

time.sleep(2.0)
print("main thread done sleeping, exiting (recv thread still running)")
# no shutdown; atexit handles it, recv thread is daemon and killed on exit
