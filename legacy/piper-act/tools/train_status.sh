#!/bin/bash
# One-glance status of a supervised lerobot-train run.
#   tools/train_status.sh [run_name]      (default: act_piper_pick_place_v1)
RUN=${1:-act_piper_pick_place_v1}
D=~/NervusOS/robots/vla/outputs/train
S=$D/$RUN.status.json
[ -f "$S" ] || { echo "no status file $S"; exit 1; }
/usr/bin/python3 - "$S" <<'EOF'
import json, sys
s = json.load(open(sys.argv[1]))
m = s.get("metrics") or {}
print(f"{s['run']}  state={s['state']}  at {s['time']}")
print(f"step {s['step']}/{s['target']} ({s['pct']}%)  ETA {s['eta_hours']} h")
print(f"loss {m.get('loss')}  grdn {m.get('grdn')}  updt_s {m.get('updt_s')}  data_s {m.get('data_s')}")
print(f"checkpoints {s['checkpoints']}")
print(f"restarts {s['restarts']}  remedies {s['remedies']}")
for h in s.get("history", []): print("  restart:", h)
print(f"disk free {s['free_gb']} GB  mem free {s['mem_free_pct']}%  trainer pid {s['pid']}")
EOF
PIDF=$D/$RUN.supervisor.pid
if [ -f "$PIDF" ] && kill -0 "$(cat $PIDF)" 2>/dev/null; then echo "supervisor alive (pid $(cat $PIDF))"; else echo "supervisor NOT running"; fi
echo "--- last supervisor events ---"; tail -5 $D/$RUN.supervisor.log
echo "--- last loss lines ---"; tr '\r' '\n' < $D/$RUN.log | grep "loss:" | tail -3
