"""The models. Each one answers a prompt plus camera images with one JSON object that fits a schema.

    OpenAI (GPT-6 Astra): the Responses API, a strict JSON schema and a reasoning effort, the way
        github.com/RobotKitAI/piper-astra-jev calls it (deciders.Astra).
    DeepSeek: Chat Completions in JSON mode, thinking switched off; the schema is spelled out in the prompt.

They only talk. agent.py decides what an answer is allowed to do.
"""
from __future__ import annotations

import base64
import json
import urllib.error
import urllib.request

from .config import PROVIDERS


class LLMError(Exception):
    pass


def _post(url: str, key: str, body: dict, timeout: float, label: str) -> dict:
    req = urllib.request.Request(url, json.dumps(body).encode(),
                                 {"Content-Type": "application/json", "Authorization": f"Bearer {key}"})
    try:
        with urllib.request.urlopen(req, timeout=timeout) as r:
            return json.loads(r.read())
    except urllib.error.HTTPError as e:
        detail = e.read()[:400].decode("utf-8", "replace")
        # Never echo the request: it carries the key in a header, and the images.
        raise LLMError(f"{label} answered HTTP {e.code}: {detail}") from None
    except (urllib.error.URLError, TimeoutError, OSError) as e:
        raise LLMError(f"{label} unreachable: {getattr(e, 'reason', e)}") from None


def _b64(jpg: bytes) -> str:
    return base64.b64encode(jpg).decode()


class OpenAIResponses:
    """POST {base_url}/responses with a strict json_schema."""

    def __init__(self, key: str, model: str, base_url: str, effort: str = "medium", timeout_s: float = 120.0):
        self.key, self.model, self.base_url = key, model, base_url.rstrip("/")
        self.effort, self.timeout_s = effort, timeout_s
        self.label = PROVIDERS["openai"]["label"]

    def ask(self, text: str, images: dict[str, bytes], schema: dict, name: str) -> dict:
        content = [{"type": "input_text", "text": text}]
        for cam, jpg in images.items():
            content += [{"type": "input_text", "text": f"camera '{cam}':"},
                        {"type": "input_image", "image_url": f"data:image/jpeg;base64,{_b64(jpg)}"}]
        body = {"model": self.model, "input": [{"role": "user", "content": content}],
                "text": {"format": {"type": "json_schema", "name": name, "schema": schema, "strict": True}}}
        if self.effort != "none":
            body["reasoning"] = {"effort": self.effort}
        data = _post(self.base_url + "/responses", self.key, body, self.timeout_s, self.label)
        out = "".join(c.get("text", "") for item in data.get("output", []) if item.get("type") == "message"
                      for c in item.get("content", []))
        try:
            return json.loads(out)
        except ValueError:
            raise LLMError(f"{self.label} did not answer in JSON: {out[:200]!r}") from None


class DeepSeekChat:
    """POST {base_url}/chat/completions in JSON mode."""

    def __init__(self, key: str, model: str, base_url: str, timeout_s: float = 60.0):
        self.key, self.model, self.base_url, self.timeout_s = key, model, base_url.rstrip("/"), timeout_s
        self.label = PROVIDERS["deepseek"]["label"]

    def ask(self, text: str, images: dict[str, bytes], schema: dict, name: str) -> dict:
        # JSON mode needs the word "json" in the prompt, and doesn't enforce a schema: describe it.
        rules = (f"Answer with one json object for '{name}' and nothing else. Its json schema:\n"
                 + json.dumps(schema))
        content = [{"type": "text", "text": text + "\n\n" + rules}]
        for cam, jpg in images.items():
            content += [{"type": "text", "text": f"camera '{cam}':"},
                        {"type": "image_url", "image_url": {"url": f"data:image/jpeg;base64,{_b64(jpg)}",
                                                            "detail": "low"}}]
        body = {"model": self.model, "messages": [{"role": "user", "content": content}],
                "response_format": {"type": "json_object"}, "temperature": 0.2,
                **PROVIDERS["deepseek"]["extra"]}
        data = _post(self.base_url + "/chat/completions", self.key, body, self.timeout_s, self.label)
        try:
            out = data["choices"][0]["message"]["content"] or ""
            return json.loads(out)
        except (KeyError, IndexError, TypeError, ValueError):
            raise LLMError(f"{self.label} did not answer in JSON: {str(data)[:200]}") from None


class Scripted:
    """A stand-in model for tests and the simulator: answers from a list, one per call."""

    def __init__(self, answers: list[dict]):
        self.answers, self.prompts = list(answers), []

    def ask(self, text, images, schema, name) -> dict:
        self.prompts.append((name, text))
        if not self.answers:
            return {"skill": "done", "note": "Nothing left in the script."} if name == "decision" else {}
        return self.answers.pop(0)
