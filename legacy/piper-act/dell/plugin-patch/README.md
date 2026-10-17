# LeRobot PiPER plugin — local patch

Upstream: [WeGo-Robotics/lerobot_robot_piper](https://github.com/WeGo-Robotics/lerobot_robot_piper) at commit
`ed387ed9502d4fb0451488bfc9c763c8d60a1e07` (2026-09-04, package `lerobot_robot_wego_piper` 0.3.1),
licensed **Apache-2.0**. It's installed editable on the Dell from `D:\Piper-CAN-Teleop\lerobot_robot_piper`.

The local clone was compared file by file with that commit by git blob hash. **Exactly one file differs:**
`lerobot_robot_piper/__init__.py`, the modified copy in this folder. The other 28 files are byte-identical to
upstream, so they aren't copied here.

## What the change does

Upstream's file is only its 2-line header and the four `from .… import` lines. The patch inserts lines 3-25: the
modification notice, `import os, platform, sys`, and the shim block. Windows and macOS have no SocketCAN. Before any `C_PiperInterface_V2` is constructed, the patched `__init__.py`
imports and installs this project's CAN shim (`dell/scripts/piper_mac_can.py`). The shim is found on `sys.path`
or via `PIPER_SHIM_DIR`, which defaults to `D:\Piper-CAN-Teleop\scripts`. It routes the arm's CAN onto the
candleLight USB adapter. On Linux, nothing changes.

## Reapplying

```bat
git clone https://github.com/WeGo-Robotics/lerobot_robot_piper
cd lerobot_robot_piper && git checkout ed387ed9502d4fb0451488bfc9c763c8d60a1e07
copy /Y <this repo>\dell\plugin-patch\lerobot_robot_piper\__init__.py lerobot_robot_piper\__init__.py
pip install -e .
```

## License

The patched file is a modified version of an Apache-2.0 work. It carries a modification notice in its header, as
§4(b) requires, and the license text is in [`LICENSE`](LICENSE), as §4(a) requires.
