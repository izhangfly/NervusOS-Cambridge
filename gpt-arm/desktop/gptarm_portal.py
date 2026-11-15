#!/usr/bin/env python3
"""GPT Arm: the portal (gpt-arm/gptarm/web/) as a desktop app on the robot host.

Opening it starts the service (gptarm.service, a systemd user unit) if it isn't answering, waits until
http://127.0.0.1:8800 answers, then shows the portal in its own window. Closing the window does not stop a run
or a benchmark (the service keeps going); use the portal's Stop or E-stop for that.

Downloads (run videos, recordings) are saved to ~/Downloads under the name the service suggests, with -1, -2
added rather than overwriting a file; a note at the bottom of the window says where each one went.

    gptarm-portal [--url http://127.0.0.1:8800]

Needs python3-gi and gir1.2-webkit2-4.1 (both on Ubuntu 24.04 desktops). Install for the desktop user:

    install -Dm755 gpt-arm/desktop/gptarm_portal.py ~/.local/bin/gptarm-portal
    install -Dm644 gpt-arm/desktop/gptarm.svg ~/.local/share/icons/hicolor/scalable/apps/gptarm.svg
    install -Dm644 gpt-arm/desktop/gptarm.desktop ~/.local/share/applications/gptarm.desktop
"""
import argparse
import os
import subprocess
import threading
import time
import urllib.request

import gi

gi.require_version("Gtk", "3.0")
gi.require_version("Gdk", "3.0")
gi.require_version("WebKit2", "4.1")
from gi.repository import Gdk, GLib, Gtk, WebKit2  # noqa: E402

START_TIMEOUT_S = 30
UNIT = "gptarm.service"
TOAST_S = 8


def downloads_dir() -> str:
    d = GLib.get_user_special_dir(GLib.UserDirectory.DIRECTORY_DOWNLOAD) or os.path.expanduser("~/Downloads")
    os.makedirs(d, exist_ok=True)
    return d


def free_path(folder: str, name: str) -> str:
    """folder/name, or folder/name-1.ext, name-2.ext, ... if that exists."""
    name = os.path.basename(name or "").strip().lstrip(".") or "download"
    stem, ext = os.path.splitext(name)
    path, n = os.path.join(folder, name), 1
    while os.path.exists(path):
        path = os.path.join(folder, f"{stem}-{n}{ext}")
        n += 1
    return path


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

        # Downloads: a note at the bottom of the window says where each file went.
        self.toast = Gtk.Revealer(halign=Gtk.Align.CENTER, valign=Gtk.Align.END, margin_bottom=22)
        bar = Gtk.Box(orientation=Gtk.Orientation.HORIZONTAL, spacing=12)
        bar.get_style_context().add_class("app-notification")
        self.toast_label = Gtk.Label(xalign=0)
        self.toast_label.set_line_wrap(True)
        self.toast_open = Gtk.Button(label="Show in folder")
        self.toast_open.connect("clicked", self.on_show_folder)
        close = Gtk.Button.new_from_icon_name("window-close-symbolic", Gtk.IconSize.BUTTON)
        close.set_relief(Gtk.ReliefStyle.NONE)
        close.connect("clicked", lambda *_: self.toast.set_reveal_child(False))
        for w in (self.toast_label, self.toast_open, close):
            bar.pack_start(w, False, False, 0)
        self.toast.add(bar)
        self.toast_timer = 0
        self.toast_folder = None
        overlay = Gtk.Overlay()
        overlay.add(self.stack)
        overlay.add_overlay(self.toast)
        self.add(overlay)
        self.watch_downloads()
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
        # A video or anything the view can't show is downloaded (WebKit does that for attachments by itself;
        # this also covers a service that forgets Content-Disposition).
        if kind == WebKit2.PolicyDecisionType.RESPONSE:
            mime = decision.get_response().get_mime_type() or ""
            if mime.startswith("video/") or not decision.is_mime_type_supported():
                decision.download()
                return True
            return False
        # Stay on the service: anything else (there should be nothing) opens in the default browser.
        if kind == WebKit2.PolicyDecisionType.NAVIGATION_ACTION:
            uri = decision.get_navigation_action().get_request().get_uri()
            if not uri.startswith(self.url) and not uri.startswith(("blob:", "about:", "data:")):
                decision.ignore()
                Gtk.show_uri_on_window(self, uri, Gdk.CURRENT_TIME)
                return True
        return False

    # ---- downloads (WebKit2GTK 4.1: WebContext::download-started, Download::decide-destination) -------------
    def watch_downloads(self):
        ctx = self.web.get_context()
        try:
            ctx.connect("download-started", self.on_download)
        except TypeError:
            # A WebKitGTK without the WebContext signal: the network session has it instead.
            self.web.get_network_session().connect("download-started", self.on_download)

    def on_download(self, _source, download):
        state = {"path": None, "failed": False}       # per download, shared by its three handlers
        download.connect("decide-destination", self.on_decide_destination, state)
        download.connect("failed", self.on_download_failed, state)
        download.connect("finished", self.on_download_finished, state)

    def on_decide_destination(self, download, suggested, state):
        if not suggested:
            uri = download.get_request().get_uri()
            suggested = GLib.uri_unescape_string(uri.split("?")[0].rstrip("/").rsplit("/", 1)[-1], None) or "download"
        path = free_path(downloads_dir(), suggested)
        download.set_allow_overwrite(False)
        download.set_destination(GLib.filename_to_uri(path, None))  # 4.1 takes a URI (6.0 takes a path)
        state["path"] = path
        self.show_toast(f"Downloading {os.path.basename(path)} to {self.short(os.path.dirname(path))}…", None)
        return True

    def on_download_failed(self, _download, error, state):
        state["failed"] = True
        try:
            cancelled = error.matches(WebKit2.download_error_quark(), WebKit2.DownloadError.CANCELLED_BY_USER)
        except (AttributeError, TypeError):
            cancelled = False
        if not cancelled:
            self.show_toast(f"Download failed: {error.message}", None)

    def on_download_finished(self, download, state):
        # "finished" also follows "failed"; only a clean finish is reported as saved.
        if state["failed"]:
            return
        dest = download.get_destination()
        path = state["path"] or (GLib.filename_from_uri(dest)[0] if dest else None)
        if path:
            self.show_toast(f"Saved {os.path.basename(path)} to {self.short(os.path.dirname(path))}", os.path.dirname(path))

    @staticmethod
    def short(folder):
        home = os.path.expanduser("~")
        return "~" + folder[len(home):] if folder.startswith(home) else folder

    def show_toast(self, text, folder):
        self.toast_label.set_text(text)
        self.toast_folder = folder
        self.toast_open.set_visible(bool(folder))
        self.toast.show_all()
        self.toast_open.set_visible(bool(folder))
        self.toast.set_reveal_child(True)
        if self.toast_timer:
            GLib.source_remove(self.toast_timer)
        self.toast_timer = GLib.timeout_add_seconds(TOAST_S, self.hide_toast)

    def hide_toast(self):
        self.toast.set_reveal_child(False)
        self.toast_timer = 0
        return False

    def on_show_folder(self, *_):
        if self.toast_folder:
            Gtk.show_uri_on_window(self, GLib.filename_to_uri(self.toast_folder, None), Gdk.CURRENT_TIME)
        self.hide_toast()

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
