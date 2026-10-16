# PiPER arm driver (hardware layer)

The AgileX PiPER (6-DOF arm plus parallel gripper) talks CAN 2.0B at 1 Mbit/s.
On this Ubuntu host it is plugged in through the candleLight USB-CAN adapter that
ships with the arm (USB `1d50:606f`, kernel driver `gs_usb`), which shows up as
`can0`.

NervusOS sits above the hardware, so the arm is driven by a small driver at the
bottom of the stack. Apps never touch CAN themselves:

```
Robot Arm app (NervusOS, sandboxed)
        │  HTTP on 127.0.0.1:8770
        ▼
piper_webctl.py   (nervus-piper.service, user nervus-piper)
        │  piper_sdk / SocketCAN
        ▼
can0 → PiPER
```

## Pieces

| File | Installed as | Job |
|---|---|---|
| `piper_webctl.py` | `/opt/nervus/piper/piper_webctl.py` | Driver and HTTP API; also serves a browser control panel |
| `requirements.txt` | `/opt/nervus/piper/venv` | `piper_sdk` and `python-can` |
| `../hosted/libexec/can-up` | `/usr/libexec/nervus/can-up` | Sets `can0` to 1 Mbit/s and brings it up |
| `../hosted/systemd/nervus-can.service` | enabled at boot | Runs `can-up can0` |
| `../hosted/udev/90-nervus-can.rules` | `/etc/udev/rules.d/` | Runs `can-up` again when the adapter is replugged |
| `../hosted/systemd/nervus-piper.service` | started with NervusOS | Runs the driver; `PartOf=nervud.service`, so quitting NervusOS stops it |

Only one program can own `can0` at a time. That's the reason the driver belongs
to the platform rather than to an app: install or remove apps freely and the bus
still has a single owner.

## Safety

- The motors start **disabled**. Nothing moves until Enable is pressed.
- The client sends a heartbeat. If it stops for more than 1.5 s, the driver stops
  streaming commands and the arm halts.
- E-Stop sends `EmergencyStop` (CAN 0x150). Recover sends `ResetPiper`, which
  clears a latched collision fault without a power cycle (the arm goes limp for a
  moment, so support it).
- Joint targets are clamped to the limits in the AgileX manual, on the server.
- The API has no authentication, so it listens on loopback only. Don't change
  `--bind` to `0.0.0.0`.

## API

`GET /api/state`, and `POST` to `/api/heartbeat`, `/api/enable {on}`,
`/api/joint {joint, deg}`, `/api/gripper {mm}`, `/api/speed {speed}`,
`/api/mode {mode: joint|cartesian}`, `/api/jog {axis, dir, on}` (leashed Cartesian
jog), `/api/jointjog {joint, dir, on}`, `/api/jogstop`, `/api/home`,
`/api/recover`, `/api/estop`.

## Useful commands

```sh
ip -details link show can0          # state, bitrate
candump can0                        # raw traffic (arm powered: ~hundreds of frames/s)
journalctl -u nervus-piper -f       # driver log
curl -s localhost:8770/api/state    # what the driver sees
ssh -L 8770:localhost:8770 <host>   # browser panel from another machine
```

## Troubleshooting

| Symptom | Cause and fix |
|---|---|
| `can0` missing | Adapter unplugged, or `gs_usb` not loaded (`sudo modprobe gs_usb`) |
| `can0` is `DOWN` after replugging | The udev rule didn't run: `sudo /usr/libexec/nervus/can-up can0` |
| Driver log: `CAN thread unhealthy on can0` | Arm not powered, or wrong bitrate. `candump can0` should show frames |
| State reads but nothing moves | Press Enable first; check E-Stop isn't latched; Recover clears a collision fault |
| `TARGET_POS_EXCEEDS_LIMIT` | Pose limit. The Cartesian jog is leashed to the actual pose, so this means a real workspace edge |
