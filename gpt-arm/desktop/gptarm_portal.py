#!/usr/bin/env python3
"""GPT Arm: the portal (gpt-arm/gptarm/web/) as a desktop app on the robot host.

Opening it starts the service (gptarm.service, a systemd user unit) if it isn't answering, waits until
http://127.0.0.1:8800 answers, then shows the portal in its own window. Closing the window does not stop a run
or a benchmark (the service keeps going); use the portal's Stop or E-stop for that.

    gptarm-portal [--url http://127.0.0.1:8800]

Needs python3-gi and gir1.2-webkit2-4.1 (both on Ubuntu 24.04 desktops). Install for the desktop user:

    install -Dm755 gpt-arm/desktop/gptarm_portal.py ~/.local/bin/gptarm-portal
    install -Dm644 gpt-arm/desktop/gptarm.svg ~/.local/share/icons/hicolor/scalable/apps/gptarm.svg
    install -Dm644 gpt-arm/desktop/gptarm.desktop ~/.local/share/applications/gptarm.desktop
"""
import argparse
import subprocess
import threading
import time
import urllib.request

import gi

gi.require_version("Gtk", "3.0")
gi.require_version("WebKit2", "4.1")
from gi.repository import Gdk, GLib, Gtk, WebKit2  # noqa: E402

START_TIMEOUT_S = 30
UNIT = "gptarm.service"


def answering(url: str) -> bool:
    try:
        with urllib.request.urlopen(url + "/api/status", timeout=1.5) as r:
            return r.status == 200
    except OSError:
        return False


class Window(Gtk.Window):
    def __init__(self, url: str):
        super().__init__(title="GPT Arm")
        self.url = url
        self.set_default_size(1600, 960)
        self.set_icon_name("gptarm")
        self.connect("destroy", Gtk.main_quit)
        self.connect("key-press-event", self.on_key)

        self.stack = Gtk.Stack()
        box = Gtk.Box(orientation=Gtk.Orientation.VERTICAL, spacing=14, halign=Gtk.Align.CENTER,
                      valign=Gtk.Align.CENTER)
        self.message = Gtk.Label(label="Starting the GPT Arm service…")
        self.message.set_line_wrap(True)
        self.message.set_justify(Gtk.Justification.CENTER)
        self.retry = Gtk.Button(label="Try again")
        self.retry.connect("clicked", lambda *_: self.start())
        self.retry.set_no_show_all(True)
        box.pack_start(self.message, False, False, 0)
        box.pack_start(self.retry, False, False, 0)
        self.stack.add_named(box, "message")

        self.web = WebKit2.WebView()
        s = self.web.get_settings()
        try:
            # The Dell renders in software: no GPU compositing, no smooth-scroll animation.
            s.set_hardware_acceleration_policy(WebKit2.HardwareAccelerationPolicy.NEVER)
        except AttributeError:
            pass
        s.set_enable_smooth_scrolling(False)
        s.set_enable_developer_extras(False)
        self.web.connect("load-failed", self.on_load_failed)
        self.web.connect("decide-policy", self.on_policy)
        self.stack.add_named(self.web, "portal")
        self.add(self.stack)
        self.start()

    def start(self):
        self.retry.hide()
        self.message.set_text("Starting the GPT Arm service…")
        self.stack.set_visible_child_name("message")
        threading.Thread(target=self.bring_up, daemon=True).start()

    def bring_up(self):
        if not answering(self.url):
            r = subprocess.run(["systemctl", "--user", "start", UNIT], capture_output=True, text=True)
            if r.returncode != 0:
                GLib.idle_add(self.fail, "Could not start the GPT Arm service:\n" + (r.stderr.strip() or "unknown error")
                              + f"\n\nCheck: systemctl --user status {UNIT}")
                return
        end = time.time() + START_TIMEOUT_S
        while time.time() < end:
            if answering(self.url):
                GLib.idle_add(self.show_portal)
                return
            time.sleep(0.5)
        GLib.idle_add(self.fail, f"The GPT Arm service started but isn't answering on {self.url}.\n\n"
                                 f"Check: journalctl --user -u {UNIT} -f")

    def show_portal(self):
        self.web.load_uri(self.url + "/")
        self.stack.set_visible_child_name("portal")
        return False

    def fail(self, text: str):
        self.message.set_text(text)
        self.retry.show()
        self.stack.set_visible_child_name("message")
        return False

    def on_load_failed(self, _view, _event, uri, error):
        # The page shows its own offline state once loaded; this only covers the first load.
        if uri.startswith(self.url):
            self.fail(f"The portal didn't load: {error.message}")
        return True

    def on_policy(self, _view, decision, kind):
        # Stay on the service: anything else (there should be nothing) opens in the default browser.
        if kind == WebKit2.PolicyDecisionType.NAVIGATION_ACTION:
            uri = decision.get_navigation_action().get_request().get_uri()
            if not uri.startswith(self.url) and not uri.startswith(("blob:", "about:", "data:")):
                decision.ignore()
                Gtk.show_uri_on_window(self, uri, Gdk.CURRENT_TIME)
                return True
        return False

    def on_key(self, _w, ev):
        ctrl = ev.state & Gdk.ModifierType.CONTROL_MASK
        if ev.keyval == Gdk.KEY_F5 or (ctrl and ev.keyval in (Gdk.KEY_r, Gdk.KEY_R)):
            self.web.reload()
            return True
        if ctrl and ev.keyval in (Gdk.KEY_q, Gdk.KEY_Q):
            self.destroy()
            return True
        return False


def main():
    ap = argparse.ArgumentParser(description="GPT Arm: the portal as a desktop app")
    ap.add_argument("--url", default="http://127.0.0.1:8800")
    args = ap.parse_args()
    url = args.url.rstrip("/")
    if not url.startswith(("http://127.0.0.1", "http://localhost")):
        raise SystemExit("the GPT Arm service listens on loopback only")
    GLib.set_prgname("gptarm-portal")  # the window's WM class, matched by StartupWMClass in gptarm.desktop
    w = Window(url)
    w.show_all()
    w.maximize()
    Gtk.main()


if __name__ == "__main__":
    main()
