#!/usr/bin/env python3
"""PiPER ACT relay — public :8790 -> 127.0.0.1:18790 (the Dell's SSH reverse tunnel).

Runs on the server (Ubuntu). One process, one port, nothing else touched. All compute stays on
the Dell; this only forwards HTTP and streams the response body so the camera MJPEG works.
Uses only the standard library. Raw-socket request parsing + http.client to the backend, so a
long-lived multipart stream is forwarded byte-for-byte.

Run:  nohup python3 relay.py > /opt/piper-act/relay.log 2>&1 &
"""
import http.client
import socketserver

BACKEND_HOST = "127.0.0.1"
BACKEND_PORT = 18790
LISTEN_PORT = 8790
HOP = {"host", "connection", "keep-alive", "proxy-connection", "transfer-encoding", "upgrade", "te", "trailer"}


class Proxy(socketserver.StreamRequestHandler):
    def handle(self):
        try:
            self._forward()
        except Exception:
            pass

    def _read_request(self):
        line = self.rfile.readline()
        if not line:
            return None
        parts = line.decode("latin-1").strip().split(" ")
        if len(parts) < 2:
            return None
        method, path = parts[0], parts[1]
        headers = {}
        while True:
            h = self.rfile.readline()
            if h in (b"\r\n", b"\n", b""):
                break
            s = h.decode("latin-1").strip()
            if ":" in s:
                k, v = s.split(":", 1)
                headers[k.strip().lower()] = v.strip()
        length = int(headers.get("content-length", 0) or 0)
        body = self.rfile.read(length) if length else b""
        return method, path, headers, body

    def _forward(self):
        req = self._read_request()
        if req is None:
            return
        method, path, headers, body = req
        conn = http.client.HTTPConnection(BACKEND_HOST, BACKEND_PORT, timeout=600)
        try:
            fwd = {k: v for k, v in headers.items() if k not in HOP}
            conn.request(method, path, body=body, headers=fwd)
            resp = conn.getresponse()
            self.wfile.write(f"HTTP/1.1 {resp.status} {resp.reason}\r\n".encode("latin-1"))
            for k, v in resp.getheaders():
                if k.lower() in HOP or k.lower() == "content-length":
                    continue
                self.wfile.write(f"{k}: {v}\r\n".encode("latin-1"))
            self.wfile.write(b"\r\n")
            while True:
                chunk = resp.read(65536)
                if not chunk:
                    break
                self.wfile.write(chunk)
                self.wfile.flush()
        except Exception as e:
            try:
                body = f"PiPER ACT portal offline (Dell not connected or starting): {e}".encode()
                self.wfile.write(b"HTTP/1.1 502 Bad Gateway\r\nContent-Type: text/plain; charset=utf-8\r\n"
                                 b"Content-Length: " + str(len(body)).encode() + b"\r\nConnection: close\r\n\r\n" + body)
            except Exception:
                pass
        finally:
            conn.close()


if __name__ == "__main__":
    with socketserver.ThreadingTCPServer(("0.0.0.0", LISTEN_PORT), Proxy) as srv:
        srv.daemon_threads = True
        srv.allow_reuse_address = True
        print(f"piper-act relay listening :{LISTEN_PORT} -> {BACKEND_HOST}:{BACKEND_PORT}", flush=True)
        srv.serve_forever()
