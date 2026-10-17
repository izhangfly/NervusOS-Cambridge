import json
from pathlib import Path

pt = Path(r"C:\Users\zhy10\Shared\Piper Arm\posttrain\rollouts")
runs = Path(r"C:\Users\zhy10\Shared\Piper Arm\deploy\runs")

print("=== rollout packages (what the Mac scans) ===")
for d in sorted(pt.iterdir()) if pt.exists() else []:
    if not d.is_dir():
        continue
    ready = (d / "READY").exists()
    meta = {}
    try:
        meta = json.loads((d / "meta.json").read_text(encoding="utf-8"))
    except Exception as e:
        meta = {"err": str(e)}
    n_human = 0
    try:
        n_human = sum(1 for l in (d / "episode.jsonl").read_text(encoding="utf-8").splitlines()
                      if l.strip() and json.loads(l).get("segment") == "human")
    except Exception:
        pass
    print(f"{d.name}: READY={ready} outcome={meta.get('outcome')} mock={meta.get('mock')} "
          f"interventions={meta.get('interventions')} stopped={meta.get('stop_reason')} "
          f"frames={meta.get('frames')} human_rows={n_human}")

print("\n=== run logs: segment transitions ===")
for d in sorted(p for p in runs.iterdir() if p.is_dir()):
    try:
        rows = [json.loads(l) for l in (d / "steps.jsonl").read_text(encoding="utf-8").splitlines() if l.strip()]
    except Exception as e:
        print(f"{d.name}: unreadable ({e})")
        continue
    segs = [r.get("segment") for r in rows]
    human = sum(1 for s in segs if s == "human")
    trans = [(i, segs[i - 1], s) for i, s in enumerate(segs) if i and s != segs[i - 1]]
    stale = sum(1 for r in rows if r.get("grip") == "stale")
    print(f"{d.name}: {len(rows)} rows, human={human}, segment transitions={trans[:6]}, "
          f"stop in summary n/a, grip-stale rows={stale}")
