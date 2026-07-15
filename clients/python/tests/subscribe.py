"""SSE subscribe smoke: boot Go, subscribe via the Python client (in a thread),
append, receive.  python3 tests/subscribe.py"""
from __future__ import annotations

import base64
import hashlib
import hmac
import json
import os
import socket
import subprocess
import sys
import threading
import time
import urllib.request

HERE = os.path.dirname(os.path.abspath(__file__))
ROOT = os.path.abspath(os.path.join(HERE, "..", "..", ".."))
sys.path.insert(0, os.path.abspath(os.path.join(HERE, "..")))

from foldbase import FoldBase  # noqa: E402

SECRET = "py-subscribe-secret-32-chars-minimum!"
failed = 0


def check(name, cond):
    global failed
    print("  %s %s" % ("✓" if cond else "✗", name))
    if not cond:
        failed += 1


def b64u(raw):
    return base64.urlsafe_b64encode(raw).rstrip(b"=").decode()


def sign(claims):
    now = int(time.time())
    h = b64u(json.dumps({"alg": "HS256", "typ": "JWT"}).encode())
    p = b64u(json.dumps({"iat": now, "exp": now + 3600, **claims}).encode())
    s = b64u(hmac.new(SECRET.encode(), (h + "." + p).encode(), hashlib.sha256).digest())
    return h + "." + p + "." + s


def free_port():
    s = socket.socket()
    s.bind(("127.0.0.1", 0))
    p = s.getsockname()[1]
    s.close()
    return p


def boot(port):
    env = dict(os.environ, PORT=str(port), DB_URL=":memory:", FOLDBASE_AUTH="service-jwt", FOLDBASE_JWT_SECRET=SECRET)
    proc = subprocess.Popen(["./bin/foldbase"], cwd=os.path.join(ROOT, "go"), env=env,
                            stdout=subprocess.PIPE, stderr=subprocess.STDOUT)
    base = "http://127.0.0.1:%d" % port
    for _ in range(80):
        try:
            with urllib.request.urlopen(base + "/healthz", timeout=1) as r:
                if r.status == 200:
                    return proc, base
        except Exception:
            time.sleep(0.12)
    proc.kill()
    raise RuntimeError("no server")


def main():
    port = free_port()
    proc, base = boot(port)
    try:
        fb = FoldBase(base, token=sign({"sub": "app", "type": "service"}), tenant="acme")
        got = []

        def listen():
            try:
                for e in fb.subscribe(type="task"):
                    got.append(e)
            except Exception:
                pass

        t = threading.Thread(target=listen, daemon=True)
        t.start()
        time.sleep(0.3)  # let the subscription establish

        fb.append("t1", 0, [{"type": "TaskCreated", "streamId": "t1", "actor": "a", "payload": {"x": 1}}], stream_type="task")
        fb.append("u1", 0, [{"type": "UserAdded", "streamId": "u1", "actor": "a", "payload": {}}], stream_type="user")
        fb.append("t1", 1, [{"type": "TaskMoved", "streamId": "t1", "actor": "a", "payload": {"x": 2}}], stream_type="task")

        for _ in range(40):
            if len(got) >= 2:
                break
            time.sleep(0.05)

        check("received live task events", len(got) == 2)
        check("in order", got[0]["type"] == "TaskCreated" and got[1]["type"] == "TaskMoved")
        check("category filter excluded user stream", all(e["streamType"] == "task" for e in got))
    finally:
        proc.kill()

    print("\n%s Python subscribe %s\n" % ("✅" if failed == 0 else "❌", "passed" if failed == 0 else "%d failed" % failed))
    return 0 if failed == 0 else 1


if __name__ == "__main__":
    sys.exit(main())
