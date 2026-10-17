"""Rollout packages (contract v1 §3): scan, validate, and split into training episodes."""
from __future__ import annotations

import json
import time
from dataclasses import dataclass, field, asdict
from pathlib import Path

import numpy as np

from . import config as C

SETTLE_S = 60  # Syncthing may deliver READY before meta.json; wait until the folder has been still this long


class Pending(Exception):
    """Not a failure: the package is still arriving. Retried on the next scan."""


class Rejected(Exception):
    """A real contract violation. Logged in the ledger once and never retried."""


@dataclass
class EpisodeSpec:
    id: str                 # "<run_id>#h0" (human) or "<run_id>#p0" (policy)
    run_id: str
    kind: str               # human | policy
    row_start: int          # first row (inclusive)
    row_end: int            # last row (exclusive); frames written = rows - 1
    rows: int = 0
    frames: int = 0
    created: str = ""       # run creation time from meta.json, orders "newest"

    def to_dict(self):
        return asdict(self)


@dataclass
class Package:
    run_id: str
    dir: Path
    meta: dict
    rows: list = field(repr=False)
    ready: str = ""


def scan_ready() -> list[Path]:
    if not C.ROLLOUTS.is_dir():
        return []
    return sorted(p.parent for p in C.ROLLOUTS.glob("*/READY"))


def _settled(d: Path) -> bool:
    newest = max((f.stat().st_mtime for f in d.iterdir() if f.is_file()), default=0)
    return time.time() - newest >= SETTLE_S


def load_and_validate(d: Path, bounds: tuple[np.ndarray, np.ndarray], decode_video: bool = True) -> Package:
    """Raises Pending or Rejected. Returns a fully validated package."""
    run_id = d.name
    ready = d / "READY"
    if not ready.exists():
        raise Pending("no READY")
    if not _settled(d):
        raise Pending(f"files changed in the last {SETTLE_S}s")
    try:
        meta = json.loads((d / "meta.json").read_text(encoding="utf-8"))
    except FileNotFoundError:
        raise Pending("meta.json not delivered yet")
    except json.JSONDecodeError as e:
        raise Rejected(f"meta.json does not parse: {e}")
    if "outcome" not in meta:
        raise Pending("meta.json has no outcome yet (label still syncing)")
    for needed in ("episode.jsonl", "wrist.mp4"):
        if not (d / needed).exists():
            raise Pending(f"{needed} not delivered yet (Syncthing still copying)")
    ready_text = ready.read_text(encoding="utf-8").strip()
    if ready_text != meta["outcome"]:
        raise Pending(f"READY says {ready_text!r}, meta.json says {meta['outcome']!r}")

    # contract v1 §3.2 reject rules
    if meta.get("mock") is True:
        raise Rejected("mock == true")
    if meta.get("contract_version") != C.SUPPORTED_CONTRACT:
        raise Rejected(f"contract_version {meta.get('contract_version')!r} != {C.SUPPORTED_CONTRACT}")
    if meta.get("writer_error") not in (None, ""):
        raise Rejected(f"writer_error: {meta['writer_error']}")
    if meta["outcome"] not in ("success", "fail"):
        raise Rejected(f"outcome {meta['outcome']!r} is not success|fail")

    rows = []
    try:
        with open(d / "episode.jsonl", encoding="utf-8") as f:
            for i, line in enumerate(f):
                if not line.strip():
                    continue
                r = json.loads(line)
                s = r["state"]
                if not (isinstance(s, list) and len(s) == 7 and all(isinstance(v, (int, float)) for v in s)):
                    raise Rejected(f"row {i}: state is not 7 numbers")
                if r["segment"] not in ("policy", "human"):
                    raise Rejected(f"row {i}: segment {r['segment']!r}")
                if int(r["frame"]) != len(rows):
                    raise Rejected(f"row {i}: frame {r['frame']} != row index {len(rows)}")
                int(r["k"])
                rows.append(r)
    except FileNotFoundError:
        raise Pending("episode.jsonl not delivered yet")
    except (json.JSONDecodeError, KeyError, TypeError, ValueError) as e:
        raise Rejected(f"episode.jsonl row does not parse: {e}")
    if meta.get("frames") != len(rows):
        raise Rejected(f"frames {meta.get('frames')} != rows {len(rows)}")
    if len(rows) and any(rows[i + 1]["k"] <= rows[i]["k"] for i in range(len(rows) - 1)):
        raise Rejected("k is not strictly increasing")

    lo, hi = bounds
    S = np.array([r["state"] for r in rows], dtype=np.float64) if rows else np.zeros((0, 7))
    if len(S):
        margin = np.array(C.STATE_RANGE_MARGINS, dtype=np.float64)
        below = S < lo - margin
        above = S > hi + margin
        if below.any() or above.any():
            i, j = np.argwhere(below | above)[0]
            raise Rejected(f"row {i} joint {j}: state {S[i, j]:.2f} outside base range [{lo[j]:.1f}, {hi[j]:.1f}] +/- {margin[j]:.0f}")

    if decode_video:
        n = 0
        for img in iter_frames(d / "wrist.mp4"):
            if img.shape != (480, 640, 3):
                raise Rejected(f"wrist.mp4 frame {n} has shape {img.shape}, expected (480, 640, 3)")
            n += 1
        if n != len(rows):
            raise Rejected(f"wrist.mp4 decodes to {n} frames, rows {len(rows)}")
    return Package(run_id=run_id, dir=d, meta=meta, rows=rows, ready=ready_text)


def iter_frames(mp4: Path, start: int = 0, stop: int | None = None):
    """Decode RGB uint8 frames [start, stop). PyAV rgb24 == OpenCV BGR converted to RGB."""
    import av
    try:
        with av.open(str(mp4)) as c:
            for i, fr in enumerate(c.decode(video=0)):
                if i < start:
                    continue
                if stop is not None and i >= stop:
                    break
                yield fr.to_ndarray(format="rgb24")
    except av.error.FFmpegError as e:
        raise Rejected(f"wrist.mp4 does not decode: {e}")


def split_episodes(pkg: Package) -> tuple[list[EpisodeSpec], list[str]]:
    """Contract v1 §3.3. Returns (episodes, notes)."""
    rows, meta, notes = pkg.rows, pkg.meta, []
    min_rows = C.load_contract()["episode_rules"].get("min_rows", 30)
    # contiguous blocks: a k gap (> 1) or a segment change starts a new block
    blocks, start = [], 0
    for i in range(1, len(rows) + 1):
        if i == len(rows) or rows[i]["k"] - rows[i - 1]["k"] != 1 or rows[i]["segment"] != rows[i - 1]["segment"]:
            blocks.append((rows[start]["segment"], start, i))
            start = i
    created = meta.get("created", "")
    eps: list[EpisodeSpec] = []
    h = p = 0
    for seg, a, b in blocks:
        n = b - a
        if seg == "human":
            if n >= min_rows:
                eps.append(EpisodeSpec(f"{pkg.run_id}#h{h}", pkg.run_id, "human", a, b, n, n - 1, created)); h += 1
            else:
                notes.append(f"human block rows {a}-{b} has {n} rows < {min_rows}: skipped")
    has_human = any(r["segment"] == "human" for r in rows)
    if meta["outcome"] == "success" and int(meta.get("interventions") or 0) == 0 and not has_human:
        for seg, a, b in blocks:
            n = b - a
            if n >= min_rows:
                eps.append(EpisodeSpec(f"{pkg.run_id}#p{p}", pkg.run_id, "policy", a, b, n, n - 1, created)); p += 1
            else:
                notes.append(f"policy block rows {a}-{b} has {n} rows < {min_rows}: skipped")
    elif meta["outcome"] == "success" and (int(meta.get("interventions") or 0) > 0 or has_human):
        notes.append("success with an intervention: policy rows never used (contract §3.3)")
    else:
        notes.append("failed run: policy rows never used (contract §3.3)")
    gaps = sum(1 for i in range(1, len(rows)) if rows[i]["k"] - rows[i - 1]["k"] != 1)
    if gaps:
        notes.append(f"{gaps} k gap(s): split there")
    return eps, notes
