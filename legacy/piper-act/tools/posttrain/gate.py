"""Offline gate (spec §5.7): mean absolute error of the 30-step chunk against recorded actions, in the plugin's
normalized joint units, sampled every 15th frame and averaged over samples. ACT inference is deterministic, so no seed."""
from __future__ import annotations

from pathlib import Path

import numpy as np

from . import config as C


class PolicyRunner:
    def __init__(self, policy_dir: Path, device: str = "cpu", threads: int = 4):
        import torch
        from lerobot.policies.act.modeling_act import ACTPolicy
        from lerobot.policies.factory import make_pre_post_processors
        torch.set_num_threads(threads)
        self.torch = torch
        self.device = torch.device(device)
        self.model = ACTPolicy.from_pretrained(str(policy_dir))
        self.model.config.device = device
        self.model.to(self.device).eval()
        dev = {"device_processor": {"device": device}}
        self.pre, self.post = make_pre_post_processors(self.model.config, pretrained_path=str(policy_dir),
                                                       preprocessor_overrides=dev, postprocessor_overrides=dev)
        self.chunk_size = int(self.model.config.chunk_size)

    def chunk(self, state: np.ndarray, image_rgb_u8: np.ndarray) -> np.ndarray:
        from lerobot.policies.utils import prepare_observation_for_inference
        obs = {"observation.state": np.asarray(state, dtype=np.float32), C.IMAGE_KEY: image_rgb_u8}
        with self.torch.inference_mode():
            batch = self.pre(prepare_observation_for_inference(obs, self.device, None, "piper_follower"))
            actions = self.model.predict_action_chunk(batch)
            return np.stack([self.post(actions[:, i, :]).squeeze(0).cpu().numpy() for i in range(actions.shape[1])])


def episode_samples(root: Path, repo_id: str, episodes: list[int], stride: int = C.GATE_STRIDE, slices: dict | None = None):
    """Yield (episode, t, state[t], image[t] uint8 RGB, actions[t:]) for every stride-th frame.
    slices: optional {episode: (start, end)} offsets inside the episode (used to skip idle lead/tail of corrections)."""
    from . import lerodata as D
    ds = D.load(root, repo_id)
    ranges = D.episode_ranges(ds.meta)
    for e in episodes:
        lo, hi = ranges[e]
        if slices and e in slices:
            a, b = slices[e]
            lo, hi = lo + a, lo + b
        cols = ds.hf_dataset.select(range(lo, hi)).with_format("numpy")
        S = np.stack(cols["observation.state"]); A = np.stack(cols["action"])
        for t in range(0, hi - lo, stride):
            yield e, t, S[t], D.to_uint8_hwc(ds[lo + t][C.IMAGE_KEY]), A[t:]


def evaluate(policies: dict[str, Path], sets: dict[str, tuple[Path, str, list[int]]], device="cpu", log=print) -> dict:
    """Returns {set: {"samples": n, "hold_still": mae, policy_name: mae, ...}}. All policies see identical samples."""
    runners = {name: PolicyRunner(p, device=device) for name, p in policies.items()}
    out = {}
    for set_name, spec in sets.items():
        root, repo_id, episodes = spec[:3]
        slices = spec[3] if len(spec) > 3 else None
        if not episodes:
            out[set_name] = {"samples": 0, "episodes": [], "skipped": "no episodes"}
            continue
        err = {name: [] for name in runners}
        hold = []
        for e, t, s, img, acts in episode_samples(root, repo_id, episodes, slices=slices):
            n = min(len(acts), runners[next(iter(runners))].chunk_size)
            target = acts[:n]
            hold.append(float(np.abs(target - s[None, :]).mean()))
            for name, r in runners.items():
                err[name].append(float(np.abs(r.chunk(s, img)[:n] - target).mean()))
        out[set_name] = {"samples": len(hold), "episodes": episodes, "hold_still": round(float(np.mean(hold)), 4),
                         **{name: round(float(np.mean(v)), 4) for name, v in err.items()}}
        log(f"gate set {set_name}: {out[set_name]}")
    return out


def score(metrics: dict, candidate: str, current: str) -> float:
    """Lower is better: A and (when present) B errors, each relative to CURRENT."""
    a = metrics["A"][candidate] / metrics["A"][current]
    b = metrics.get("B", {})
    return a + (b[candidate] / b[current] if b.get("samples") else a)


def decide(metrics: dict, candidate: str, current: str) -> dict:
    """Spec §5.7: A <= 1.10 x CURRENT (no forgetting); B < CURRENT (learned the corrections; skipped if B is empty)."""
    a = metrics["A"]
    a_ok = a[candidate] <= 1.10 * a[current]
    b = metrics.get("B", {})
    if b.get("samples"):
        b_ok = b[candidate] < b[current]
        b_note = f"B {b[candidate]} < CURRENT {b[current]}: {b_ok}"
    else:
        b_ok, b_note = True, "B empty (no held-out human episodes): not applicable"
    return {"pass": bool(a_ok and b_ok), "A_ok": bool(a_ok), "B_ok": bool(b_ok),
            "A_rule": f"{a[candidate]} <= 1.10 x {a[current]} = {round(1.10 * a[current], 4)}", "B_rule": b_note}
