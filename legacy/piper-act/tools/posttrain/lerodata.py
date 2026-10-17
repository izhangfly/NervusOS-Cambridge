"""LeRobot 0.4.4 dataset writing/reading for post-training. Every dataset is h264 (vcodec passed explicitly)."""
from __future__ import annotations

import json
import shutil
import time
from pathlib import Path

import numpy as np

from . import config as C


def base_meta():
    from lerobot.datasets.lerobot_dataset import LeRobotDatasetMetadata
    return LeRobotDatasetMetadata("local/piper_pick_place", root=C.BASE_DATASET)


def base_features() -> dict:
    """The base dataset's user features, exactly (names/shapes/dtypes), minus LeRobot's automatic ones."""
    info = json.loads((C.BASE_DATASET / "meta" / "info.json").read_text())
    feats = info["features"]
    out = {}
    for k in ("observation.state", "action", C.IMAGE_KEY):
        f = dict(feats[k])
        f["shape"] = tuple(f["shape"])  # info.json stores lists; LeRobot's validate_frame compares tuples
        f.pop("info", None)             # codec info is regenerated on encode (h264 here)
        out[k] = f
    return out


def state_bounds() -> tuple[np.ndarray, np.ndarray]:
    st = json.loads((C.BASE_DATASET / "meta" / "stats.json").read_text())["observation.state"]
    return np.array(st["min"], dtype=np.float64), np.array(st["max"], dtype=np.float64)


def create_dataset(root: Path, repo_id: str):
    from lerobot.datasets.lerobot_dataset import LeRobotDataset
    ds = LeRobotDataset.create(repo_id=repo_id, fps=C.FPS, features=base_features(), root=root,
                               robot_type="piper_follower", use_videos=True, vcodec=C.VCODEC,
                               streaming_encoding=True)
    return _quality(ds)


def _quality(ds):
    # LeRobot hard-codes crf=30, which measured 3.8/255 mean error on frame 0 (fails the spec's < 3/255 check).
    # StreamingVideoEncoder reads self.crf when each episode starts (video_utils.py:640), so override it here.
    if getattr(ds, "_streaming_encoder", None) is not None:
        ds._streaming_encoder.crf = C.CRF
    return ds


def open_for_append(root: Path, repo_id: str):
    from lerobot.datasets.lerobot_dataset import LeRobotDataset
    return _quality(LeRobotDataset(repo_id, root=root, vcodec=C.VCODEC, streaming_encoding=True))


def write_episode(ds, states: np.ndarray, frames, task: str) -> int:
    """states: (T+1, 7) contiguous measured states; frames: iterable of T+1 uint8 HxWx3 RGB images.
    Writes T frames with action[t] = state[t+1] (the last row has no next state and is dropped)."""
    states = np.asarray(states, dtype=np.float32)
    n = len(states) - 1
    it = iter(frames)
    for t in range(n):
        img = next(it)
        ds.add_frame({"observation.state": states[t], "action": states[t + 1], C.IMAGE_KEY: img, "task": task})
    ds.save_episode()
    return n


def to_uint8_hwc(chw_float) -> np.ndarray:
    import torch
    return torch.round(chw_float.clamp(0, 1) * 255).to(torch.uint8).permute(1, 2, 0).contiguous().numpy()


def episode_ranges(meta) -> dict[int, tuple[int, int]]:
    eps = meta.episodes
    return {int(e["episode_index"]): (int(e["dataset_from_index"]), int(e["dataset_to_index"])) for e in eps}


def load(root: Path, repo_id: str, episodes=None):
    from lerobot.datasets.lerobot_dataset import LeRobotDataset
    return LeRobotDataset(repo_id, root=root, episodes=episodes, vcodec=C.VCODEC)


def verify_dataset_episode(root: Path, repo_id: str, ep_index: int, expected_len: int, first_frame_rgb: np.ndarray | None,
                           expected_states: np.ndarray | None = None) -> dict:
    """Spec §5.3 checks: reloaded length == rows-1, action[t] == state[t+1], frame 0 MAD < 3/255 vs the source."""
    import torch
    ds = load(root, repo_id)
    lo, hi = episode_ranges(ds.meta)[ep_index]
    out = {"episode": ep_index, "length": hi - lo, "expected": expected_len}
    if hi - lo != expected_len:
        raise AssertionError(f"episode {ep_index}: length {hi - lo} != expected {expected_len}")
    cols = ds.hf_dataset.select(range(lo, hi)).with_format("numpy")
    S = np.stack(cols["observation.state"]); A = np.stack(cols["action"])
    if hi - lo > 1 and not np.array_equal(A[:-1], S[1:]):
        raise AssertionError(f"episode {ep_index}: action[t] != state[t+1]")
    if expected_states is not None and not np.allclose(S, expected_states[: hi - lo], atol=1e-4):
        raise AssertionError(f"episode {ep_index}: states differ from source")
    if first_frame_rgb is not None:
        f0 = ds[lo][C.IMAGE_KEY]
        mad = float(torch.abs(f0 - torch.from_numpy(first_frame_rgb).permute(2, 0, 1).float() / 255).mean())
        out["frame0_mad"] = round(mad, 5)
        if mad >= 3 / 255:
            raise AssertionError(f"episode {ep_index}: frame 0 MAD {mad:.4f} >= 3/255")
    return out


# ----------------------------------------------------------------------------------------------------------------------
def trim_onsets(meta=None) -> dict[int, dict]:
    """Spec §5.4: onset = first frame with max_j |state[t]-state[0]| (joint1..6) > 1.0; keep from max(0, onset-5)."""
    import glob
    import pandas as pd
    df = pd.concat([pd.read_parquet(f) for f in sorted(glob.glob(str(C.BASE_DATASET / "data" / "*" / "*.parquet")))])
    df = df.sort_values("index")
    S = np.stack(df["observation.state"].values)
    E = df["episode_index"].values
    out = {}
    for e in np.unique(E):
        s = S[E == e]
        d = np.abs(s[:, :6] - s[0, :6]).max(axis=1)
        moving = d > C.TRIM_THRESHOLD
        onset = int(np.argmax(moving)) if moving.any() else None
        keep_from = max(0, onset - C.TRIM_KEEP_BEFORE) if onset is not None else 0
        out[int(e)] = {"length": int(len(s)), "onset": onset, "keep_from": keep_from, "removed": keep_from}
    return out


def build_base_trimmed(dest: Path = None, episodes: list[int] | None = None, log=print) -> dict:
    """Decode the base dataset once and write the idle-trimmed copy (h264). Never modifies the base dataset."""
    from lerobot.datasets.lerobot_dataset import LeRobotDataset
    dest = Path(dest or C.BASE_TRIMMED)
    if dest.exists():
        raise FileExistsError(f"{dest} already exists")
    task = C.load_contract()["task"]
    onsets = trim_onsets()
    src = LeRobotDataset("local/piper_pick_place", root=C.BASE_DATASET)
    ranges = episode_ranges(src.meta)
    tmp = dest.with_name(dest.name + ".partial")
    shutil.rmtree(tmp, ignore_errors=True)
    ds = create_dataset(tmp, "local/piper_pick_place_trimmed")
    report, t0, new_idx = [], time.time(), 0
    todo = episodes if episodes is not None else sorted(ranges)
    for e in todo:
        lo, hi = ranges[e]
        k0 = onsets[e]["keep_from"]
        # base rows already satisfy action[t] = state[t+1] within an episode, and its last row was dropped when recorded,
        # so keep base (state, action) pairs as they are rather than re-deriving.
        cols = src.hf_dataset.select(range(lo + k0, hi)).with_format("numpy")
        S = np.stack(cols["observation.state"]).astype(np.float32)
        A = np.stack(cols["action"]).astype(np.float32)
        first = None
        for i, gi in enumerate(range(lo + k0, hi)):
            img = to_uint8_hwc(src[gi][C.IMAGE_KEY])
            if i == 0:
                first = img
            ds.add_frame({"observation.state": S[i], "action": A[i], C.IMAGE_KEY: img, "task": task})
        ds.save_episode()
        report.append({"src_episode": e, "episode": new_idx, "frames_before": hi - lo, "removed": k0, "frames_after": hi - lo - k0,
                       "onset": onsets[e]["onset"], "_first": first, "_S": S})
        new_idx += 1
        log(f"base_trimmed: episode {e} -> {new_idx - 1}: {hi - lo} -> {hi - lo - k0} frames ({time.time() - t0:.0f}s)")
    ds.finalize()
    for r in report:
        r["verify"] = verify_dataset_episode(tmp, "local/piper_pick_place_trimmed", r["episode"], r["frames_after"], r.pop("_first"),
                                             expected_states=r.pop("_S"))
    tmp.rename(dest)
    summary = {"built": C.utc_now(), "source": str(C.BASE_DATASET), "vcodec": C.VCODEC, "rule": {
        "threshold": C.TRIM_THRESHOLD, "keep_before": C.TRIM_KEEP_BEFORE, "joints": "joint1..joint6"},
        "episodes": report, "frames_removed": int(sum(r["removed"] for r in report)),
        "frames_total": int(sum(r["frames_after"] for r in report)),
        "episodes_losing_over_25": int(sum(r["removed"] > 25 for r in report)), "seconds": round(time.time() - t0, 1)}
    C.write_json_atomic(dest / "TRIM_REPORT.json", summary)
    return summary
