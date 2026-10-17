#!/usr/bin/env python3
"""Static server for the ACT training portal: gzip, sane caching, no directory listings, no dotfiles."""
import gzip
import os
from functools import lru_cache
from http import HTTPStatus
from http.server import SimpleHTTPRequestHandler, ThreadingHTTPServer

ROOT = os.path.join(os.path.dirname(os.path.abspath(__file__)), "www")
PORT = int(os.environ.get("PORT", "8850"))
COMPRESS = {".html", ".js", ".css", ".json", ".svg", ".txt"}


@lru_cache(maxsize=64)
def gz(path, mtime):
    with open(path, "rb") as f:
        return gzip.compress(f.read(), 6)


class Handler(SimpleHTTPRequestHandler):
    server_version = "portal"
    sys_version = ""

    def __init__(self, *a, **kw):
        super().__init__(*a, directory=ROOT, **kw)

    def log_message(self, fmt, *args):
        pass

    def list_directory(self, path):
        self.send_error(HTTPStatus.NOT_FOUND)
        return None

    def end_headers(self):
        self.send_header("X-Content-Type-Options", "nosniff")
        self.send_header("Referrer-Policy", "no-referrer")
        super().end_headers()

    def do_GET(self):
        path = self.translate_path(self.path)
        name = os.path.basename(path)
        if name.startswith(".") or name.endswith(".tmp"):
            return self.send_error(HTTPStatus.NOT_FOUND)
        if os.path.isdir(path):
            path = os.path.join(path, "index.html")
        if not os.path.isfile(path):
            return self.send_error(HTTPStatus.NOT_FOUND)
        ext = os.path.splitext(path)[1]
        st = os.stat(path)
        if not self.path.startswith("/assets/"):
            cache = "no-cache"
        else:
            cache = "public, max-age=86400"
        body = None
        use_gz = ext in COMPRESS and "gzip" in self.headers.get("Accept-Encoding", "")
        if use_gz:
            body = gz(path, st.st_mtime_ns)
        else:
            with open(path, "rb") as f:
                body = f.read()
        self.send_response(HTTPStatus.OK)
        self.send_header("Content-Type", self.guess_type(path))
        self.send_header("Content-Length", str(len(body)))
        self.send_header("Cache-Control", cache)
        self.send_header("Last-Modified", self.date_time_string(st.st_mtime))
        if use_gz:
            self.send_header("Content-Encoding", "gzip")
            self.send_header("Vary", "Accept-Encoding")
        self.end_headers()
        self.wfile.write(body)



Handler.extensions_map.update({".js": "text/javascript; charset=utf-8", ".json": "application/json; charset=utf-8",
                               ".woff2": "font/woff2", ".html": "text/html; charset=utf-8", ".css": "text/css; charset=utf-8"})

if __name__ == "__main__":
    ThreadingHTTPServer(("0.0.0.0", PORT), Handler).serve_forever()
