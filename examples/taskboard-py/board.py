"""taskboard-py — the same kanban as taskboard-ts, driven by the Python client.

Exercises the whole surface: projections (upsert/inc/delete), policies (owner +
admin role), append with optimistic concurrency (409), generic query
(where/sort), tenant isolation, and rebuild.

Runnable end-to-end: boots the Go binary itself (service-jwt mode).
    python3 board.py
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
ROOT = os.path.abspath(os.path.join(HERE, "..", ".."))  # projects/foldbase
sys.path.insert(0, os.path.join(ROOT, "clients", "python"))

from foldbase import Foldbase, FoldbaseError  # noqa: E402

SECRET = "taskboard-demo-secret-32-chars-minimum!"


def b64u(raw: bytes) -> str:
    return base64.urlsafe_b64encode(raw).rstrip(b"=").decode()


def sign_jwt(claims: dict, secret: str, ttl: int = 3600) -> str:
    now = int(time.time())
    header = b64u(json.dumps({"alg": "HS256", "typ": "JWT"}).encode())
    payload = b64u(json.dumps({"iat": now, "exp": now + ttl, **claims}).encode())
    sig = b64u(hmac.new(secret.encode(), (header + "." + payload).encode(), hashlib.sha256).digest())
    return header + "." + payload + "." + sig


def boot(port: int):
    env = dict(os.environ, PORT=str(port), DB_URL=":memory:",
               FOLDBASE_AUTH="service-jwt", FOLDBASE_JWT_SECRET=SECRET)
    proc = subprocess.Popen(["./bin/foldbase"], cwd=os.path.join(ROOT, "go"),
                            env=env, stdout=subprocess.PIPE, stderr=subprocess.STDOUT)
    base = "http://127.0.0.1:%d" % port
    deadline = time.time() + 10
    while time.time() < deadline:
        if proc.poll() is not None:
            raise RuntimeError("server exited early:\n" + proc.stdout.read().decode())
        try:
            with urllib.request.urlopen(base + "/healthz", timeout=1) as r:
                if r.status == 200:
                    return proc, base
        except Exception:
            time.sleep(0.12)
    proc.kill()
    raise RuntimeError("server not healthy in 10s")


TASKS = {
    "name": "tasks",
    "columns": {"owner": "text", "title": "text", "status": "text", "created_at": "integer"},
    "on": {
        "TaskCreated": {"op": "upsert", "set": {"owner": "$.owner", "title": "$.title", "status": "todo", "created_at": "$.at"}},
        "TaskMoved": {"op": "upsert", "set": {"status": "$.status"}},
        "TaskCompleted": {"op": "upsert", "set": {"status": "done"}},
        "TaskDeleted": {"op": "delete"},
    },
}
STATS = {
    "name": "board_stats",
    "columns": {"created": "integer", "completed": "integer"},
    "on": {
        "TaskCreated": {"op": "upsert", "inc": {"created": 1}},
        "TaskCompleted": {"op": "upsert", "inc": {"completed": 1}},
    },
}


def show(title, rows):
    print("\n  " + title)
    if not rows:
        print("    (none)")
    for r in rows:
        print("    • [%s] %s  (owner %s)" % (r.get("status"), r.get("title"), r.get("owner")))


def free_port() -> int:
    s = socket.socket()
    s.bind(("127.0.0.1", 0))
    p = s.getsockname()[1]
    s.close()
    return p


def main() -> int:
    port = free_port()
    proc, base = boot(port)
    try:
        svc = sign_jwt({"sub": "taskboard-api", "type": "service"}, SECRET)
        api = Foldbase(base, token=svc, tenant="acme")

        api.put_projection(TASKS)
        api.put_projection(STATS)
        api.put_policy({"name": "tasks", "role": "*", "using": "owner = :auth_uid"})
        api.put_policy({"name": "tasks", "role": "admin"})
        api.put_policy({"name": "board_stats", "role": "*"})
        print("▶ taskboard — registered projections: tasks, board_stats\n")

        api.append("t1", 0, [{"type": "TaskCreated", "streamId": "t1", "actor": "alice", "payload": {"owner": "alice", "title": "Write ADRs", "at": 1}}])
        api.append("t2", 0, [{"type": "TaskCreated", "streamId": "t2", "actor": "alice", "payload": {"owner": "alice", "title": "Ship Go port", "at": 2}}])
        api.append("t3", 0, [{"type": "TaskCreated", "streamId": "t3", "actor": "bob", "payload": {"owner": "bob", "title": "Python client", "at": 3}}])
        api.append("t1", 1, [{"type": "TaskMoved", "streamId": "t1", "actor": "alice", "payload": {"status": "doing"}}])
        api.append("t2", 1, [{"type": "TaskCompleted", "streamId": "t2", "actor": "alice", "payload": {}}])

        alice = api.with_auth(uid="alice")
        bob = api.with_auth(uid="bob")
        show("alice's tasks (sorted)", alice.query("tasks", {"sort": ["created_at"]})["rows"])
        show("bob's tasks", bob.query("tasks")["rows"])

        admin = api.with_auth(uid="root", role="admin")
        show('admin — everything in "doing"', admin.query("tasks", {"where": {"status": {"eq": "doing"}}})["rows"])

        print("\n  board_stats: %s" % json.dumps(alice.query("board_stats")["rows"]))

        try:
            api.append("t1", 1, [{"type": "TaskMoved", "streamId": "t1", "actor": "alice", "payload": {"status": "done"}}])
        except FoldbaseError as e:
            if e.status == 409:
                print("\n  ⚠ stale write on t1 rejected (409); stream actually at version %s — re-read & retry" % e.actual)
            else:
                raise

        other = Foldbase(base, token=svc, tenant="globex").with_auth(uid="alice")
        print("\n  globex tenant sees %d tasks (isolation)" % len(other.query("tasks")["rows"]))

        rb = api.rebuild()
        print("\n  rebuilt board from %d events; alice still has %d tasks" %
              (rb["rebuiltFrom"], len(alice.query("tasks")["rows"])))

        print("\n✅ taskboard-py demo complete\n")
        return 0
    finally:
        proc.kill()


if __name__ == "__main__":
    sys.exit(main())
