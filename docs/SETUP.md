# Setting up a fresh Ubuntu machine

Tested on Ubuntu 24.04.5 LTS (x86_64), Python 3.12, on a Dell Latitude with an i7-8650U. Everything below runs as
your normal desktop user; only the install step needs `sudo`. All of it works over SSH.

## 1. SSH (optional)

```sh
sudo apt install -y openssh-server
sudo systemctl enable --now ssh
```

If the laptop sleeps while you're connected, turn off automatic suspend on AC power
(Settings → Power, or `gsettings set org.gnome.settings-daemon.plugins.power sleep-inactive-ac-type 'nothing'`).

## 2. Toolchain

The build needs Go 1.26+ and JDK 21. `scripts/build.sh` looks in `~/.local/opt` first, so neither has to be on
your PATH and nothing goes into system directories.

```sh
mkdir -p ~/.local/opt && cd ~/.local/opt

# Go (https://go.dev/dl/)
curl -fsSLO https://go.dev/dl/go1.26.9.linux-amd64.tar.gz
mkdir go-go1.26.9 && tar -xzf go1.26.9.linux-amd64.tar.gz -C go-go1.26.9 --strip-components=1
ln -sfn go-go1.26.9 go
rm go1.26.9.linux-amd64.tar.gz

# JDK 21 (Eclipse Temurin, https://adoptium.net/)
curl -fsSL -o jdk21.tar.gz "https://api.adoptium.net/v3/binary/latest/21/ga/linux/x64/jdk/hotspot/normal/eclipse"
mkdir jdk-21 && tar -xzf jdk21.tar.gz -C jdk-21 --strip-components=1
rm jdk21.tar.gz

~/.local/opt/go/bin/go version
~/.local/opt/jdk-21/bin/java -version
```

Gradle isn't needed: every Gradle project has a wrapper that downloads its own copy on first use. The first build
also downloads a JRE for the release and the Maven dependencies, so it needs internet access.

## 3. Build

```sh
git clone https://github.com/izhangfly/NervusOS-Cambridge.git ~/NervusOS-Cambridge
cd ~/NervusOS-Cambridge
scripts/build.sh            # or: scripts/build.sh release | scripts/build.sh apps
```

Output lands in `out/`: `nervus-os-linux-x86_64.tar.gz` (kernel, system services, system apps, JRE, trust root) and
`out/store/` (the catalog and signed app packages). Missing signing keys are generated into `keys/` on the first run.

## 4. Install

```sh
sudo hosted/install-hosted.sh
```

The installer:

- installs the packages it needs (`xserver-xephyr`, `openbox`, `can-utils` and a few X11 tools);
- runs the upstream NervusOS installer non-interactively, which keeps GDM and `graphical.target` as they are;
- installs nervud as a system service that does **not** start at boot;
- adds `nervus-session`, the hosted power hook, the Openbox layout, a polkit rule that lets your user start and stop
  NervusOS, and the NervusOS / Quit NervusOS launchers with `Ctrl+Alt+N` / `Ctrl+Alt+Q`;
- sets up the PiPER layer: the `can-up` script, `nervus-can.service`, the udev rule, and a Python venv for the driver
  in `/opt/nervus/piper/venv`;
- installs the ACT engine (`nervus-act.service`, run as you, with its own venv in `/opt/nervus/act/venv`: CPU torch and
  LeRobot 0.4.4, a few minutes the first time) and makes sure `~/Shared/NervusACT` exists;
- copies the App Store catalog and packages to `/usr/share/nervus/store/`.

It finishes with a check list. Every line should say `ok`.

## 5. Run

Click **NervusOS** in the dock or app grid. Over SSH, with someone logged in to the desktop:

```sh
nervus-session start          # or: start --window 1600x900
nervus-session status
nervus-session stop
```

The session log is `~/.local/state/nervus/session.log`; the kernel logs to the journal (`journalctl -u nervud`).

## 6. The arm

```sh
ip -details link show can0                  # should be UP, bitrate 1000000
candump can0 | head                         # with the arm powered, hundreds of frames per second
journalctl -u nervus-piper -f               # the driver
curl -s http://127.0.0.1:8770/api/state     # joint angles, gripper, enable state
```

If `can0` is missing, the adapter is unplugged or the arm is off. Plugging it back in brings `can0` up again on its own.

## 7. Apps

Install from the App Store inside NervusOS, or over SSH (the permissions listed are granted on the user's behalf):

```sh
sudo nervusctl install out/store/packages/com.nervus.handteleop-1.0.1.nspkg perm.camera.capture,perm.manipulator.control
sudo nervusctl install out/store/packages/com.nervus.act-1.0.1.nspkg perm.camera.capture,perm.manipulator.control
sudo nervusctl list
```

## 8. ACT: training on a Mac

ACT Studio records on the Dell and trains on a Mac (Apple silicon, 16 GB or more). The two share `~/Shared` through
Syncthing (folder ID `mac-dell-shared`); when they are on different networks it goes through a relay, see `server/`.

On the Dell, the robot cameras are found by their `/dev/v4l/by-id` names: the Orbbec Dabai DC1 on the wrist, the
Logitech C270 for the scene. Plug the C270 into a **USB-A** port (on the USB-C/Thunderbolt side it delivers no frames).

```sh
journalctl -u nervus-act -f                         # the engine
curl -s http://127.0.0.1:8795/api/status | head -c 600
```

On the Mac, from a clone of this repository:

```sh
act/mac/install.sh                                  # launchd agent com.nervus.act-worker
tail -f ~/NervusACT-work/worker.log
```

Everything else (the shared-folder layout, what the worker does, troubleshooting) is in [act/README.md](../act/README.md).

## Updating

Pull or copy the new sources, then run `scripts/build.sh` and `sudo hosted/install-hosted.sh` again, and restart the
session with `nervus-session restart`.

## Tests

```sh
export PATH=$HOME/.local/opt/go/bin:$PATH
for m in nervus-ipc nervud nervus-system-server; do (cd src/$m && go vet ./... && go test ./...); done
```

The Go modules only build on Linux (`GOOS=linux go vet ./...` works for checking on a Mac). GitHub Actions runs the same
suites on every push (`.github/workflows/ci.yml`), plus the ACT unit tests and the secret scan.

The whole ACT pipeline can be tested without the arm, on any machine with LeRobot 0.4.4:

```sh
python act/tests/test_units.py
python act/tests/e2e_sim.py           # record, train, run, correct, post-train, promote on a simulated arm
```
