#!/usr/bin/env python3
"""Build the report page's video clips, posters and full-length videos from the run recordings.

    python site/build_media.py --runs ~/gptarm-runs --screens ~/gptarm-screen/runs \
        --edit owner-edit.mov --out site/media [--only clip-id ...]

Inputs: each run's camera video (<runs>/<id>/video.mp4: overhead left, wrist right, 1920x720 at 8 fps,
starting with the run), the portal screen recordings (<screens>/<id>-screen.mp4, 1920x1080 at 15 fps) and the
owner's edit. Clips are short, silent, looping H.264 MP4s (smaller than GIFs and they play everywhere).
"Moving parts only" time-lapses keep the seconds the arm was moving, from each decision's motion window
(a step is logged when its motion ends: the motion ran over [t - motion_s, t]), and drop the time the model
spent thinking. Writes <out>/manifest.json describing every file.
"""
import argparse
import json
import subprocess
from pathlib import Path

ENC = ["-c:v", "libx264", "-preset", "slow", "-pix_fmt", "yuv420p", "-movflags", "+faststart", "-an"]
# When the portal screen recording started, relative to the run's start (s), from record_runs.log / the
# screencast log. Positive: the recording began after the run did.
SCREEN_OFFSET = {"20261116-130056-gpt-5.6-sol": -4.0, "20261116-134633-gpt-5.6-sol": 1.0,
                 "20261116-141102-gpt-6.1-sol": 1.0, "20261116-142403-gpt-5.6-sol": 0.0,
                 "20261116-150826-gpt-6.1-sol": 0.0, "20261116-154924-gpt-5.6-sol": 2.0}


def ff(*args):
    subprocess.run(["ffmpeg", "-v", "error", "-y", *args], check=True)


def probe(path):
    out = subprocess.run(["ffprobe", "-v", "error", "-show_entries", "format=duration:stream=width,height",
                          "-of", "json", str(path)], capture_output=True, text=True, check=True).stdout
    d = json.loads(out)
    s = next(x for x in d["streams"] if "width" in x)
    return {"w": s["width"], "h": s["height"], "duration": round(float(d["format"]["duration"]), 2),
            "bytes": Path(path).stat().st_size}


def motion_windows(run_dir, pad_before=0.6, pad_after=0.4, gripper_s=1.6):
    """[(a, b)] seconds from the run's start when the arm moved (moves and gripper actions)."""
    d = json.loads((Path(run_dir) / "run.json").read_text())
    out = []
    for s in d["steps"]:
        if s.get("kind") != "decision" or not s.get("ok"):
            continue
        end = s["t"] - d["started"]
        dur = float(s.get("motion_s") or 0.0)
        if s.get("skill") == "gripper":
            dur = max(dur, gripper_s)
        if dur < 0.3:
            continue
        out.append((max(0.0, end - dur - pad_before), end + pad_after))
    merged = []
    for a, b in sorted(out):
        if merged and a <= merged[-1][1] + 0.3:
            merged[-1] = (merged[-1][0], max(b, merged[-1][1]))
        else:
            merged.append((a, b))
    return merged


def select_expr(windows):
    return "+".join(f"between(t,{a:.2f},{b:.2f})" for a, b in windows)


def poster(src, at, dst, width):
    ff("-ss", f"{at:.2f}", "-i", str(src), "-frames:v", "1", "-vf", f"scale={width}:-2", "-q:v", "4", str(dst))


def clips(a):
    R, S, O = Path(a.runs).expanduser(), Path(a.screens).expanduser(), Path(a.out)
    cam = lambda rid: R / rid / "video.mp4"            # noqa: E731
    scr = lambda rid: S / f"{rid}-screen.mp4"          # noqa: E731
    jobs = {}

    def job(fn):
        jobs[fn.__name__.replace("_", "-")] = fn
        return fn

    @job
    def hero_edit(out):
        """The owner's time-aligned edit: phone footage, portal, camera video (already SDR-converted)."""
        ff("-i", a.edit, "-vf", "scale=720:-2,fps=30,format=yuv420p", "-c:v", "libx264", "-preset", "slow",
           "-crf", "24", "-c:a", "aac", "-b:a", "128k", "-movflags", "+faststart", str(out))
        return {"caption": "The full cycle of 16 November, 13:00: the arm, the portal and the run's own camera video, "
                           "time-aligned (edit by the author).", "poster_at": 5.5, "audio": True}

    def timelapse(rid, out, speed, crop=None, width=1280):
        w = motion_windows(R / rid)
        vf = f"select='{select_expr(w)}',setpts=N/(8*{speed})/TB"
        if crop:
            vf = f"crop={crop}," + vf
        ff("-i", str(cam(rid)), "-vf", vf + f",scale={width}:-2,fps=24", *ENC, "-crf", "27", str(out))
        return sum(b - a for a, b in w)

    @job
    def cycle_6_1(out):
        moving = timelapse("20261116-141102-gpt-6.1-sol", out, 3)
        return {"caption": f"GPT-6.1 Sol, full cycle in 13 minutes (run 141102). Only the {moving:.0f} s the arm "
                           "was moving, at 3x; the time the model spent thinking is cut out.", "poster_at": 6}

    @job
    def cycle_5_6(out):
        moving = timelapse("20261116-130056-gpt-5.6-sol", out, 3)
        return {"caption": f"GPT-5.6 Sol's full cycle from the development runs (run 130056), moving parts only at 3x "
                           f"({moving:.0f} s of motion in 12.9 minutes).", "poster_at": 6}

    @job
    def first_success(out):
        timelapse("20261116-122955-gpt-5.6-sol", out, 2)
        return {"caption": "The first time it worked: out of the basket in 11 decisions (run 122955), moving parts "
                           "only at 2x.", "poster_at": 4}

    @job
    def race(out):
        """Same wall-clock speed, overhead views stacked: GPT-6.1 Sol 141102 against GPT-5.6 Sol 134633."""
        speed, length = 24.0, 1453.0 / 24.0
        left, right = cam("20261116-141102-gpt-6.1-sol"), cam("20261116-134633-gpt-5.6-sol")
        lab = ("drawtext=text='{t}':x=16:y=16:fontsize=26:fontcolor=white:box=1:boxcolor=0x000000AA:boxborderw=8"
               ":fontfile=/System/Library/Fonts/Supplemental/Arial.ttf")
        ff("-i", str(left), "-i", str(right), "-filter_complex",
           f"[0:v]crop=960:720:0:0,setpts=PTS/{speed},fps=24,scale=640:480,tpad=stop_mode=clone:stop_duration={length},"
           f"{lab.format(t='GPT-6.1 Sol  (run 141102)')}[a];"
           f"[1:v]crop=960:720:0:0,setpts=PTS/{speed},fps=24,scale=640:480,{lab.format(t='GPT-5.6 Sol  (run 134633)')}[b];"
           f"[a][b]hstack=inputs=2,trim=duration={length:.1f}[v]", "-map", "[v]", *ENC, "-crf", "28", str(out))
        return {"caption": "Same task, same speed (24x): GPT-6.1 Sol finishes in 13 minutes and holds still; GPT-5.6 Sol "
                           "drops the bottle on the way out of the basket and is still going at 24 minutes.",
                "poster_at": 20}

    def portal_pair(rid, out, start, length, speed):
        off = SCREEN_OFFSET.get(rid, 0.0)
        ff("-ss", f"{start:.2f}", "-t", f"{length:.2f}", "-i", str(cam(rid)),
           "-ss", f"{max(0.0, start - off):.2f}", "-t", f"{length:.2f}", "-i", str(scr(rid)), "-filter_complex",
           f"[0:v]crop=960:720:0:0,scale=720:540,setpts=PTS/{speed},fps=20[c];"
           f"[1:v]scale=960:540,setpts=PTS/{speed},fps=20[s];[s][c]hstack=inputs=2[v]",
           "-map", "[v]", *ENC, "-crf", "29", str(out))

    @job
    def portal_6_1(out):
        portal_pair("20261116-141102-gpt-6.1-sol", out, 0, 781, 16)
        return {"caption": "The portal and the overhead camera side by side for the whole of run 141102 at 16x: each "
                           "decision appears on the left as the arm carries it out on the right.", "poster_at": 30}

    @job
    def portal_cycle(out):
        portal_pair("20261116-130056-gpt-5.6-sol", out, 0, 774, 16)
        return {"caption": "Run 130056 (the full cycle in the hero video) in the portal and the overhead camera, 16x.",
                "poster_at": 30}

    def moment(rid, out, start, length, speed=1.0, crop=None, width=960):
        vf = (f"crop={crop}," if crop else "") + f"setpts=PTS/{speed},fps=16,scale={width}:-2"
        ff("-ss", f"{start:.2f}", "-t", f"{length:.2f}", "-i", str(cam(rid)), "-vf", vf, *ENC, "-crf", "26", str(out))

    @job
    def fail_drop(out):
        moment("20261116-134633-gpt-5.6-sol", out, 286, 42, speed=1.5, width=1280)
        return {"caption": "GPT-5.6 Sol (run 134633) lifts the bottle out of the basket and loses it while carrying it "
                           "to the table (1.5x).", "poster_at": 8}

    @job
    def fail_slip(out):
        moment("20261116-154924-gpt-5.6-sol", out, 300, 52, speed=1.5, width=1280)
        return {"caption": "GPT-5.6 Sol (run 154924) closes on the lying bottle and lifts; the bottle stays on the table. "
                           "It happened six times in this run (1.5x).", "poster_at": 10}

    @job
    def fail_fabric(out):
        moment("20261116-142403-gpt-5.6-sol", out, 900, 60, speed=1.5, width=1280)
        return {"caption": "GPT-5.6 Sol (run 142403) re-grasps inside the basket; the fingers stop at 56 mm, wider than "
                           "the bottle, and nothing comes up (1.5x).", "poster_at": 14}

    @job
    def recover_6_1(out):
        moment("20261116-150826-gpt-6.1-sol", out, 760, 420, speed=8, width=1280)
        return {"caption": "GPT-6.1 Sol (run 150826) closes on something at the far edge of the basket, lifts it, lowers "
                           "it back and lets go, then looks again (8x).", "poster_at": 6}

    @job
    def hover(out):
        timelapse("20261116-121848-gpt-5.6-sol", out, 2)
        return {"caption": "Before the depth map changed (run 121848): 12 moves, all above 18 cm, and the gripper never "
                           "closes. Moving parts only, 2x.", "poster_at": 6}

    return jobs


def full_videos(a, man):
    """Every run's camera video and portal recording, smaller, for the "watch" links."""
    R, S, O = Path(a.runs).expanduser(), Path(a.screens).expanduser(), Path(a.out) / "full"
    O.mkdir(parents=True, exist_ok=True)
    for v in sorted(R.glob("*/video.mp4")):
        rid = v.parent.name
        dst = O / f"{rid}-cameras.mp4"
        if not dst.exists():
            ff("-i", str(v), "-vf", "scale=1280:-2", *ENC, "-crf", "30", str(dst))
        man["full"][f"{rid}-cameras"] = {"path": f"media/full/{dst.name}", **probe(dst)}
    for v in sorted(S.glob("*-screen*.mp4")):
        rid = v.name.split("-screen")[0]
        dst = O / f"{rid}-portal.mp4"
        if not dst.exists():
            ff("-i", str(v), "-vf", "scale=1280:-2,fps=10", *ENC, "-crf", "32", str(dst))
        man["full"][f"{rid}-portal"] = {"path": f"media/full/{dst.name}", "partial": "partial" in v.name, **probe(dst)}


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--runs", required=True)
    ap.add_argument("--screens", required=True)
    ap.add_argument("--edit", required=True)
    ap.add_argument("--out", default="site/media")
    ap.add_argument("--only", nargs="*")
    ap.add_argument("--no-full", action="store_true")
    a = ap.parse_args()
    out = Path(a.out)
    (out / "clips").mkdir(parents=True, exist_ok=True)
    mpath = out / "manifest.json"
    man = json.loads(mpath.read_text()) if mpath.exists() else {"clips": {}, "full": {}}
    for cid, fn in clips(a).items():
        if a.only and cid not in a.only:
            continue
        dst = out / "clips" / f"{cid}.mp4"
        info = fn(dst)
        pos = out / "clips" / f"{cid}.jpg"
        poster(dst, min(info.pop("poster_at", 1.0), probe(dst)["duration"] - 0.2), pos, 960)
        man["clips"][cid] = {"path": f"media/clips/{dst.name}", "poster": f"media/clips/{pos.name}", **info, **probe(dst)}
        print(f"{cid}: {man['clips'][cid]['duration']} s, {man['clips'][cid]['bytes'] / 1e6:.1f} MB", flush=True)
    if not a.no_full:
        full_videos(a, man)
    mpath.write_text(json.dumps(man, indent=1))


if __name__ == "__main__":
    main()
