#!/usr/bin/env python3
"""Record the robot host's whole screen (GNOME on Wayland) to a .webm, e.g. the portal during a run.

Uses GNOME Shell's own recorder (org.gnome.Shell.Screencast on the session bus), so it works over SSH
with no extra packages and no permission dialog. GNOME stops a recording when the process that started it
goes away, so this stays running until SIGTERM/SIGINT or the time limit.

    /usr/bin/python3 tools/screencast.py ~/portal.webm [limit_s=3600] [fps=15] &     # then: kill -TERM <pid>
    ffmpeg -i ~/portal.webm -vf fps=15,format=yuv420p -c:v libx264 -preset veryfast -crf 23 \
           -movflags +faststart ~/portal.mp4                                        # VP8 -> H.264 for a Mac

Run it with the system Python (it needs python3-gi). The recording shows whatever is on screen: put the
portal window in front first.
"""
import signal
import sys

from gi.repository import Gio, GLib


def main():
    out = sys.argv[1]
    limit = float(sys.argv[2]) if len(sys.argv) > 2 else 3600.0
    fps = int(sys.argv[3]) if len(sys.argv) > 3 else 15
    bus = Gio.bus_get_sync(Gio.BusType.SESSION, None)
    proxy = Gio.DBusProxy.new_sync(bus, 0, None, "org.gnome.Shell.Screencast", "/org/gnome/Shell/Screencast",
                                   "org.gnome.Shell.Screencast", None)
    opts = {"framerate": GLib.Variant("i", fps), "draw-cursor": GLib.Variant("b", False)}
    ok, used = proxy.call_sync("Screencast", GLib.Variant("(sa{sv})", (out, opts)), 0, -1, None).unpack()
    print("started", ok, used, flush=True)
    if not ok:
        sys.exit(1)
    loop = GLib.MainLoop()

    def stop(*_):
        try:
            proxy.call_sync("StopScreencast", None, 0, 20000, None)
        except GLib.Error as e:
            print("stop error", e, flush=True)
        loop.quit()
        return False

    GLib.unix_signal_add(GLib.PRIORITY_DEFAULT, signal.SIGTERM, stop)
    GLib.unix_signal_add(GLib.PRIORITY_DEFAULT, signal.SIGINT, stop)
    GLib.timeout_add(int(limit * 1000), stop)
    loop.run()
    print("stopped", used, flush=True)


if __name__ == "__main__":
    main()
