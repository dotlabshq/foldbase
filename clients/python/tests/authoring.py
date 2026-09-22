"""End-to-end test of the typed authoring layer (needs pydantic).

    /tmp/fbvenv/bin/python tests/authoring.py     # against the Go binary

Proves: event-schema-driven column inference, proxy path capture compiles to
wire rules, runtime path validation, and typed emit (payload validation +
stream_type stamping).
"""
from __future__ import annotations

import base64
import enum
import hashlib
import hmac
import json
import os
import socket
import subprocess
import sys
import time
import urllib.request

from pydantic import BaseModel

HERE = os.path.dirname(os.path.abspath(__file__))
ROOT = os.path.abspath(os.path.join(HERE, "..", "..", ".."))  # projects/foldbase
sys.path.insert(0, os.path.abspath(os.path.join(HERE, "..")))  # clients/python

from foldbase import FoldBase, define_aggregate, define_projection  # noqa: E402

SECRET = "py-authoring-secret-32-chars-minimum!"

failed = 0


def check(name: str, cond: bool, detail: str = "") -> None:
    global failed
    print("  %s %s%s" % ("✓" if cond else "✗", name, "" if cond else "\n      " + detail))
    if not cond:
        failed += 1


def b64u(raw: bytes) -> str:
    return base64.urlsafe_b64encode(raw).rstrip(b"=").decode()


def sign_jwt(claims: dict, secret: str, ttl: int = 3600) -> str:
    now = int(time.time())
    header = b64u(json.dumps({"alg": "HS256", "typ": "JWT"}).encode())
    payload = b64u(json.dumps({"iat": now, "exp": now + ttl, **claims}).encode())
    sig = b64u(hmac.new(secret.encode(), (header + "." + payload).encode(), hashlib.sha256).digest())
    return header + "." + payload + "." + sig


def free_port() -> int:
    s = socket.socket()
    s.bind(("127.0.0.1", 0))
    p = s.getsockname()[1]
    s.close()
    return p


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


# ── the aggregate: pydantic models are the source of truth ────────────────────
class Status(str, enum.Enum):
    todo = "todo"
    doing = "doing"
    done = "done"


class Estimate(BaseModel):
    points: int


class TaskCreated(BaseModel):
    owner: str
    title: str
    at: int
    pinned: bool = False
    estimate: Estimate = Estimate(points=0)


class TaskMoved(BaseModel):
    status: Status


class TaskDeleted(BaseModel):
    pass


Tasks = define_aggregate("task", TaskCreated=TaskCreated, TaskMoved=TaskMoved, TaskDeleted=TaskDeleted)

# columns INFERRED from the model field types; proxy paths compile to "$.x"
tasks = define_projection("tasks", Tasks, lambda on: {
    "TaskCreated": on.TaskCreated.upsert(lambda e: {"owner": e.owner, "title": e.title, "status": "todo", "created_at": e.at, "pinned": e.pinned}),
    "TaskMoved": on.TaskMoved.upsert(lambda e: {"status": e.status}),
    "TaskDeleted": on.TaskDeleted.delete(),
})
stats = define_projection("board_stats", Tasks, lambda on: {
    "TaskCreated": on.TaskCreated.inc({"created": 1}),
})
# inc can read the payload: a total maintained by the fold, not by the app
totals = define_projection("board_totals", Tasks, lambda on: {
    "TaskCreated": on.TaskCreated.inc(lambda e: {"created": 1, "age_sum": e.at}),
})
# a nested inc path: the server resolves it and adds a number, so the column
# must be typed from the leaf — not folded to text the way a nested set is
nested = define_projection("board_points", Tasks, lambda on: {
    "TaskCreated": on.TaskCreated.inc(lambda e: {"points": e.estimate.points}),
})
# the row key is a rule too: one row per owner, across every task stream
per_owner = define_projection("board_per_owner", Tasks, lambda on: {
    "TaskCreated": on.TaskCreated.inc(lambda e: {"created": 1}, key=lambda e: e.owner),
    "TaskDeleted": on.TaskDeleted.delete(key=lambda e: e.owner),
})


def main() -> int:
    # 1. pure authoring assertions (no server needed)
    print("\n▶ Python authoring layer\n")
    check("column inference from model types",
          tasks.definition["columns"] == {"owner": "text", "title": "text", "status": "text", "created_at": "integer", "pinned": "boolean"},
          str(tasks.definition["columns"]))
    check("proxy path capture → wire rule",
          tasks.definition["on"]["TaskCreated"]["set"] == {"owner": "$.owner", "title": "$.title", "status": "todo", "created_at": "$.at", "pinned": "$.pinned"},
          str(tasks.definition["on"]["TaskCreated"]))
    check("delete rule compiled", tasks.definition["on"]["TaskDeleted"] == {"op": "delete"})
    check("inc rule → integer column", stats.definition["columns"] == {"created": "integer"} and stats.definition["on"]["TaskCreated"]["inc"] == {"created": 1})
    check("inc proxy path → wire path",
          totals.definition["on"]["TaskCreated"]["inc"] == {"created": 1, "age_sum": "$.at"},
          str(totals.definition["on"]["TaskCreated"]))
    check("inc path column typed from the model",
          totals.definition["columns"] == {"created": "integer", "age_sum": "integer"},
          str(totals.definition["columns"]))
    check("nested inc path compiles to a wire path",
          nested.definition["on"]["TaskCreated"]["inc"] == {"points": "$.estimate.points"},
          str(nested.definition["on"]["TaskCreated"]))
    check("nested inc column typed from the leaf, not text",
          nested.definition["columns"] == {"points": "integer"},
          str(nested.definition["columns"]))
    check("key proxy → wire path",
          per_owner.definition["on"]["TaskCreated"]["key"] == "$.owner",
          str(per_owner.definition["on"]["TaskCreated"]))
    check("delete carries the same key",
          per_owner.definition["on"]["TaskDeleted"] == {"op": "delete", "key": "$.owner"},
          str(per_owner.definition["on"]["TaskDeleted"]))

    # runtime path validation: a typo raises at author time
    typo_raised = False
    try:
        define_projection("bad", Tasks, lambda on: {"TaskCreated": on.TaskCreated.upsert(lambda e: {"owner": e.ownerr})})
    except ValueError:
        typo_raised = True
    check("runtime path validation catches typo (e.ownerr)", typo_raised)

    # 2. end-to-end against the Go binary
    port = free_port()
    proc, base = boot(port)
    try:
        svc = sign_jwt({"sub": "app", "type": "service"}, SECRET)
        fb = FoldBase(base, token=svc, tenant="acme")
        fb.put_projection(tasks.definition)
        fb.put_projection(stats.definition)
        fb.put_policy({"name": "tasks", "role": "*", "using": "owner = :auth_uid"})
        fb.put_policy({"name": "board_stats", "role": "*"})

        write = fb.catalog(Tasks)
        tid = Tasks.new_id()
        check("new_id is bare uuidv7", len(tid) == 36 and tid[14] == "7")

        ap = write.emit(tid, 0, "TaskCreated", {"owner": "alice", "title": "Ship python", "at": 1}, actor="alice")
        check("typed emit stamps stream_type", ap["events"][0]["streamType"] == "task")
        check("typed emit projected", ap.get("projected") is True)

        # enum payload validates + serializes to its value over the wire
        write.emit(tid, 1, "TaskMoved", {"status": "doing"}, actor="alice")
        rows = fb.with_auth(uid="alice").query("tasks")["rows"]
        check("read model reflects typed writes", len(rows) == 1 and rows[0]["status"] == "doing" and rows[0]["created_at"] == 1)

        # client-side payload validation rejects a bad payload BEFORE the server
        bad_raised = False
        try:
            write.emit(Tasks.new_id(), 0, "TaskMoved", {"status": "nonsense"}, actor="alice")
        except Exception:
            bad_raised = True
        check("emit rejects invalid payload (bad enum)", bad_raised)

        # category read via stream_type
        cat = fb.read_all(type="task")
        check("category read by stream_type", len(cat) >= 1 and all(e["streamType"] == "task" for e in cat))
    finally:
        proc.kill()

    print("\n%s Python authoring %s\n" % ("✅" if failed == 0 else "❌", "passed" if failed == 0 else "%d failed" % failed))
    return 0 if failed == 0 else 1


if __name__ == "__main__":
    sys.exit(main())
