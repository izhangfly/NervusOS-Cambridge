#!/usr/bin/env python3
"""Translate the Chinese in files to English with a local model (Ollama), in place.

    scripts/translate.py PATH...                 files or folders, translated in place
    scripts/translate.py --dry-run PATH...       list what would be translated
    scripts/translate.py --check PATH...         exit 1 if any Chinese is left

This is how the team repositories imported into this one were translated: by a local model, not by hand and not
by an online service. Only text that contains Chinese is sent to the model, a chunk at a time:

* Markdown: whole blocks (a paragraph, a list, a table, a heading), so tables and lists keep their shape;
  fenced code blocks are translated as code.
* Code and config files: runs of consecutive lines that contain Chinese (comments, strings), with the code
  around them kept byte for byte; a run whose translation comes back with a different number of lines is
  retried one line at a time.
* File and folder names in Chinese are translated too, and Markdown links to them updated.

Every answer is checked: if Chinese is still in it, the chunk is retried once at temperature 0, and a chunk that
still fails is left as it was and reported. Results are cached (~/.cache/nervus-translate), so a rerun only
translates what changed.

Needs Ollama (https://ollama.com) with the model pulled: `ollama pull qwen3:8b`.
"""
from __future__ import annotations

import argparse
import hashlib
import json
import os
import re
import sys
import time
import urllib.request
from pathlib import Path

MODEL = os.environ.get("NERVUS_TRANSLATE_MODEL", "qwen3:8b")
OLLAMA = os.environ.get("OLLAMA_HOST", "http://127.0.0.1:11434")
CACHE = Path(os.environ.get("NERVUS_TRANSLATE_CACHE", "~/.cache/nervus-translate")).expanduser()
CHUNK = 1800  # characters of Markdown per request

# CJK punctuation, ideographs and full-width forms, from code points so this file stays ASCII.
CJK = re.compile("[%s]" % "".join(f"{chr(a)}-{chr(b)}" for a, b in (
    (0x3000, 0x303F), (0x3400, 0x4DBF), (0x4E00, 0x9FFF), (0xF900, 0xFAFF), (0xFF01, 0xFF5E))))
FULLWIDTH = {chr(0xFF0C): ", ", chr(0x3002): ". ", chr(0xFF1A): ": ", chr(0xFF1B): "; ", chr(0xFF08): " (",
             chr(0xFF09): ") ", chr(0xFF01): "! ", chr(0xFF1F): "? ", chr(0x3001): ", ", chr(0x300C): '"',
             chr(0x300D): '"', chr(0x300E): '"', chr(0x300F): '"', chr(0x3010): "[", chr(0x3011): "]",
             chr(0x300A): "*", chr(0x300B): "*", chr(0xFF5E): "~", chr(0x3000): " ", chr(0xFF0B): "+",
             chr(0xFF1D): "=", chr(0xFF0F): "/", chr(0xFF05): "%", chr(0xFF03): "#"}
TEXT_EXT = {".md", ".markdown", ".txt", ".go", ".py", ".kt", ".kts", ".java", ".sh", ".bash", ".yml", ".yaml",
            ".json", ".proto", ".ts", ".tsx", ".js", ".jsx", ".html", ".htm", ".css", ".c", ".h", ".cc", ".cpp",
            ".hpp", ".rs", ".toml", ".ini", ".cfg", ".conf", ".xml", ".gradle", ".properties", ".sql", ".mod",
            ".csv", ".dts", ".dtsi", ".service", ".rules", ".env", ".swift", ".m", ".mm", ".rst", ".tex", ""}

SYSTEM = ("You are a professional technical translator. Translate Chinese into clear, natural English. "
          "Rules: translate every Chinese word, leaving no Chinese characters or Chinese punctuation; "
          "keep everything that is not Chinese exactly as it is (code, identifiers, paths, URLs, numbers, "
          "Markdown syntax, table pipes, list markers, indentation, comment markers, line breaks); "
          "write plain, direct English, with no filler and no added explanations; "
          "output only the translation, nothing before or after it.")


def has_cjk(s: str) -> bool:
    return bool(CJK.search(s))


def normalise(s: str) -> str:
    """Full-width punctuation the model left behind -> ASCII (spacing tidied)."""
    for k, v in FULLWIDTH.items():
        s = s.replace(k, v)
    return re.sub(r"[ \t]+\n", "\n", re.sub(r"(?<=\S)  +(?=[\w(])", " ", s))


class Translator:
    def __init__(self, model: str = MODEL):
        self.model = model
        CACHE.mkdir(parents=True, exist_ok=True)
        self.cache_path = CACHE / f"{re.sub(r'[^a-z0-9.]+', '_', model.lower())}.jsonl"
        self.cache: dict[str, str] = {}
        if self.cache_path.exists():
            for line in self.cache_path.read_text(encoding="utf-8").splitlines():
                try:
                    d = json.loads(line)
                    self.cache[d["k"]] = d["v"]
                except Exception:
                    pass
        self.calls = 0
        self.seconds = 0.0
        self.failed: list[str] = []

    def _ask(self, prompt: str, temperature: float) -> str:
        body = {"model": self.model, "stream": False, "think": False,
                "options": {"temperature": temperature, "num_ctx": 8192, "num_predict": 4096},
                "messages": [{"role": "system", "content": SYSTEM}, {"role": "user", "content": prompt}]}
        req = urllib.request.Request(f"{OLLAMA}/api/chat", data=json.dumps(body).encode(),
                                     headers={"Content-Type": "application/json"})
        t = time.time()
        with urllib.request.urlopen(req, timeout=900) as r:
            out = json.loads(r.read())["message"]["content"]
        self.calls += 1
        self.seconds += time.time() - t
        out = re.sub(r"^\s*<think>.*?</think>\s*", "", out, flags=re.S)
        return out

    def translate(self, text: str, kind: str) -> str:
        """kind: 'markdown', 'code:<language>' or 'name'."""
        key = hashlib.sha256(f"{self.model}\0{kind}\0{text}".encode()).hexdigest()
        if key in self.cache:
            return self.cache[key]
        if kind == "name":
            prompt = ("Translate this file or folder name into a short English name. Use only ASCII letters, digits, "
                      "spaces, '-' and '_'. Keep any file extension. Output only the name.\n\n" + text)
        elif kind == "markdown":
            prompt = "Translate the Chinese in this Markdown into English, keeping the Markdown exactly:\n\n" + text
        else:
            lang = kind.split(":", 1)[1]
            n = text.count("\n") + 1
            prompt = (f"These are {n} consecutive lines from a {lang} file. Translate the Chinese in them (comments, "
                      f"strings, text) into English. Keep the code, indentation and comment markers exactly, and "
                      f"output exactly {n} lines, one for each input line:\n\n{text}")
        out = None
        for temperature in (0.2, 0.0):
            try:
                cand = self._ask(prompt, temperature)
            except Exception as e:
                print(f"  ! model call failed: {e}", file=sys.stderr)
                time.sleep(3)
                continue
            cand = normalise(cand.strip("\n") if kind != "markdown" else cand.strip())
            if kind.startswith("code:") and cand.count("\n") != text.count("\n"):
                continue  # line count changed: try again, then fall back to line by line
            if not has_cjk(cand):
                out = cand
                break
        if out is None:
            if kind.startswith("code:") and "\n" in text:
                out = "\n".join(self.translate(l, kind) if has_cjk(l) else l for l in text.split("\n"))
            else:
                self.failed.append(text[:80])
                return text
        self.cache[key] = out
        with open(self.cache_path, "a", encoding="utf-8") as f:
            f.write(json.dumps({"k": key, "v": out}, ensure_ascii=False) + "\n")
        return out


LANG = {".go": "Go", ".py": "Python", ".kt": "Kotlin", ".kts": "Kotlin", ".java": "Java", ".sh": "shell",
        ".bash": "shell", ".yml": "YAML", ".yaml": "YAML", ".json": "JSON", ".proto": "protobuf", ".js": "JavaScript",
        ".ts": "TypeScript", ".html": "HTML", ".css": "CSS", ".c": "C", ".h": "C", ".cpp": "C++", ".rs": "Rust",
        ".toml": "TOML", ".xml": "XML", ".sql": "SQL", ".swift": "Swift", ".dts": "device tree", ".txt": "text"}


def md_blocks(text: str) -> list[tuple[str, str]]:
    """[(kind, block)]: kind 'code' for fenced blocks (with their fences), 'text' otherwise; blank-line separated."""
    out, buf, fence = [], [], None
    for line in text.split("\n"):
        m = re.match(r"^\s*(`{3,}|~{3,})", line)
        if fence:
            buf.append(line)
            if m and m.group(1)[0] == fence[0] and len(m.group(1)) >= len(fence):
                out.append(("code", "\n".join(buf)))
                buf, fence = [], None
            continue
        if m:
            if buf:
                out.append(("text", "\n".join(buf)))
            buf, fence = [line], m.group(1)
            continue
        if line.strip() == "":
            if buf:
                out.append(("text", "\n".join(buf)))
                buf = []
            out.append(("blank", ""))
            continue
        buf.append(line)
    if buf:
        out.append(("code" if fence else "text", "\n".join(buf)))
    return out


def translate_markdown(tr: Translator, text: str) -> str:
    parts = md_blocks(text)
    out: list[str] = []
    pending: list[str] = []  # text blocks waiting to be sent together

    def flush():
        if pending:
            joined = "\n\n".join(pending)
            res = tr.translate(joined, "markdown") if has_cjk(joined) else joined
            out.append(res)
            pending.clear()

    for kind, block in parts:
        if kind == "blank":
            continue
        if kind == "code":
            flush()
            out.append(translate_code(tr, block, "code") if has_cjk(block) else block)
            continue
        if not has_cjk(block):
            flush()
            out.append(block)
            continue
        if pending and sum(len(p) for p in pending) + len(block) > CHUNK:
            flush()
        pending.append(block)
        if len(block) > CHUNK:
            flush()
    flush()
    return "\n\n".join(out) + ("\n" if text.endswith("\n") else "")


def translate_code(tr: Translator, text: str, lang: str) -> str:
    lines = text.split("\n")
    i, out = 0, []
    while i < len(lines):
        if not has_cjk(lines[i]):
            out.append(lines[i])
            i += 1
            continue
        j = i
        while j < len(lines) and has_cjk(lines[j]) and j - i < 40:
            j += 1
        run = lines[i:j]
        # The model sees the run without its common indentation; it goes back on afterwards.
        indent = os.path.commonprefix([re.match(r"^[ \t]*", l).group(0) for l in run])
        res = tr.translate("\n".join(l[len(indent):] for l in run), f"code:{lang}").split("\n")
        if len(res) != len(run):
            res = [tr.translate(l[len(indent):], f"code:{lang}") for l in run]
        out.extend(indent + r if r.strip() else r for r in res)
        i = j
    return "\n".join(out)


def is_text(p: Path) -> bool:
    if p.suffix.lower() not in TEXT_EXT:
        return False
    try:
        head = p.read_bytes()[:8192]
    except OSError:
        return False
    return b"\0" not in head


def translate_file(tr: Translator, p: Path) -> bool:
    try:
        text = p.read_text(encoding="utf-8")
    except UnicodeDecodeError:
        try:
            text = p.read_bytes().decode("gb18030")  # older Chinese files are often GBK
        except UnicodeDecodeError:
            return False
    if not has_cjk(text):
        return False
    if p.suffix.lower() in (".md", ".markdown", ".rst"):
        new = translate_markdown(tr, text)
    else:
        new = translate_code(tr, text, LANG.get(p.suffix.lower(), "source"))
    p.write_text(new, encoding="utf-8")
    return True


def ascii_name(tr: Translator, name: str) -> str:
    stem, ext = (name.rsplit(".", 1) + [""])[:2] if "." in name[1:] else (name, "")
    en = tr.translate(name, "name")
    en = re.sub(r"[^A-Za-z0-9 ._\-]+", "", en).strip() or hashlib.sha1(name.encode()).hexdigest()[:8]
    if ext and not en.lower().endswith("." + ext.lower()):
        en = f"{en}.{ext}"
    return re.sub(r"\s+", "-", en)


def rename_tree(tr: Translator, root: Path) -> dict[str, str]:
    """Rename Chinese file and folder names, deepest first. Returns {old relative path: new relative path}."""
    moves = {}
    for p in sorted(root.rglob("*"), key=lambda x: len(x.parts), reverse=True):
        if not has_cjk(p.name):
            continue
        new = p.with_name(ascii_name(tr, p.name))
        k = 1
        while new.exists():
            new = p.with_name(f"{new.stem}-{k}{new.suffix}")
            k += 1
        p.rename(new)
        moves[str(p.relative_to(root))] = str(new.relative_to(root))
        print(f"  renamed {p.relative_to(root)} -> {new.name}")
    return moves


def fix_links(root: Path, moves: dict[str, str]):
    """Markdown links that named a renamed file or folder now name the new one (by basename, URL-quoted too)."""
    from urllib.parse import quote
    pairs = []
    for old, new in moves.items():
        o, n = Path(old).name, Path(new).name
        pairs += [(o, n), (quote(o), quote(n))]
    if not pairs:
        return
    for p in root.rglob("*"):
        if p.is_file() and is_text(p):
            try:
                t = p.read_text(encoding="utf-8")
            except UnicodeDecodeError:
                continue
            u = t
            for o, n in pairs:
                u = u.replace(o, n)
            if u != t:
                p.write_text(u, encoding="utf-8")


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__.split("\n")[0])
    ap.add_argument("paths", nargs="+")
    ap.add_argument("--dry-run", action="store_true")
    ap.add_argument("--check", action="store_true")
    a = ap.parse_args()
    files = []
    for path in map(Path, a.paths):
        files += [path] if path.is_file() else [p for p in sorted(path.rglob("*")) if p.is_file() and ".git" not in p.parts]
    targets = [p for p in files if is_text(p) and has_cjk(p.read_text(encoding="utf-8", errors="replace"))]
    named = [p for p in files if has_cjk(str(p))]
    if a.check or a.dry_run:
        for p in targets:
            print(p)
        for p in named:
            print(f"name: {p}")
        return 1 if (a.check and (targets or named)) else 0
    tr = Translator()
    t0 = time.time()
    for n, p in enumerate(targets, 1):
        print(f"[{n}/{len(targets)}] {p}", flush=True)
        translate_file(tr, p)
    for path in map(Path, a.paths):
        if path.is_dir():
            fix_links(path, rename_tree(tr, path))
    print(f"done: {len(targets)} files, {tr.calls} model calls, {tr.seconds / 60:.1f} min in the model, "
          f"{(time.time() - t0) / 60:.1f} min total")
    if tr.failed:
        print(f"{len(tr.failed)} chunk(s) still contain Chinese and were left as they were:", file=sys.stderr)
        for f in tr.failed[:20]:
            print(f"  {f!r}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
