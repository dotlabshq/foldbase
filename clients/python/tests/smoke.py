"""End-to-end smoke: boot a real server, drive it with the Python client.

    python3 tests/smoke.py            # against the Go binary (default)
    SMOKE_TARGET=ts python3 tests/smoke.py

Exit 0 iff every check passes.
"""
from __future__ import annotations

import base64
import hashlib
import hmac
import json
import os
import socket
import subprocess
import sys
import time
import urllib.request

HERE = os.path.dirname(os.path.abspath(__file__))
ROOT = os.path.abspath(os.path.join(HERE, "..", "..", ".."))  # projects/foldbase
sys.path.insert(0, os.path.abspath(os.path.join(HERE, "..")))  # clients/python

from foldbase import Foldbase, FoldbaseError  # noqa: E402

SECRET = "py-client-smoke-secret-32-chars-min!"


def b64u(raw: bytes) -> str:
    return base64.urlsafe_b64encode(raw).rstrip(b"=").decode()


def sign_jwt(claims: dict, secret: str, ttl: int = 3600) -> str:
    now = int(time.time())
    header = b64u(json.dumps({"alg": "HS256", "typ": "JWT"}).encode())
    payload = b64u(json.dumps({"iat": now, "exp": now + ttl, **claims}).encode())
    signing = (header + "." + payload).encode()
    sig = b64u(hmac.new(secret.encode(), signing, hashlib.sha256).digest())
    return header + "." + payload + "." + sig


def free_port() -> int:
    s = socket.socket()
    s.bind(("127.0.0.1", 0))
    port = s.getsockname()[1]
    s.close()
    return port


def boot(port: int):
    use_go = os.environ.get("SMOKE_TARGET") != "ts"
    if use_go:
        cmd = ["./bin/foldbase"]
        cwd = os.path.join(ROOT, "go")
    else:
        cmd = ["node", "dist/index.js"]
        cwd = ROOT
    env = dict(os.environ, PORT=str(port), DB_URL=":memory:",
               FOLDBASE_AUTH="service-jwt", FOLDBASE_JWT_SECRET=SECRET)
    proc = subprocess.Popen(cmd, cwd=cwd, env=env, stdout=subprocess.PIPE, stderr=subprocess.STDOUT)
    base = "http://127.0.0.1:%d" % port
    deadline = time.time() + 10
    while time.time() < deadline:
        if proc.poll() is not None:
            raise RuntimeError("server exited early:\n" + proc.stdout.read().decode())
        try:
            with urllib.request.urlopen(base + "/healthz", timeout=1) as r:
                if r.status == 200:
                    return proc, base, use_go
        except Exception:
            time.sleep(0.12)
    proc.kill()
    raise RuntimeError("server not healthy in 10s")


failed = 0


def check(name: str, cond: bool, detail: str = "") -> None:
    global failed
    print("  %s %s%s" % ("✓" if cond else "✗", name, "" if cond else "\n      " + detail))
    if not cond:
        failed += 1


def main() -> int:
    port = free_port()
    proc, base, use_go = boot(port)
    print("\n▶ Python client smoke against %s\n" % ("Go" if use_go else "TS"))
    try:
        svc = sign_jwt({"sub": "app", "type": "service"}, SECRET)
        es = Foldbase(base, token=svc, tenant="acme")

        h = es.health()
        check("health ok", h.get("ok") is True and h.get("service") == "foldbase")

        reg = es.put_projection({
            "name": "notes",
            "columns": {"owner": "text", "text": "text", "created_at": "integer"},
            "on": {
                "NoteAdded": {"op": "upsert", "set": {"owner": "$.owner", "text": "$.text", "created_at": "$.createdAt"}},
                "NoteDeleted": {"op": "delete"},
            },
        })
        check("put_projection", reg.get("ok") is True and reg.get("name") == "notes")
        es.put_policy({"name": "notes", "role": "*", "using": "owner = :auth_uid"})

        ap = es.append("n1", 0, [{
            "type": "NoteAdded", "streamId": "n1", "actor": "u1",
            "payload": {"owner": "u1", "text": "hello", "createdAt": 111},
        }])
        check("append projected", ap.get("projected") is True and ap.get("version") == 1)
        check("append stamps writtenBy", ap["events"][0]["metadata"].get("writtenBy") == "app")

        as_u1 = es.with_auth(uid="u1")
        q = as_u1.query("notes")
        check("query row count", len(q["rows"]) == 1)
        check("query row shape", q["rows"][0].get("owner") == "u1" and q["rows"][0].get("text") == "hello")

        # tenant isolation via a different X-Tenant-ID (service token selects tenant)
        other = es.with_tenant("other").read_all()
        check("tenant isolation", len(other) == 0)

        # concurrency conflict → typed 409
        conflict = None
        try:
            es.append("n1", 0, [{"type": "NoteAdded", "streamId": "n1", "actor": "u1", "payload": {"owner": "u1", "text": "dup", "createdAt": 1}}])
        except FoldbaseError as e:
            conflict = e
        check("409 FoldbaseError with actual", conflict is not None and conflict.status == 409 and conflict.actual == 1)

        # delete + rebuild
        es.append("n1", 1, [{"type": "NoteDeleted", "streamId": "n1", "actor": "u1", "payload": {}}])
        check("delete removes row", len(as_u1.query("notes")["rows"]) == 0)
        rb = es.rebuild()
        check("rebuild", rb.get("ok") is True)

        # deny-by-default: no uid → policy unsatisfiable → 403
        denied = None
        try:
            es.query("notes")
        except FoldbaseError as e:
            denied = e
        check("deny without uid 403", denied is not None and denied.status == 403)
    finally:
        proc.kill()

    print("\n%s Python client smoke %s\n" % ("✅" if failed == 0 else "❌",
          "passed" if failed == 0 else ("%d failed" % failed)))
    return 0 if failed == 0 else 1


if __name__ == "__main__":
    sys.exit(main())
