#!/usr/bin/env python3
"""Mac post-training control (AUTOMATED POST-TRAINING.md §5).

    posttrain_ctl.py status              STATUS.json, rounds, candidate, ledger summary
    posttrain_ctl.py tick                one watcher iteration (scan, convert, promote, trigger/advance a round)
    posttrain_ctl.py daemon              loop every 5 minutes (what the launchd agent runs)
    posttrain_ctl.py round-now           start a round at the next tick, ignoring the 6-hour spacing
    posttrain_ctl.py retry-errors        forget Mac-side conversion errors so the next tick retries those packages
    posttrain_ctl.py install-agent       install + load ~/Library/LaunchAgents/com.nervus.piper-posttrain.plist
    posttrain_ctl.py uninstall-agent
"""
import argparse
import json
import os
import subprocess
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
from posttrain import config as C  # noqa: E402

LABEL = "com.nervus.piper-posttrain"
PLIST = Path.home() / "Library" / "LaunchAgents" / f"{LABEL}.plist"


def cmd_status(_):
    from posttrain import pipeline as PL
    print(json.dumps(C.read_json(C.MAC_SHARED / "STATUS.json", {}), indent=2))
    for r in PL.round_records():
        print(f"round {r['round']}: {r['phase']}  parent={r['parent']}  published={r.get('published')}  reason={r['reason']}")
    print("candidate:", json.dumps(C.read_json(PL.CANDIDATE), indent=2) if PL.CANDIDATE.exists() else None)
    lg = PL.ledger()
    from collections import Counter
    print("ledger runs:", dict(Counter(v.get("status") for v in lg["runs"].values())),
          "accepted episodes:", len(PL.accepted_episodes(lg)))
    print("CURRENT:", C.read_json(C.POLICIES / "CURRENT.json"))


def cmd_tick(_):
    from posttrain import pipeline as PL
    PL.tick()


def cmd_daemon(a):
    from posttrain import pipeline as PL
    PL.daemon(a.interval)


def cmd_retry_errors(_):
    from posttrain import pipeline as PL
    lg = PL.ledger()
    n = [k for k, v in lg["runs"].items() if v.get("status") in ("error", "pending")]
    for k in n:
        del lg["runs"][k]
    PL.save_ledger(lg)
    print("cleared:", n)


def cmd_round_now(_):
    C.LOCAL.mkdir(parents=True, exist_ok=True)
    (C.LOCAL / "START_ROUND").write_text(C.utc_now())
    print("round will start at the next tick (if no round is running)")


def cmd_install_agent(a):
    C.LOCAL.mkdir(parents=True, exist_ok=True)
    plist = f"""<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
  <key>Label</key><string>{LABEL}</string>
  <key>ProgramArguments</key><array>
    <string>{C.VENV_PY}</string><string>{Path(__file__).resolve()}</string><string>daemon</string>
    <string>--interval</string><string>{a.interval}</string>
  </array>
  <key>WorkingDirectory</key><string>{C.VLA}</string>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>ThrottleInterval</key><integer>60</integer>
  <key>EnvironmentVariables</key><dict><key>PYTHONUNBUFFERED</key><string>1</string></dict>
  <key>StandardOutPath</key><string>{C.LOCAL / 'agent.out.log'}</string>
  <key>StandardErrorPath</key><string>{C.LOCAL / 'agent.err.log'}</string>
</dict></plist>
"""
    PLIST.parent.mkdir(parents=True, exist_ok=True)
    subprocess.run(["launchctl", "bootout", f"gui/{os.getuid()}/{LABEL}"], capture_output=True)
    PLIST.write_text(plist)
    r = subprocess.run(["launchctl", "bootstrap", f"gui/{os.getuid()}", str(PLIST)], capture_output=True, text=True)
    print("bootstrap:", r.returncode, r.stderr.strip())
    print(subprocess.run(["launchctl", "print", f"gui/{os.getuid()}/{LABEL}"], capture_output=True, text=True).stdout[:400])


def cmd_uninstall_agent(_):
    subprocess.run(["launchctl", "bootout", f"gui/{os.getuid()}/{LABEL}"])
    PLIST.unlink(missing_ok=True)


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    sub = ap.add_subparsers(dest="cmd", required=True)
    sub.add_parser("status").set_defaults(fn=cmd_status)
    sub.add_parser("tick").set_defaults(fn=cmd_tick)
    d = sub.add_parser("daemon"); d.add_argument("--interval", type=int, default=300); d.set_defaults(fn=cmd_daemon)
    sub.add_parser("round-now").set_defaults(fn=cmd_round_now)
    sub.add_parser("retry-errors").set_defaults(fn=cmd_retry_errors)
    i = sub.add_parser("install-agent"); i.add_argument("--interval", type=int, default=300); i.set_defaults(fn=cmd_install_agent)
    sub.add_parser("uninstall-agent").set_defaults(fn=cmd_uninstall_agent)
    a = ap.parse_args()
    a.fn(a)


if __name__ == "__main__":
    main()
