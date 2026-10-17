#!/usr/bin/env python3
"""Secret scan for this repository, which is public. Exit status 1 on any finding.

    scripts/secret-scan.py          the staged changes (what the next commit would add)
    scripts/secret-scan.py --all    every tracked file

It blocks private keys, API tokens, Syncthing API keys, hard-coded passwords, any Chinese text (the repo is
English only), files over 50 MB, and the address of the server that relays the Mac-Dell link. That address
is never written into a file, this one included: it is read at run time from `ssh -G hetzner`, from the
relay URLs in the local Syncthing config, and from $NERVUS_SCAN_HOSTS. Extra local-only patterns (one
regular expression per line) can go in ~/.config/nervus-scan/extra.txt. Matches are printed masked.
"""
import os
import re
import subprocess
import sys
from pathlib import Path

REPO = Path(__file__).resolve().parent.parent
MAX_BYTES = 50 * 1024 * 1024
SYNCTHING_CONFIGS = ["~/Library/Application Support/Syncthing/config.xml", "~/.local/state/syncthing/config.xml",
                     "~/.config/syncthing/config.xml"]


def git(*a) -> bytes:
    return subprocess.run(["git", "-C", str(REPO), *a], capture_output=True, check=True).stdout


def server_hosts() -> set[str]:
    hosts = set(filter(None, os.environ.get("NERVUS_SCAN_HOSTS", "").split(",")))
    try:
        cfg = subprocess.run(["ssh", "-G", "hetzner"], capture_output=True, text=True, timeout=5).stdout
        for line in cfg.splitlines():
            if line.startswith("hostname ") and line.split()[1] != "hetzner":
                hosts.add(line.split()[1])
    except Exception:
        pass
    for p in SYNCTHING_CONFIGS:
        try:
            xml = Path(p).expanduser().read_text()
        except OSError:
            continue
        for m in re.finditer(r"relay://\[?([0-9A-Za-z.:-]+?)\]?:\d+", xml):
            hosts.add(m.group(1))
        for m in re.finditer(r"(?:tcp|quic)://\[?([0-9A-Za-z.:-]+?)\]?:\d+", xml):
            if not re.match(r"^(0\.0\.0\.0|127\.|10\.|192\.168\.|172\.(1[6-9]|2\d|3[01])\.|::|dynamic|default)", m.group(1)):
                hosts.add(m.group(1))
    return {h for h in hosts if len(h) >= 7}


PATTERNS = [
    # A header followed by key material: code that parses PEM mentions the header with no key after it.
    ("private key", re.compile(rb"-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----\s*[\r\n]+[A-Za-z0-9+/=]{40,}|PuTTY-User-Key-File-[0-9]+: ")),
    ("GitHub token", re.compile(rb"\b(gh[pousr]_[A-Za-z0-9]{30,}|github_pat_[A-Za-z0-9_]{40,})")),
    ("API key", re.compile(rb"\b(sk-(ant-)?[A-Za-z0-9_\-]{20,}|AKIA[0-9A-Z]{16}|AIza[0-9A-Za-z_\-]{35}"
                           rb"|xox[abprs]-[A-Za-z0-9\-]{10,}|re_[A-Za-z0-9]{24,})")),
    ("Syncthing API key", re.compile(rb"<apikey>[^<]{8,}</apikey>|X-API-Key:\s*[A-Za-z0-9]{20,}")),
    ("LinkedIn cookie", re.compile(rb"li_at=[A-Za-z0-9_\-]{20,}")),
    ("hard-coded password", re.compile(
        rb"(?i)\b(password|passwd|passcode|secret|api_?key|auth_?token)\b[\"']?\s*[:=]\s*[\"'][^\"'\s$<{]{6,}[\"']")),
]
# CJK punctuation, ideographs and full-width forms, built from code points so this file stays ASCII.
CJK = re.compile("[%s]" % "".join(f"{chr(a)}-{chr(b)}" for a, b in (
    (0x3000, 0x303F), (0x3400, 0x4DBF), (0x4E00, 0x9FFF), (0xF900, 0xFAFF), (0xFF01, 0xFF5E))))


def local_patterns() -> list:
    p = Path("~/.config/nervus-scan/extra.txt").expanduser()
    try:
        lines = p.read_text().splitlines()
    except OSError:
        return []
    return [("local pattern", re.compile(l.strip().encode(), re.I)) for l in lines if l.strip() and not l.startswith("#")]


def mask(s: str) -> str:
    s = s.strip()
    return s[:3] + "…" + s[-2:] if len(s) > 6 else "…"


def main() -> int:
    every = "--all" in sys.argv
    names = git("ls-files", "-z") if every else git("diff", "--cached", "--name-only", "--diff-filter=ACMR", "-z")
    hosts = [h.encode() for h in server_hosts()]
    if not hosts:
        print("secret-scan: found no server address to check for (ssh -G hetzner, Syncthing relays, "
              "$NERVUS_SCAN_HOSTS); refusing to vouch for the commit", file=sys.stderr)
        return 1
    patterns = PATTERNS + local_patterns()
    bad = []
    for raw in filter(None, names.split(b"\0")):
        path = raw.decode()
        try:
            blob = git("show", f":{path}")
        except subprocess.CalledProcessError:
            continue
        if len(blob) > MAX_BYTES:
            bad.append((path, 0, "file over 50 MB", ""))
            continue
        for h in hosts:
            if h in blob:
                bad.append((path, blob[:blob.index(h)].count(b"\n") + 1, "the server's address", "…"))
        if b"\0" in blob[:8192]:
            continue  # binary: only the address check applies
        for label, rx in patterns:
            for m in rx.finditer(blob):
                bad.append((path, blob[:m.start()].count(b"\n") + 1, label, mask(m.group(0).decode("utf-8", "replace"))))
        for n, line in enumerate(blob.decode("utf-8", "replace").splitlines(), 1):
            m = CJK.search(line)
            if m:
                bad.append((path, n, "Chinese text", m.group(0)))
                break
    for path, line, label, what in bad:
        print(f"secret-scan: {path}:{line}: {label} {what}", file=sys.stderr)
    return 1 if bad else 0


if __name__ == "__main__":
    sys.exit(main())
