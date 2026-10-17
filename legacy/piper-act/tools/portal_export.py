#!/usr/bin/env python3
"""Export a supervised lerobot-train run to one JSON document for the training portal, and push it.

    python tools/portal_export.py --config tools/run_act_piper_pick_place_v1.json --once     # build + push once
    python tools/portal_export.py --config tools/run_act_piper_pick_place_v1.json --daemon   # every 60 s

Only derived numbers leave this machine: no file paths, host names or supervisor internals.
"""
import argparse, ast, json, os, re, shutil, subprocess, sys, time
from datetime import datetime, timezone
from pathlib import Path

VLA = Path(__file__).resolve().parent.parent
# SSH target of the dashboard server, e.g. root@203.0.113.7. Set in the environment (the launchd plist does this),
# so the address never lives in the source.
REMOTE = os.environ.get("PIPER_PORTAL_REMOTE", "")
REMOTE_PATH = "/opt/act-portal/www/data/metrics.json"
SSH_OPTS = ["-o", "BatchMode=yes", "-o", "ConnectTimeout=20", "-o", "ControlMaster=auto",
            "-o", "ControlPath=/tmp/act-portal-%r@%h:%p", "-o", "ControlPersist=10m"]

BANNER = re.compile(r"===== supervisor launch #(\d+) (\S+ \S+) UTC — (.*?) =====")
RESUME = re.compile(r"resume from (\d+) \(step (\d+)\)")
METRIC = re.compile(
    r"INFO (\d{4}-\d\d-\d\d \d\d:\d\d:\d\d) \S+ step:(\S+) smpl:(\S+) ep:(\S+) epch:([0-9.]+) "
    r"loss:(\S+) grdn:(\S+) lr:(\S+) updt_s:([0-9.]+) data_s:([0-9.]+)")
CKPT_LOG = re.compile(r"INFO (\d{4}-\d\d-\d\d \d\d:\d\d:\d\d) \S+ Checkpoint policy after step (\d+)")
START = re.compile(r"INFO (\d{4}-\d\d-\d\d \d\d:\d\d:\d\d) \S+ Start offline training")


def local_to_epoch(s):
    # lerobot logs in the Mac's local time
    return time.mktime(time.strptime(s, "%Y-%m-%d %H:%M:%S"))


def big(s):
    m = re.fullmatch(r"([0-9.]+)([KMB]?)", s)
    return float(m.group(1)) * {"": 1, "K": 1e3, "M": 1e6, "B": 1e9}[m.group(2)] if m else None


def num(s):
    try:
        v = float(s)
        return v if v == v and abs(v) != float("inf") else None
    except ValueError:
        return None


def dir_size(p):
    return sum(f.stat().st_size for f in p.rglob("*") if f.is_file())


class Exporter:
    def __init__(self, cfg, run_name=None, dataset_override=None):
        self.cfg = cfg
        self.run_name = run_name
        self.dataset_override = dataset_override
        out = Path(cfg["output_dir"])
        self.out = out if out.is_absolute() else VLA / out
        self.name = self.out.name
        self.logdir = self.out.parent
        self.log = self.logdir / f"{self.name}.log"
        self.status = self.logdir / f"{self.name}.status.json"
        self.sys_series = self.logdir / f"{self.name}.sysmetrics.jsonl"
        self.log_freq = int(next((a.split("=", 1)[1] for a in cfg["args"] if a.startswith("--log_freq=")), 200))
        root = next(a.split("=", 1)[1] for a in cfg["args"] if a.startswith("--dataset.root="))
        self.dataset_root = Path(root)

    # ---- log ----
    def parse_log(self):
        text = self.log.read_text(errors="replace").replace("\r", "\n") if self.log.exists() else ""
        rows, launches, ckpt_times, starts = {}, [], {}, []
        base, k = 0, 0
        for line in text.split("\n"):
            b = BANNER.search(line)
            if b:
                r = RESUME.search(b.group(3))
                base = int(r.group(2)) if r else 0
                k = 0
                launches.append({"n": int(b.group(1)), "utc": b.group(2) + " UTC",
                                 "mode": "resumed" if r else "started", "from_step": base})
                continue
            m = START.search(line)
            if m:
                starts.append(local_to_epoch(m.group(1)))
                continue
            c = CKPT_LOG.search(line)
            if c:
                ckpt_times[int(c.group(2))] = local_to_epoch(c.group(1))
                continue
            m = METRIC.search(line)
            if not m:
                continue
            k += 1
            step = base + k * self.log_freq
            shown = big(m.group(2))
            if shown is not None and abs(shown - step) > 600:  # formatter rounds to 1K; anything worse = misaligned
                step = int(round(shown))
            rows[step] = {  # a resumed segment overwrites the redone steps
                "step": step, "t": local_to_epoch(m.group(1)), "epoch": float(m.group(5)),
                "loss": num(m.group(6)), "grad_norm": num(m.group(7)),
                "lr": num(m.group(8)), "update_s": float(m.group(9)), "data_s": float(m.group(10)),
            }
        series = [rows[s] for s in sorted(rows)]
        # wall-clock throughput between consecutive log lines of the same launch (includes checkpoint I/O)
        for a, b in zip(series, series[1:]):
            dt = b["t"] - a["t"]
            b["wall_steps_per_s"] = round((b["step"] - a["step"]) / dt, 3) if 0 < dt < 3600 and b["step"] > a["step"] else None
        if series:
            series[0]["wall_steps_per_s"] = None
        for r in series:
            per = r["update_s"] + r["data_s"]
            r["compute_steps_per_s"] = round(1 / per, 3) if per > 0 else None
        return series, launches, ckpt_times, text

    def run_config(self, text):
        ck = self.checkpoints_dirs()
        if ck:
            try:
                return json.loads((ck[-1] / "pretrained_model/train_config.json").read_text())
            except Exception:
                pass
        m = re.search(r"ot_train\.py:\d+ (\{'batch_size'.*?)\nINFO ", text, re.S)
        if m:
            dump = re.sub(r"<[\w.]+: ('[^']*'|-?\d+)>", r"\1", m.group(1))  # enum reprs -> their values
            dump = re.sub(r"\b\w+\((?:[^()]|\([^()]*\))*\)", "None", dump)       # dataclass reprs -> None
            try:
                return ast.literal_eval(dump)
            except Exception:
                pass
        return {}

    def checkpoints_dirs(self):
        d = self.out / "checkpoints"
        if not d.is_dir():
            return []
        return sorted(p for p in d.iterdir() if p.is_dir() and not p.is_symlink() and p.name.isdigit()
                      and (p / "training_state/optimizer_param_groups.json").is_file())

    def sample_system(self, st):
        pid = st.get("pid")
        rss = cpu = None
        if pid:
            try:
                o = subprocess.run(["ps", "-o", "rss=,%cpu=", "-p", str(pid)], capture_output=True, text=True, timeout=5).stdout.split()
                rss, cpu = round(int(o[0]) / 1024, 1), float(o[1])
            except Exception:
                pass
        row = {"t": time.time(), "mem_free_pct": st.get("mem_free_pct"),
               "disk_free_gb": round(shutil.disk_usage(self.logdir).free / 1e9, 2), "trainer_rss_mb": rss, "trainer_cpu_pct": cpu}
        with open(self.sys_series, "a") as f:
            f.write(json.dumps(row) + "\n")

    def _dataset(self):
        if self.dataset_override is not None:
            o = self.dataset_override
            fps = o.get("fps", 15)
            return {"robot": "AgileX PiPER · 6-DoF + gripper", "episodes": o.get("episodes"),
                    "frames": o.get("frames"), "fps": fps, "camera": "Wrist RGB 640×480",
                    "state_dim": 7, "action_dim": 7, "collection": o.get("collection"),
                    "task": o.get("task"), "hours": round(o["frames"] / fps / 3600, 2) if o.get("frames") else None}
        try:
            info = json.loads((self.dataset_root / "meta/info.json").read_text())
            tasks = []
            try:
                import pandas as pd
                tasks = list(pd.read_parquet(self.dataset_root / "meta/tasks.parquet").index)
            except Exception:
                pass
            return {"robot": "AgileX PiPER · 6-DoF + gripper", "episodes": info["total_episodes"], "frames": info["total_frames"],
                    "fps": info["fps"], "camera": "Wrist RGB 640×480", "state_dim": 7, "action_dim": 7,
                    "collection": "Kinesthetic demonstration (drag-teach)", "task": tasks[0] if tasks else None,
                    "hours": round(info["total_frames"] / info["fps"] / 3600, 2)}
        except Exception:
            return {"robot": "AgileX PiPER · 6-DoF + gripper", "episodes": None, "frames": None, "fps": 15,
                    "camera": "Wrist RGB 640×480", "state_dim": 7, "action_dim": 7, "collection": None, "task": None, "hours": None}

    def build(self):
        series, launches, ckpt_times, text = self.parse_log()
        starts = [local_to_epoch(m.group(1)) for m in START.finditer(text)]
        st = json.loads(self.status.read_text()) if self.status.exists() else {}
        self.sample_system(st)
        sysrows = [json.loads(l) for l in self.sys_series.read_text().splitlines() if l.strip()] if self.sys_series.exists() else []
        rc = self.run_config(text)
        pol = rc.get("policy", {})
        target = int(self.cfg["steps"])
        bs = rc.get("batch_size") or 8
        for r in series:
            r["samples"] = r["step"] * bs  # the log rounds smpl to 1K; step x batch is exact
        pm = re.search(r"num_learnable_params=(\d+)", text)
        loss_by_step = {r["step"]: r["loss"] for r in series}
        ckpts = []
        for p in self.checkpoints_dirs():
            s = int(p.name)
            near = min(loss_by_step, key=lambda x: abs(x - s)) if loss_by_step else None
            pub = Path(self.cfg.get("publish_dir", "")) / f"{self.cfg.get('publish_prefix', '')}_{p.name}"
            ckpts.append({"step": s, "t": ckpt_times.get(s, p.stat().st_mtime),
                          "loss": loss_by_step.get(near), "model_mb": round(dir_size(p / "pretrained_model") / 1e6, 1),
                          "state_mb": round(dir_size(p / "training_state") / 1e6, 1) if (p / "training_state").is_dir() else 0,
                          "published": (pub / "model.safetensors").is_file()})

        last = series[-1] if series else {}
        cur_step = max(st.get("step") or 0, last.get("step", 0))
        recent = [r["compute_steps_per_s"] for r in series[-10:] if r.get("compute_steps_per_s")]
        rate = sum(recent) / len(recent) if recent else None
        state = st.get("state", "unknown")
        state = "training" if state.startswith("restarting") else state
        doc = {
            "generated_utc": datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ"),
            "run": {"name": self.run_name or self.name, "state": state, "target_steps": target, "step": cur_step,
                    "started_t": starts[0] if starts else (series[0]["t"] if series else None), "updated_t": time.time(),
                    "steps_per_s": round(rate, 3) if rate else None,
                    "eta_s": round((target - cur_step) / rate) if rate and state == "training" else None,
                    "restarts": st.get("restarts", 0), "launches": launches},
            "model": {"type": pol.get("type", "act"), "params": int(pm.group(1)) if pm else None,
                      "chunk_size": pol.get("chunk_size"), "n_action_steps": pol.get("n_action_steps"),
                      "vision_backbone": pol.get("vision_backbone"), "pretrained_backbone_weights": pol.get("pretrained_backbone_weights"),
                      "dim_model": pol.get("dim_model"), "n_heads": pol.get("n_heads"), "dim_feedforward": pol.get("dim_feedforward"),
                      "n_encoder_layers": pol.get("n_encoder_layers"), "n_decoder_layers": pol.get("n_decoder_layers"),
                      "use_vae": pol.get("use_vae"), "latent_dim": pol.get("latent_dim"), "kl_weight": pol.get("kl_weight"),
                      "dropout": pol.get("dropout"), "optimizer_lr": pol.get("optimizer_lr"),
                      "optimizer_lr_backbone": pol.get("optimizer_lr_backbone"), "optimizer_weight_decay": pol.get("optimizer_weight_decay"),
                      "device": "Apple M2 Pro · Metal (MPS)"},
            "training": {"batch_size": rc.get("batch_size"), "steps": target, "save_freq": rc.get("save_freq"),
                         "log_freq": rc.get("log_freq"), "seed": rc.get("seed"), "num_workers": rc.get("num_workers"),
                         "image_augmentation": (rc.get("dataset", {}).get("image_transforms", {}) or {}).get("enable"),
                         "optimizer": (rc.get("optimizer") or {}).get("type"),
                         "grad_clip_norm": (rc.get("optimizer") or {}).get("grad_clip_norm"),
                         "framework": "LeRobot 0.4.4 · PyTorch 2.10"},
            "dataset": self._dataset(),
            "series": series,
            "checkpoints": ckpts,
            "system": [{k: r[k] for k in ("t", "mem_free_pct", "disk_free_gb", "trainer_rss_mb", "trainer_cpu_pct")} for r in sysrows[-2000:]],
        }
        return doc

    def push(self, doc):
        if not REMOTE:
            raise RuntimeError("PIPER_PORTAL_REMOTE is not set (e.g. export PIPER_PORTAL_REMOTE=root@<server>)")
        data = json.dumps(doc, separators=(",", ":")).encode()
        cmd = ["ssh", *SSH_OPTS, REMOTE, f"cat > {REMOTE_PATH}.tmp && mv {REMOTE_PATH}.tmp {REMOTE_PATH}"]
        r = subprocess.run(cmd, input=data, capture_output=True, timeout=60)
        if r.returncode != 0:
            raise RuntimeError(r.stderr.decode()[-500:])
        return len(data)


def daemonize(logfile):
    if os.fork() > 0:
        os._exit(0)
    os.setsid()
    if os.fork() > 0:
        os._exit(0)
    fd = os.open(logfile, os.O_WRONLY | os.O_CREAT | os.O_APPEND, 0o644)
    os.dup2(fd, 1); os.dup2(fd, 2)
    os.dup2(os.open(os.devnull, os.O_RDONLY), 0)


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--config", required=True)
    ap.add_argument("--once", action="store_true")
    ap.add_argument("--daemon", action="store_true")
    ap.add_argument("--dump", help="write JSON locally instead of pushing")
    ap.add_argument("--interval", type=int, default=60)
    a = ap.parse_args()
    ex = Exporter(json.loads(Path(a.config).read_text()))
    if a.dump:
        Path(a.dump).write_text(json.dumps(ex.build(), indent=1))
        return
    if a.once:
        print("pushed", ex.push(ex.build()), "bytes")
        return
    if a.daemon:
        daemonize(str(ex.logdir / f"{ex.name}.portal.log"))
        (ex.logdir / f"{ex.name}.portal.pid").write_text(str(os.getpid()))
    finished_at = None
    while True:
        try:
            doc = ex.build()
            n = ex.push(doc)
            print(f"[{datetime.now(timezone.utc):%H:%M:%S}] pushed {n} bytes step={doc['run']['step']} state={doc['run']['state']}", flush=True)
            if doc["run"]["state"] in ("finished", "stopped_by_user") or doc["run"]["state"].startswith(("fatal", "gave_up")):
                finished_at = finished_at or time.time()
                if time.time() - finished_at > 1800:
                    print("run ended; final state pushed, exiting", flush=True)
                    return
        except Exception as e:
            print(f"[{datetime.now(timezone.utc):%H:%M:%S}] export/push failed: {e}", flush=True)
        time.sleep(a.interval)


if __name__ == "__main__":
    main()
