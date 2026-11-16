#!/usr/bin/env python3
"""Screen-record every run: GNOME's recorder starts when a run starts and stops a few seconds after it ends
(so the verdict is on screen), then the recording is converted to MP4 next to the others.

    /usr/bin/python3 tools/record_runs.py [~/gptarm-screen/runs] [--until-bench-ends]

Each run gives <out>/<run id>-screen.mp4 (1920x1080, 15 fps, H.264). A run already going when this starts is
recorded from then on (<run id>-screen-partial.mp4). The run's own camera video stays in its run folder
(~/gptarm-runs/<run id>/video.mp4). Needs the system Python (python3-gi, for tools/screencast.py) and
ffmpeg; the portal window should be in front.
"""
import json
import signal
import subprocess
import sys
import time
import urllib.request
from pathlib import Path

API = "http://127.0.0.1:8800"
HERE = Path(__file__).resolve().parent
TAIL_S = 6.0          # keep recording this long after a run ends: the checker's verdict and the summary


def status():
    try:
        return json.load(urllib.request.urlopen(API + "/api/status", timeout=10))
    except (OSError, ValueError):
        return None


def convert(webm, mp4):
    """VP8 -> H.264 at low priority (the arm's driver runs on the same machine), then drop the .webm."""
    cmd = (f"nice -n 19 ffmpeg -v error -y -i '{webm}' -vf fps=15,format=yuv420p -c:v libx264 -preset veryfast "
           f"-crf 23 -movflags +faststart '{mp4}.part.mp4' && mv '{mp4}.part.mp4' '{mp4}' && rm -f '{webm}'")
    return subprocess.Popen(["bash", "-c", cmd])


def main():
    args = [a for a in sys.argv[1:] if not a.startswith("--")]
    until_bench = "--until-bench-ends" in sys.argv
    out = Path(args[0] if args else "~/gptarm-screen/runs").expanduser()
    out.mkdir(parents=True, exist_ok=True)
    rec = None                      # (run id, recorder process, webm path, partial)
    ended_at = None
    jobs = []
    first = True
    stop = {"now": False}
    signal.signal(signal.SIGTERM, lambda *_: stop.update(now=True))
    signal.signal(signal.SIGINT, lambda *_: stop.update(now=True))
    while not stop["now"]:
        s = status()
        run = (s or {}).get("run") or {}
        live = run.get("id") if run.get("state") == "running" else None
        if rec and live != rec[0]:
            ended_at = ended_at or time.time()
            if time.time() - ended_at >= TAIL_S or live:
                rid, p, webm, partial = rec
                p.send_signal(signal.SIGTERM)
                p.wait(timeout=30)
                name = f"{rid}-screen{'-partial' if partial else ''}.mp4"
                jobs.append(convert(webm, out / name))
                print(time.strftime("%H:%M:%S"), "recorded", name, flush=True)
                rec, ended_at = None, None
        if live and rec is None:
            webm = out / f"{live}.webm"
            p = subprocess.Popen(["/usr/bin/python3", str(HERE / "screencast.py"), str(webm), "7200", "15"],
                                 stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
            rec = (live, p, webm, first)
            print(time.strftime("%H:%M:%S"), "recording", live, "(partial)" if first else "", flush=True)
        first = False
        bench = (s or {}).get("bench") or {}
        if until_bench and s is not None and bench.get("state") not in ("running", "paused") and rec is None:
            break
        time.sleep(2.0)
    if rec:
        rec[1].send_signal(signal.SIGTERM)
        rec[1].wait(timeout=30)
        jobs.append(convert(rec[2], out / f"{rec[0]}-screen{'-partial' if rec[3] else ''}.mp4"))
    for j in jobs:
        j.wait()
    print("done", flush=True)


if __name__ == "__main__":
    main()
