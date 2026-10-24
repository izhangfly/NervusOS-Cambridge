"""The model: one OpenAI-compatible Chat Completions call with tools, for any provider in config.PROVIDERS.

The client only talks; it never moves anything. agent.py decides what each tool call does.
"""
from __future__ import annotations

import json
import urllib.error
import urllib.request

from .config import PROVIDERS


class LLMError(Exception):
    pass


class ChatClient:
    """POST {base_url}/chat/completions. Returns the assistant message dict (role, content, tool_calls)."""

    def __init__(self, provider: str, key: str, model: str, base_url: str, timeout_s: float = 60.0):
        self.provider, self.key, self.model = provider, key, model
        self.base_url, self.timeout_s = base_url.rstrip("/"), timeout_s

    def chat(self, messages: list, tools: list) -> dict:
        body = {"model": self.model, "messages": messages, "tools": tools, "tool_choice": "auto",
                "temperature": 0.2, **PROVIDERS[self.provider]["extra"]}
        req = urllib.request.Request(
            self.base_url + "/chat/completions", json.dumps(body).encode(),
            {"Content-Type": "application/json", "Authorization": f"Bearer {self.key}"})
        try:
            with urllib.request.urlopen(req, timeout=self.timeout_s) as r:
                data = json.loads(r.read())
        except urllib.error.HTTPError as e:
            detail = e.read()[:400].decode("utf-8", "replace")
            # Never echo the request (it holds the key in a header, and images); the status and reply are enough.
            raise LLMError(f"{PROVIDERS[self.provider]['label']} answered HTTP {e.code}: {detail}") from None
        except (urllib.error.URLError, TimeoutError, OSError) as e:
            raise LLMError(f"{PROVIDERS[self.provider]['label']} unreachable: {getattr(e, 'reason', e)}") from None
        try:
            msg = data["choices"][0]["message"]
        except (KeyError, IndexError, TypeError):
            raise LLMError(f"unexpected reply: {str(data)[:300]}") from None
        out = {"role": "assistant", "content": msg.get("content") or ""}
        if msg.get("tool_calls"):
            out["tool_calls"] = [{"id": c["id"], "type": "function",
                                  "function": {"name": c["function"]["name"],
                                               "arguments": c["function"].get("arguments") or "{}"}}
                                 for c in msg["tool_calls"]]
        return out


class ScriptedLLM:
    """A stand-in model for tests and the simulator: replies from a list, one per call.

    Each item is either a string (a final text reply) or a list of (tool_name, args) calls."""

    def __init__(self, script: list):
        self.script, self.calls, self.n = list(script), [], 0

    def chat(self, messages: list, tools: list) -> dict:
        self.calls.append(messages)
        if not self.script:
            return {"role": "assistant", "content": "Done."}
        step = self.script.pop(0)
        if isinstance(step, str):
            return {"role": "assistant", "content": step}
        calls = []
        for name, args in step:
            self.n += 1
            calls.append({"id": f"call_{self.n}", "type": "function",
                          "function": {"name": name, "arguments": json.dumps(args)}})
        return {"role": "assistant", "content": "", "tool_calls": calls}
