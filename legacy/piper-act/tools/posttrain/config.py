"""Paths, contract loading and small I/O helpers shared by every post-training module."""
from __future__ import annotations

import json
import os
import time
from datetime import datetime, timezone
from pathlib import Path

SUPPORTED_CONTRACT = 1

SHARED = Path(os.environ.get("PIPER_SHARED", "/Users/ianzhang/Shared/Piper Arm"))
CONTRACT_PATH = SHARED / "posttrain" / "CONTRACT.json"
ROLLOUTS = SHARED / "posttrain" / "rollouts"          # Dell-owned: read only
MAC_SHARED = SHARED / "posttrain" / "mac"             # Mac-owned, synced
POLICIES = SHARED / "policies"                        # Mac writes new policy dirs + CURRENT.json
RESULTS_CSV = SHARED / "deploy" / "runs" / "results.csv"  # Dell-owned: read only
UPDATE_MD = SHARED / "UPDATE.md"
BASE_DATASET = SHARED / "datasets" / "piper_pick_place"   # read only

VLA = Path(__file__).resolve().parents[2]
LOCAL = Path(os.environ.get("PIPER_POSTTRAIN_LOCAL", VLA / "posttrain"))  # NOT synced
BASE_TRIMMED = LOCAL / "base_trimmed"
ROLLOUT_EPS = LOCAL / "rollout_eps"
VENV_PY = VLA / ".venv" / "bin" / "python"
LEROBOT_TRAIN = VLA / ".venv" / "bin" / "lerobot-train"

VCODEC = "h264"          # pass explicitly every time a LeRobotDataset is created/opened (spec §5.3)
CRF = 18                 # LeRobot default 30 loses 3.8/255 on frame 0; 18 keeps re-encodes near-lossless
FPS = 15
IMAGE_KEY = "observation.images.wrist"
GATE_SET_A = [4, 13, 27, 41, 49]
GATE_STRIDE = 15
TRIM_THRESHOLD = 1.0
TRIM_KEEP_BEFORE = 5
STATE_RANGE_MARGIN = 10.0
# joint4 and joint6 are the roll joints: rotated-block corrections legitimately twist past the demos' range, so they
# get a wider margin (a 57.9 wrist roll, 0.4 past demo max + 10, was wrongly rejected on 2026-09-15).
STATE_RANGE_MARGINS = [10.0, 10.0, 10.0, 30.0, 10.0, 30.0, 10.0]
HOLDOUT_FRACTION = 0.15   # share of not-yet-trained human corrections held out as gate set B
HOLDOUT_MAX = 5
ROLLOUT_TRIM_THRESHOLD = 1.0  # leading/trailing stillness trim for rollout episodes, all 7 dims (gripper release counts)
ROLLOUT_TRIM_KEEP = 5
TRIGGER_NEW_ACCEPTED = 10
TRIGGER_NEW_HUMAN = 5
MIN_HOURS_BETWEEN_ROUNDS = 6.0
HUMAN_MIN_FRACTION = 0.15
FINETUNE_STEPS = 20000
SAVE_FREQ = 5000


def utc_now() -> str:
    return datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")


def read_json(path: Path, default=None):
    try:
        return json.loads(Path(path).read_text(encoding="utf-8"))
    except FileNotFoundError:
        return default


def write_json_atomic(path: Path, obj) -> None:
    path = Path(path)
    path.parent.mkdir(parents=True, exist_ok=True)
    tmp = path.with_name(f".{path.name}.tmp")
    tmp.write_text(json.dumps(obj, indent=2, ensure_ascii=False), encoding="utf-8")
    os.replace(tmp, path)


class ContractError(RuntimeError):
    pass


def load_contract() -> dict:
    c = read_json(CONTRACT_PATH)
    if c is None:
        raise ContractError(f"{CONTRACT_PATH} missing")
    v = c.get("contract_version")
    if v != SUPPORTED_CONTRACT:
        raise ContractError(f"contract_version {v!r} is not supported by this Mac pipeline (knows {SUPPORTED_CONTRACT})")
    return c


def set_status(state: str, detail: str, **extra) -> None:
    write_json_atomic(MAC_SHARED / "STATUS.json", {"state": state, "detail": detail, "updated": utc_now(), **extra})


def log(msg: str) -> None:
    line = f"[{utc_now()}] {msg}"
    print(line, flush=True)
    LOCAL.mkdir(parents=True, exist_ok=True)
    with open(LOCAL / "pipeline.log", "a", encoding="utf-8") as f:
        f.write(line + "\n")


def since_hours(iso: str | None) -> float:
    if not iso:
        return float("inf")
    t = datetime.strptime(iso, "%Y-%m-%dT%H:%M:%SZ").replace(tzinfo=timezone.utc).timestamp()
    return (time.time() - t) / 3600
