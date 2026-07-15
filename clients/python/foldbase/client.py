"""Typed HTTP client for foldbase.

Wire-compatible with any implementation of openapi.yaml (TS reference or Go).
Dependency-free — stdlib urllib only, so it runs anywhere Python does.

    es = Foldbase(base_url, token=svc_token, tenant="acme")
    es.put_projection({"name": "notes", "columns": {...}, "on": {...}})
    es.append("n1", 0, [{"type": "NoteAdded", "streamId": "n1",
                         "actor": "u1", "payload": {...}}])
    rows = es.query("notes", {"where": {"owner": {"eq": "u1"}}})["rows"]
"""
from __future__ import annotations

import json
import urllib.error
import urllib.parse
import urllib.request
from typing import Any, Dict, List, Optional

from .errors import FoldbaseError

JSON = Dict[str, Any]


class Foldbase:
    def __init__(
        self,
        base_url: str,
        token: Optional[str] = None,
        tenant: Optional[str] = None,
        auth: Optional[Dict[str, str]] = None,
        timeout: float = 30.0,
    ):
        self.base_url = base_url.rstrip("/")
        self.token = token
        self.tenant = tenant
        self.auth = auth or {}
        self.timeout = timeout

    # ── derivations ────────────────────────────────────────────────────────────
    def with_auth(self, **auth: str) -> "Foldbase":
        """Return a client with a different forwarded end-user identity."""
        return Foldbase(self.base_url, self.token, self.tenant, auth, self.timeout)

    def with_tenant(self, tenant: str) -> "Foldbase":
        """Return a client scoped to a different tenant."""
        return Foldbase(self.base_url, self.token, tenant, self.auth, self.timeout)

    # ── transport ──────────────────────────────────────────────────────────────
    def _headers(self, has_body: bool) -> Dict[str, str]:
        h: Dict[str, str] = {}
        if has_body:
            h["Content-Type"] = "application/json"
        if self.token:
            h["Authorization"] = "Bearer " + self.token
        if self.tenant:
            h["X-Tenant-ID"] = self.tenant
        if self.auth.get("uid"):
            h["X-Auth-UID"] = self.auth["uid"]
        if self.auth.get("role"):
            h["X-Auth-Role"] = self.auth["role"]
        if self.auth.get("email"):
            h["X-Auth-Email"] = self.auth["email"]
        return h

    def _call(self, method: str, path: str, body: Optional[Any] = None) -> Any:
        data = None
        if body is not None:
            data = json.dumps(body).encode("utf-8")
        req = urllib.request.Request(
            self.base_url + path, data=data, method=method, headers=self._headers(body is not None)
        )
        try:
            with urllib.request.urlopen(req, timeout=self.timeout) as resp:
                text = resp.read().decode("utf-8")
                return json.loads(text) if text else None
        except urllib.error.HTTPError as e:
            text = e.read().decode("utf-8")
            try:
                payload = json.loads(text) if text else {}
            except json.JSONDecodeError:
                payload = {}
            raise FoldbaseError(
                e.code, payload.get("error", "error"), payload.get("message"), payload.get("actual")
            ) from None

    # ── streams (data plane) ────────────────────────────────────────────────────
    def append(
        self, stream_id: str, expected_version: int, events: List[JSON], stream_type: Optional[str] = None
    ) -> JSON:
        """Append events with optimistic concurrency. Raises FoldbaseError(409) on conflict.

        stream_type fixes the aggregate's kind on first append (drives the
        ?type= category read); a later conflicting value is a 400.
        """
        body: JSON = {"expectedVersion": expected_version, "events": events}
        if stream_type:
            body["streamType"] = stream_type
        return self._call("POST", "/v1/streams/" + urllib.parse.quote(stream_id, safe=""), body)

    def stream_version(self, stream_id: str) -> JSON:
        return self._call("GET", "/v1/streams/" + urllib.parse.quote(stream_id, safe="") + "/version")

    def read_stream(self, stream_id: str, from_version: Optional[int] = None) -> List[JSON]:
        q = "?fromVersion=" + str(from_version) if from_version is not None else ""
        return self._call("GET", "/v1/streams/" + urllib.parse.quote(stream_id, safe="") + q)

    def read_all(
        self,
        from_global_seq: Optional[int] = None,
        type: Optional[str] = None,
        limit: Optional[int] = None,
    ) -> List[JSON]:
        """Read the tenant log in global order; `type` narrows to one stream category."""
        params = {}
        if from_global_seq is not None:
            params["fromGlobalSeq"] = str(from_global_seq)
        if type:
            params["type"] = type
        if limit is not None:
            params["limit"] = str(limit)
        q = "?" + urllib.parse.urlencode(params) if params else ""
        return self._call("GET", "/v1/events" + q)

    def read_by_correlation(self, correlation_id: str) -> List[JSON]:
        return self._call("GET", "/v1/events/by-correlation/" + urllib.parse.quote(correlation_id, safe=""))

    def query(self, name: str, request: Optional[JSON] = None) -> JSON:
        """Query a registered read model. Returns {'rows', 'limit', 'offset'}."""
        return self._call("POST", "/v1/query/" + urllib.parse.quote(name, safe=""), request or {})

    # ── definitions + admin (control plane; needs a service token) ──────────────
    def put_projection(self, definition: JSON) -> JSON:
        return self._call("PUT", "/v1/projections", definition)

    def put_policy(self, definition: JSON) -> JSON:
        return self._call("PUT", "/v1/policies", definition)

    def rebuild(self, name: Optional[str] = None) -> JSON:
        return self._call("POST", "/admin/rebuild", {"name": name} if name else {})

    def reload(self) -> JSON:
        return self._call("POST", "/admin/reload", {})

    def health(self) -> JSON:
        return self._call("GET", "/healthz")

    def subscribe(
        self,
        type: Optional[str] = None,
        from_global_seq: Optional[int] = None,
        last_event_id: Optional[int] = None,
    ):
        """Subscribe to appended events over SSE (realtime), as a blocking
        generator that yields each event dict. Ordered and gap-free; on a dropped
        connection, re-call with the last yielded event's `globalSeq` to resume.
        Requires a service token (the raw log bypasses row policies).

            for event in fb.subscribe(type="task"):
                handle(event)
        """
        params = {}
        if type:
            params["type"] = type
        if from_global_seq is not None:
            params["fromGlobalSeq"] = str(from_global_seq)
        q = "?" + urllib.parse.urlencode(params) if params else ""
        headers = self._headers(False)
        headers["Accept"] = "text/event-stream"
        if last_event_id is not None:
            headers["Last-Event-ID"] = str(last_event_id)
        req = urllib.request.Request(self.base_url + "/v1/subscribe" + q, headers=headers)
        try:
            resp = urllib.request.urlopen(req)  # no timeout — long-lived stream
        except urllib.error.HTTPError as e:
            payload = {}
            try:
                payload = json.loads(e.read().decode("utf-8"))
            except Exception:
                pass
            raise FoldbaseError(e.code, payload.get("error", "error"), payload.get("message")) from None

        buf = ""
        for chunk in resp:
            buf += chunk.decode("utf-8")
            while "\n\n" in buf:
                raw, buf = buf.split("\n\n", 1)
                if raw.startswith(":"):
                    continue  # heartbeat / comment
                data = None
                for line in raw.split("\n"):
                    if line.startswith("data:"):
                        data = line[5:].lstrip(" ")
                if data is not None:
                    yield json.loads(data)

    def catalog(self, catalog: Any) -> Any:
        """Bind an event catalog for typed, payload-validated writes (`emit`).

        Needs the authoring layer (pydantic, an optional extra); imported lazily
        so the core client stays dependency-free.
        """
        from .schema import TypedClient  # lazy: only pulls pydantic when used

        return TypedClient(self, catalog)
