"""Asking a model for the next decision.

The models are reached through AgentRouter, which only serves its supported clients, so the client is the
Codex CLI (`codex exec`), configured in ~/.config/nervus/codex/config.toml to use AgentRouter's Responses
endpoint. Each call is a fresh, ephemeral Codex session: nothing is carried from one decision to the next
except what the caller puts in the prompt, and nothing at all from one run to the next.

    Codex(model).ask(prompt, {"overhead": jpeg, "wrist": jpeg}, schema) -> (answer dict, usage dict)

`Scripted` has the same `ask` and answers from a Python function: the whole pipeline can be driven on the
real arm, or tested without one, without spending a token.
"""
from __future__ import annotations

import json
import os
import re
import shutil
import subprocess
import tempfile
import time
from pathlib import Path

MODELS = [
    {"id": "gpt-6-astra", "label": "GPT-6 Astra"},
    {"id": "gpt-6.1-sol", "label": "GPT-6.1 Sol"},
    {"id": "gpt-5.6-sol", "label": "GPT-5.6 Sol"},
]
CODEX_HOME = Path(os.environ.get("GPTARM_CODEX_HOME", "~/.config/nervus/codex")).expanduser()
KEY_FILE = Path(os.environ.get("GPTARM_KEY_FILE", "~/.config/nervus/openai.key")).expanduser()
ROUTE = "Codex CLI -> AgentRouter"

# The session is told this instead of Codex's own long coding-agent instructions: it has one job here.
SYSTEM = ("You are the decision-maker for a robot arm. You are given a task, the arm's state, and camera images. "
          "Answer only with the JSON object asked for. Never run commands, read files or use tools: everything "
          "you need is in the message and the images.")


class LLMError(Exception):
    """The model could not be asked. `fatal`: asking again will not help (no budget, bad key, bad model)."""

    def __init__(self, msg, fatal=False):
        super().__init__(msg)
        self.fatal = fatal


def _codex_bin():
    for c in (shutil.which("codex"), str(Path.home() / ".npm-global/bin/codex")):
        if c and Path(c).exists():
            return c
    raise LLMError("the Codex CLI is not installed (npm install -g @openai/codex)", fatal=True)


def _usage(stdout: str) -> dict:
    """Token counts from `codex exec --json` events (the last event that carries a usage object)."""
    best = {}
    for line in stdout.splitlines():
        line = line.strip()
        if not line.startswith("{"):
            continue
        try:
            ev = json.loads(line)
        except ValueError:
            continue
        stack = [ev]
        while stack:
            o = stack.pop()
            if isinstance(o, dict):
                if "input_tokens" in o and "output_tokens" in o:
                    best = o
                stack.extend(o.values())
            elif isinstance(o, list):
                stack.extend(o)
    if not best:
        return {}
    cached = int(best.get("cached_input_tokens") or 0)
    return {"input_tokens": int(best.get("input_tokens") or 0), "cached_input_tokens": cached,
            "output_tokens": int(best.get("output_tokens") or 0),
            "reasoning_tokens": int(best.get("reasoning_output_tokens") or 0)}


def _error_text(out: str) -> str:
    for pat in (r'"message"\s*:\s*"([^"]{8,300})"', r"(?im)^.*\b(error|unauthori[sz]ed|quota|exhausted)\b.*$"):
        m = re.search(pat, out)
        if m:
            return (m.group(1) if m.lastindex and pat.startswith('"') else m.group(0)).strip()[:300]
    return out.strip()[-300:]


class Codex:
    def __init__(self, model, effort="medium", timeout_s=180.0, retries=2):
        self.model, self.effort, self.timeout_s, self.retries = model, effort, float(timeout_s), int(retries)
        self.calls, self.seconds, self.tokens = 0, 0.0, 0

    def ask(self, prompt: str, images: dict, schema: dict):
        """-> (answer, {"seconds", "tokens", "input_tokens", "output_tokens", ...}). Raises LLMError."""
        if not KEY_FILE.exists():
            raise LLMError(f"no API key at {KEY_FILE}", fatal=True)
        last = None
        for attempt in range(self.retries + 1):
            try:
                return self._once(prompt, images, schema)
            except LLMError as e:
                last = e
                if e.fatal:
                    raise
                time.sleep(3.0 * (attempt + 1))
        raise last

    def _once(self, prompt, images, schema):
        with tempfile.TemporaryDirectory(prefix="gptarm-llm-") as d:
            d = Path(d)
            (d / "schema.json").write_text(json.dumps(schema))
            (d / "system.md").write_text(SYSTEM)
            cmd = [_codex_bin(), "exec", "--skip-git-repo-check", "--ephemeral", "-s", "read-only", "--json",
                   "-m", self.model, "-c", f'model_reasoning_effort="{self.effort}"',
                   "-c", f'model_instructions_file="{d / "system.md"}"',
                   "--output-schema", str(d / "schema.json"), "-o", str(d / "out.json")]
            for name, data in images.items():
                (d / f"{name}.jpg").write_bytes(data)
                cmd += ["-i", str(d / f"{name}.jpg")]
            cmd.append("-")                       # the prompt comes on stdin (-i would swallow a positional one)
            env = dict(os.environ, CODEX_HOME=str(CODEX_HOME), CODEX_GATEWAY_API_KEY=KEY_FILE.read_text().strip(),
                       PATH=f"{Path.home() / '.npm-global/bin'}:{os.environ.get('PATH', '')}")
            t0 = time.time()
            try:
                r = subprocess.run(cmd, input=prompt, capture_output=True, text=True, timeout=self.timeout_s, cwd=d, env=env)
            except subprocess.TimeoutExpired:
                raise LLMError(f"{self.model} did not answer within {self.timeout_s:.0f} s") from None
            dt = time.time() - t0
            out = (r.stdout or "") + "\n" + (r.stderr or "")
            try:
                answer = json.loads((d / "out.json").read_text())
            except (OSError, ValueError):
                msg = _error_text(out)
                fatal = bool(re.search(r"402|quota|exhausted|unauthori[sz]ed|invalid.*key|model.*not.*(found|exist)", out, re.I))
                raise LLMError(f"{self.model}: {msg}", fatal=fatal) from None
        u = _usage(r.stdout or "")
        u["tokens"] = u.get("input_tokens", 0) + u.get("output_tokens", 0)
        u["seconds"] = round(dt, 2)
        self.calls += 1
        self.seconds += dt
        self.tokens += u["tokens"]
        return answer, u


class Scripted:
    """Stands in for a model: `policy(prompt, images, schema) -> answer`. No network, no tokens."""

    def __init__(self, policy, model="scripted", delay_s=0.0):
        self.policy, self.model, self.delay_s = policy, model, delay_s
        self.calls, self.seconds, self.tokens = 0, 0.0, 0

    def ask(self, prompt, images, schema):
        time.sleep(self.delay_s)
        self.calls += 1
        return self.policy(prompt, images, schema), {"seconds": self.delay_s, "tokens": 0}


def client(model, **kw):
    if model not in [m["id"] for m in MODELS]:
        raise LLMError(f"unknown model {model!r}", fatal=True)
    return Codex(model, **kw)


if __name__ == "__main__":       # python -m gptarm.llm gpt-5.6-sol [image.jpg]: one small question, to check the route
    import sys
    schema = {"type": "object", "additionalProperties": False, "required": ["answer"],
              "properties": {"answer": {"type": "string"}}}
    imgs = {"image": Path(sys.argv[2]).read_bytes()} if len(sys.argv) > 2 else {}
    q = "Describe the image in one short sentence." if imgs else "What is 17 + 25? Answer with the number."
    try:
        print(Codex(sys.argv[1], retries=0).ask(q, imgs, schema))
    except LLMError as e:
        print("failed:", e, "(fatal)" if e.fatal else "")
