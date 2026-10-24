"""Settings, safety limits and the model providers' API keys.

Files, all in the config folder (default ~/.config/nervus; --sim uses "-sim" names so a simulated calibration
never overwrites the real one):

    deepseek.key            DeepSeek API key, one line, mode 0600 (or DEEPSEEK_API_KEY in the environment)
    openai.key              OpenAI API key for the GPT models, same rules (or OPENAI_API_KEY)
    llm-arm.json            settings changed from the app
    llm-arm-calib.json      scene camera calibration and the table height

Keys are never logged, never returned by the API and never written by this program.
"""
from __future__ import annotations

import json
import math
import os
import re
import stat
import threading
from pathlib import Path

DEFAULT_CONFIG_DIR = Path.home() / ".config" / "nervus"
# Every provider speaks the OpenAI Chat Completions API (POST {base_url}/chat/completions, Bearer key, tools).
# "extra" is merged into each request body: DeepSeek's thinking mode would otherwise require sending every
# earlier reasoning_content back on each tool round.
PROVIDERS = {
    "deepseek": {"label": "DeepSeek", "base_url": "https://api.deepseek.com", "model": "deepseek-flash",
                 "key_file": "deepseek.key", "key_env": "DEEPSEEK_API_KEY",
                 "extra": {"thinking": {"type": "disabled"}}},
    "openai": {"label": "OpenAI GPT", "base_url": "https://api.openai.com/v1", "model": "gpt-6-astra",
               "key_file": "openai.key", "key_env": "OPENAI_API_KEY", "extra": {}},
}
# The systemd unit reads an optional env file (EnvironmentFile=-...) and names it here, so a key that came from a
# file anyone can read is refused just like a world-readable deepseek.key.
ENV_FILE_ENV = "NERVUS_LLMARM_ENV_FILE"
def key_help(provider: str) -> str:
    f = PROVIDERS[provider]["key_file"]
    return (f"Put the {PROVIDERS[provider]['label']} key on this machine, readable only by you: "
            f"mkdir -p ~/.config/nervus && install -m 600 /dev/stdin ~/.config/nervus/{f} "
            "(paste the key, press Enter, then Ctrl-D), then press Reload keys.")

# ---- motion limits (millimetres, degrees) ------------------------------------------------------------------
# Hand Teleop's box (src/hand-teleop Teleop.kt, metres there), a little inside the driver's own box.
WORKSPACE_MM = {"x": (80.0, 460.0), "y": (-300.0, 300.0), "z": (100.0, 550.0)}
# Until the table height is calibrated nothing may go below this: high enough for any gripper on the PiPER.
UNCALIBRATED_Z_FLOOR_MM = 200.0
TABLE_CLEARANCE_MM = 5.0         # the TCP stays this far above "fingertips touch the table"
MAX_RELATIVE_MM = 50.0           # per axis, per move_relative call
HARD_MAX_SPEED_MM_S = 100.0      # the speed setting can never exceed this
YAW_RATE_DEG_S = 30.0
MAX_STEPS = 25                   # decisions per request (piper-astra-jev runs 25)
STALE_FRAME_S = 5.0
HEARTBEAT_TIMEOUT_S = 2.0        # the app's dead-man while the arm moves
OBJECT_MAX_AGE_S = 30.0          # pick/place refuse objects not seen for longer than this

SETTINGS: dict[str, tuple] = {
    # name: (default, low, high, type, what it does)
    "frame_interval_s": (2.0, 0.5, 10.0, float, "Seconds between camera grabs"),
    "speed_mm_s": (50.0, 5.0, HARD_MAX_SPEED_MM_S, float, "Top speed of the gripper along every straight-line move"),
    "confirm_moves": (True, None, None, bool, "Ask for Approve before every motion step"),
    "images": (True, None, None, bool, "Send the camera images to the model with each request"),
    "provider": ("deepseek", None, None, str, "Which model provider answers: " + ", ".join(PROVIDERS)),
    "deepseek_model": (PROVIDERS["deepseek"]["model"], None, None, str, "DeepSeek model (it must accept images)"),
    "deepseek_base_url": (PROVIDERS["deepseek"]["base_url"], None, None, str, "DeepSeek API base URL"),
    "openai_model": (PROVIDERS["openai"]["model"], None, None, str, "OpenAI model (it must accept images)"),
    "openai_base_url": (PROVIDERS["openai"]["base_url"], None, None, str, "OpenAI API base URL"),
    "openai_effort": ("medium", None, None, str, "GPT reasoning effort: none, low, medium or high (high is slow)"),
    "llm_timeout_s": (120.0, 5.0, 600.0, float, "How long to wait for one model reply"),
    "grip_effort": (500, 100, 3000, int, "Gripper force, 0.001 N*m (1000 crushed a printed cube)"),
    "object_height_mm": (30.0, 5.0, 120.0, float, "Height of the objects on the table"),
    "open_width_mm": (70.0, 20.0, 95.0, float, "Gripper opening before a grasp and when letting go"),
    "travel_height_mm": (120.0, 40.0, 300.0, float, "Height above the table for moves between objects"),
    "container_rim_mm": (100.0, 10.0, 250.0, float, "Rim height of a tray or box things are put into"),
}
MODEL_RE = re.compile(r"^[A-Za-z0-9._:-]{1,64}$")


def _coerce(name: str, value):
    default, lo, hi, typ, _ = SETTINGS[name]
    if typ is bool:
        if isinstance(value, str):
            value = value.strip().lower() in ("1", "true", "yes", "on")
        return bool(value)
    if typ is str:
        v = str(value).strip()
        if name == "openai_effort" and v not in ("none", "low", "medium", "high"):
            raise ValueError("openai_effort must be none, low, medium or high")
        if name == "provider" and v not in PROVIDERS:
            raise ValueError("provider must be one of " + ", ".join(PROVIDERS))
        if name.endswith("_model") and not MODEL_RE.match(v):
            raise ValueError("model: letters, digits and . _ : - only")
        if name.endswith("_base_url"):
            v = v.rstrip("/")
            local = re.match(r"^http://(127\.0\.0\.1|localhost)(:\d+)?(/.*)?$", v)
            if not (v.startswith("https://") or local):
                raise ValueError("base_url must be https:// (or http://127.0.0.1 for a local test server)")
        return v
    v = typ(value)
    if isinstance(v, float) and not math.isfinite(v):
        raise ValueError(f"{name} must be a finite number")
    return typ(min(hi, max(lo, v)))


class Settings:
    """The settings the app can change, saved to a JSON file. Unknown keys are refused, values are clamped."""

    def __init__(self, path: Path | None):
        self.path = path
        self._lock = threading.Lock()
        self._v = {k: spec[0] for k, spec in SETTINGS.items()}
        if path and path.exists():
            try:
                saved = json.loads(path.read_text())
                for k, v in saved.items():
                    if k in SETTINGS:
                        try:
                            self._v[k] = _coerce(k, v)
                        except (TypeError, ValueError):
                            pass
            except (OSError, ValueError):
                pass

    def __getitem__(self, k):
        with self._lock:
            return self._v[k]

    def all(self) -> dict:
        with self._lock:
            return dict(self._v)

    def describe(self) -> dict:
        return {k: {"value": self[k], "min": s[1], "max": s[2], "type": s[3].__name__, "help": s[4]}
                for k, s in SETTINGS.items()}

    def update(self, changes: dict) -> dict:
        new = {}
        for k, v in (changes or {}).items():
            if k not in SETTINGS:
                raise ValueError(f"unknown setting {k!r}")
            try:
                new[k] = _coerce(k, v)
            except (TypeError, ValueError) as e:
                raise ValueError(f"{k}: {e}")
        with self._lock:
            self._v.update(new)
            snapshot = dict(self._v)
        if self.path:
            write_json(self.path, snapshot)
        return snapshot


def write_json(path: Path, obj, mode: int = 0o644):
    path.parent.mkdir(parents=True, exist_ok=True)
    tmp = path.with_suffix(path.suffix + ".tmp")
    tmp.write_text(json.dumps(obj, indent=2))
    os.chmod(tmp, mode)
    os.replace(tmp, path)


# ---- the key ------------------------------------------------------------------------------------------------
def file_problem(p: Path) -> str | None:
    """Why a secret file must not be used, or None. It must be a regular file, ours, and private (0600 or 0400)."""
    try:
        st = os.stat(p)
    except OSError as e:
        return f"cannot be read ({e.strerror})"
    if not stat.S_ISREG(st.st_mode):
        return "is not a regular file"
    if hasattr(os, "getuid") and st.st_uid != os.getuid():
        return "belongs to another user"
    if st.st_mode & 0o077:
        return f"can be read by other users (mode {oct(st.st_mode & 0o777)[2:]})"
    return None


def load_key(config_dir: Path, provider: str = "deepseek", environ=None) -> tuple[str | None, dict]:
    """(key or None, status) for one provider. The status never contains the key. The environment wins."""
    environ = os.environ if environ is None else environ
    KEY_ENV = PROVIDERS[provider]["key_env"]
    KEY_HELP = key_help(provider)
    label = PROVIDERS[provider]["label"]
    key_path = Path(config_dir) / PROVIDERS[provider]["key_file"]
    shown = str(key_path).replace(str(Path.home()), "~", 1)
    env = (environ.get(KEY_ENV) or "").strip()
    if env:
        env_file = environ.get(ENV_FILE_ENV)
        if env_file and os.path.exists(env_file):
            problem = file_problem(Path(env_file))
            if problem:
                return None, {"state": "refused", "source": "env",
                              "message": f"The key comes from {env_file}, which {problem}. Run: chmod 600 {env_file}"}
        if not _plausible(env):
            return None, {"state": "refused", "source": "env", "message": f"{KEY_ENV} does not look like an API key"}
        return env, {"state": "ready", "source": "env", "message": f"Key from the {KEY_ENV} environment variable"}
    if not key_path.exists():
        return None, {"state": "no key", "source": None, "message": f"No {label} key ({shown} is missing). {KEY_HELP}"}
    problem = file_problem(key_path)
    if problem:
        return None, {"state": "refused", "source": "file",
                      "message": f"Refusing the key: {shown} {problem}. Run: chmod 600 {shown}"}
    try:
        key = key_path.read_text().strip()
    except OSError as e:
        return None, {"state": "refused", "source": "file", "message": f"{shown}: {e.strerror}"}
    if not _plausible(key):
        return None, {"state": "refused", "source": "file",
                      "message": f"{shown} does not hold a single-line API key"}
    return key, {"state": "ready", "source": "file", "message": f"Key from {shown}"}


def _plausible(key: str) -> bool:
    return 8 <= len(key) <= 256 and not any(c.isspace() for c in key) and key.isprintable()
