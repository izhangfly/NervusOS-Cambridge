"""Does the model plan to LEAVE the rest pose? Training start frames vs the live frame; state vs image effects."""
import sys
from pathlib import Path

import cv2
import numpy as np

SHARED = Path(r"C:\Users\zhy10\Shared\Piper Arm")
OUT = Path(__file__).parent
sys.path.insert(0, str(SHARED / "deploy"))
import piper_policy_portal as portal  # noqa: E402
from lerobot.datasets.lerobot_dataset import LeRobotDataset  # noqa: E402

J = portal.JOINTS
pol = portal.Policy(SHARED / "policies" / "act_pick_place_v1_100000", threads=4)
root = SHARED / "datasets" / "piper_pick_place"
ds = LeRobotDataset("local/piper_pick_place", root=root)
starts = ds.meta.episodes["dataset_from_index"]
ends = ds.meta.episodes["dataset_to_index"]


def mag(ch, st):
    d = ch - np.array([st[j] for j in J])
    return np.abs(d[:, :6]).max(1)  # per step: largest joint deviation from the current state


def frame(i):
    it = ds[i]
    img = (it["observation.images.wrist"].permute(1, 2, 0).numpy() * 255).round().astype(np.uint8)
    return img, {j: float(v) for j, v in zip(J, it["observation.state"].numpy())}


live = cv2.cvtColor(cv2.imread(str(OUT / "preflight_live.jpg")), cv2.COLOR_BGR2RGB)
print("ep | onset(s) | demo max dev @30 | model max dev @30 (train img) | model @30 (LIVE img, same state)")
rows = []
for e in range(0, 50, 5):
    a = int(starts[e])
    img, st = frame(a)
    acts = np.stack([np.asarray(ds.hf_dataset[a + k]["action"]) for k in range(60)])
    s0 = np.array([st[j] for j in J])
    dev = np.abs(acts[:, :6] - s0[:6]).max(1)
    onset = int(np.argmax(dev > 1.0)) if (dev > 1.0).any() else 60
    m_train = mag(pol.chunk(st, img), st)[-1]
    m_live = mag(pol.chunk(st, live), st)[-1]
    rows.append((m_train, m_live))
    print(f"{e:>2} | {onset/15:6.2f} | {dev[29]:8.2f} | {m_train:8.2f} | {m_live:8.2f}")
r = np.array(rows)
print(f"\nmean planned deviation after 2 s: train images {r[:,0].mean():.2f}, live image {r[:,1].mean():.2f}")

# after the arm has started rising (onset + 0.5 s), does the model keep going?
e = 5
a = int(starts[e])
for off in (0, 15, 30, 45, 60, 90):
    img, st = frame(a + off)
    print(f"ep {e} frame {off:>3}: model max dev @30 = {mag(pol.chunk(st, img), st)[-1]:6.2f}")
