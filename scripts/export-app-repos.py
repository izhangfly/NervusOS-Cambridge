#!/usr/bin/env python3
"""Export the PiPER desktop apps from this monorepo into their own GitHub repositories.

    scripts/export-app-repos.py                  all of them
    scripts/export-app-repos.py piper-llm-arm    one
    scripts/export-app-repos.py --dry-run        assemble and scan, don't commit or push

This monorepo stays the source of truth: edit here, then export. Each app repository is assembled from the
folders listed in REPOS plus its own files under export/<repo>/ (README, LICENSE, NOTICE, install.sh), staged
in ~/NervusOS-exports/<repo> (a git checkout of the app repository), secret-scanned, committed as the owner
and pushed. A repository that doesn't exist on GitHub yet is created (public).
"""
import shutil
import subprocess
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
STAGE = Path.home() / "NervusOS-exports"
OWNER = "izhangfly"
AUTHOR = ["-c", "user.name=izhangfly", "-c", "user.email=144861428+izhangfly@users.noreply.github.com"]

# The Kotlin apps build against the NervusOS app SDK as included builds (siblings at the repo root, as in
# src/). The SDK only needs nervus-ipc's generated Java protocol classes, not the Go code.
SDK = [
    ("src/nervus-app-sdk", "nervus-app-sdk"),
    ("src/nervus-packaging", "nervus-packaging"),
    ("src/nervus-ipc/jvm/protocol/src/main", "nervus-ipc/jvm/protocol/src/main"),
    ("src/nervus-ipc/LICENSE", "nervus-ipc/LICENSE"),
]
DRIVER_ONLY = [
    ("hardware/piper", "driver"),
    ("hosted/libexec/can-up", "driver/can-up"),
    ("hosted/udev/90-nervus-can.rules", "driver/90-nervus-can.rules"),
    ("export/common/systemd", "driver/systemd"),
]
DRIVER = [
    ("hardware/piper", "driver"),
    ("hosted/libexec/can-up", "driver/can-up"),
    ("hosted/udev/90-nervus-can.rules", "driver/90-nervus-can.rules"),
    ("export/common/systemd", "driver/systemd"),
    ("export/common/50-piper-apps.rules.in", "driver/50-piper-apps.rules.in"),
    ("export/common/install-common.sh", "scripts/install-common.sh"),
    ("hosted/bin/nervus-standalone", "desktop/nervus-standalone"),
]

REPOS = {
    "piper-hand-teleop": {
        "description": "Drive an AgileX PiPER arm with your bare hand in front of a webcam: pose + hand landmarks, "
                       "metric depth, wrist roll and pinch-to-grip. Runs on its own or as a NervusOS app.",
        "topics": ["robotics", "teleoperation", "agilex-piper", "mediapipe", "onnxruntime", "compose-desktop"],
        "paths": SDK + DRIVER + [
            ("src/hand-teleop", "hand-teleop"),
            ("hosted/applications/nervus-handteleop.desktop", "desktop/nervus-handteleop.desktop"),
            ("hosted/icons/nervus-handteleop.svg", "desktop/nervus-handteleop.svg"),
            ("hosted/libexec/surface-camera", "surface-camera/surface-camera"),
            ("hosted/systemd/nervus-surface-camera.service", "surface-camera/nervus-surface-camera.service"),
            ("hosted/surface", "surface-camera"),
        ],
    },
    "piper-llm-arm": {
        "description": "Tell an AgileX PiPER arm what to do in plain language: GPT-6 Astra or DeepSeek grounds the "
                       "request in the camera image and picks one bounded skill at a time under a safety governor.",
        "topics": ["robotics", "llm", "vision-language-model", "agilex-piper", "deepseek", "openai"],
        "paths": SDK + DRIVER + [
            ("llm-arm", "service"),
            ("src/llm-arm", "llm-arm"),
            ("hosted/systemd/nervus-llm-arm.service.in", "service/nervus-llm-arm.service.in"),
            ("hosted/applications/nervus-llmarm.desktop", "desktop/nervus-llmarm.desktop"),
            ("hosted/icons/nervus-llmarm.svg", "desktop/nervus-llmarm.svg"),
        ],
    },
    "piper-act-studio": {
        "description": "Teach an AgileX PiPER by demonstration and run what it learned: record LeRobot datasets, "
                       "train ACT on a Mac (or a GPU), gate, run and post-train on corrections, from one desktop app.",
        "topics": ["robotics", "imitation-learning", "lerobot", "act", "agilex-piper", "compose-desktop"],
        "paths": SDK + DRIVER + [
            ("act", "act"),
            ("src/act-studio", "act-studio"),
            ("hosted/systemd/nervus-act.service.in", "act/nervus-act.service.in"),
            ("hosted/applications/nervus-act.desktop", "desktop/nervus-act.desktop"),
            ("hosted/icons/nervus-act.svg", "desktop/nervus-act.svg"),
        ],
    },
    "piper-smolvla": {
        "description": "Fine-tune and run SmolVLA on an AgileX PiPER with LeRobot: train on a Mac or a rented GPU, "
                       "serve the policy on the Mac, drive the arm from the robot host.",
        "topics": ["robotics", "vla", "smolvla", "lerobot", "agilex-piper"],
        "paths": [("smolvla", ".")] + DRIVER_ONLY,
    },
}
IGNORE = shutil.ignore_patterns("build", ".gradle", ".kotlin", "__pycache__", "*.pyc", ".pytest_cache",
                                "*.pem", ".venv", "venv", ".DS_Store", "local.properties")


def run(*cmd, cwd=None, check=True, capture=False):
    return subprocess.run(cmd, cwd=cwd, check=check, text=True, capture_output=capture)


def assemble(name: str, spec: dict, dest: Path):
    for child in dest.iterdir():
        if child.name != ".git":
            shutil.rmtree(child) if child.is_dir() else child.unlink()
    for src, dst in spec["paths"]:
        s, d = ROOT / src, dest / dst
        if not s.exists():
            raise SystemExit(f"{name}: {src} is missing")
        d.parent.mkdir(parents=True, exist_ok=True)
        if s.is_dir():
            shutil.copytree(s, d, ignore=IGNORE, dirs_exist_ok=True)
        else:
            shutil.copy2(s, d)
    if (ROOT / "export" / name).exists():
        shutil.copytree(ROOT / "export" / name, dest, ignore=IGNORE, dirs_exist_ok=True)


def main():
    args = [a for a in sys.argv[1:] if not a.startswith("--")]
    dry = "--dry-run" in sys.argv
    names = args or list(REPOS)
    STAGE.mkdir(exist_ok=True)
    for name in names:
        spec = REPOS[name]
        missing = [s for s, _ in spec["paths"] if not (ROOT / s).exists()]
        if missing:
            print(f"== {name}: skipped, not in this checkout yet: {', '.join(missing)}")
            continue
        dest = STAGE / name
        exists = run("gh", "repo", "view", f"{OWNER}/{name}", check=False, capture=True).returncode == 0
        if not dest.exists():
            if exists:
                run("git", "clone", "-q", f"https://github.com/{OWNER}/{name}.git", str(dest))
            else:
                dest.mkdir()
                run("git", "init", "-q", "-b", "main", cwd=dest)
        assemble(name, spec, dest)
        run("git", "add", "-A", cwd=dest)
        if run(sys.executable, str(ROOT / "scripts/secret-scan.py"), cwd=dest, check=False).returncode:
            raise SystemExit(f"{name}: the secret scan failed; nothing was committed")
        if not run("git", "status", "--porcelain", cwd=dest, capture=True).stdout.strip():
            print(f"== {name}: up to date")
            continue
        src_rev = run("git", "rev-parse", "--short", "HEAD", cwd=ROOT, capture=True).stdout.strip()
        if dry:
            print(f"== {name}: assembled in {dest} (dry run)")
            continue
        run("git", *AUTHOR, "commit", "-q", "-m", f"Update from NervusOS-Cambridge {src_rev}", cwd=dest)
        if not exists:
            run("gh", "repo", "create", f"{OWNER}/{name}", "--public", "--description", spec["description"],
                "--source", str(dest), "--remote", "origin")
            run("gh", "repo", "edit", f"{OWNER}/{name}", *sum((["--add-topic", t] for t in spec["topics"]), []))
        run("git", "push", "-q", "-u", "origin", "main", cwd=dest)
        print(f"== {name}: pushed https://github.com/{OWNER}/{name}")


if __name__ == "__main__":
    main()
