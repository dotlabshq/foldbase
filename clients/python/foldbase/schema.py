"""The typed authoring layer for Python (Approach B — proxy path capture),
built on pydantic. Mirrors the TS `@baseworks/foldbase` schema module.

Event payload schemas (pydantic models) are the single source of truth. A
projection's field mappings are written as typed accessors
(`lambda e: {"owner": e.owner}`) captured by a proxy that compiles `e.owner`
down to the wire path `"$.owner"`. Read-model column types are INFERRED from
the model field types, so the event schema drives the `read_<name>` table.

Because Python type-checking is opt-in, `define_projection` ALSO validates
every captured path against the event's model at author time — a typo like
`e.ownerr` raises immediately instead of silently writing NULL. That runtime
guard is the Python-appropriate stand-in for TS's compile-time check.

Requires pydantic (an optional extra); the core client stays dependency-free.
"""
from __future__ import annotations

import enum
import re
import typing
from typing import Any, Callable, Dict, Optional, Type

from pydantic import BaseModel

from .id import uuidv7

_IDENT = re.compile(r"^[a-z][a-z0-9_]*$")
Schemas = Dict[str, Type[BaseModel]]


# ── event catalog ─────────────────────────────────────────────────────────────
class EventCatalog:
    """The events that may enter the log — the source of truth for payloads.

    An aggregate catalog also carries a `stream_type` (stamped on every append
    via a bound client) and `new_id()` to mint the aggregate's stream id.
    """

    def __init__(self, schemas: Schemas, stream_type: Optional[str] = None):
        self.schemas = schemas
        self.stream_type = stream_type

    def new_id(self) -> str:
        """Mint a bare UUIDv7 stream id (the read-model PK)."""
        return uuidv7()


def define_events(**schemas: Type[BaseModel]) -> EventCatalog:
    """Declare event payload schemas by type name, e.g.
    `define_events(TaskCreated=TaskCreated, TaskMoved=TaskMoved)`."""
    return EventCatalog(schemas)


def define_aggregate(stream_type: str, **schemas: Type[BaseModel]) -> EventCatalog:
    """Declare an aggregate: its stream type + its event catalog."""
    if not _IDENT.match(stream_type):
        raise ValueError("foldbase: stream type '%s' must be a lowercase identifier" % stream_type)
    return EventCatalog(schemas, stream_type)


# ── pydantic type → column type ───────────────────────────────────────────────
def _unwrap_optional(t: Any) -> Any:
    if typing.get_origin(t) is typing.Union:
        args = [a for a in typing.get_args(t) if a is not type(None)]  # noqa: E721
        if len(args) == 1:
            return args[0]
    return t


def _col_type_of(annotation: Any) -> str:
    t = _unwrap_optional(annotation)
    if typing.get_origin(t) is typing.Literal:
        return "text"  # Literal['a','b'] — the Python z.enum
    if isinstance(t, type):
        if issubclass(t, bool):  # bool before int (bool is an int subclass)
            return "integer"
        if issubclass(t, enum.Enum):
            return "text"
        if issubclass(t, int):
            return "integer"
        if issubclass(t, float):
            return "real"
        if issubclass(t, str):
            return "text"
    return "text"  # lists/dicts/models fold to JSON text (jsonCol counterpart)


# ── proxy path capture ────────────────────────────────────────────────────────
class _Path:
    """Records field access as a "$.a.b" payload path."""

    __slots__ = ("_p",)

    def __init__(self, p: str):
        object.__setattr__(self, "_p", p)

    def __getattr__(self, key: str) -> "_Path":
        cur = object.__getattribute__(self, "_p")
        return _Path("$." + key if cur == "" else cur + "." + key)


def _path_of(v: Any) -> Optional[str]:
    if isinstance(v, _Path):
        return object.__getattribute__(v, "_p")
    return None


# ── rule builders ─────────────────────────────────────────────────────────────
class _RuleBuilder:
    def __init__(self, event_type: str):
        self._evt = event_type

    def upsert(self, fn: Callable[[Any], Dict[str, Any]], inc: Optional[Dict[str, float]] = None) -> Dict[str, Any]:
        mapping = fn(_Path(""))
        return {"op": "upsert", "_set": mapping, "_inc": inc, "_evt": self._evt}

    def inc(self, counters: Dict[str, float]) -> Dict[str, Any]:
        return {"op": "upsert", "_inc": counters, "_evt": self._evt}

    def delete(self) -> Dict[str, Any]:
        return {"op": "delete", "_evt": self._evt}


class _On:
    def __init__(self, catalog: EventCatalog):
        self._cat = catalog

    def __getattr__(self, event_type: str) -> _RuleBuilder:
        if event_type not in self._cat.schemas:
            raise ValueError("foldbase: unknown event type '%s' (not in the catalog)" % event_type)
        return _RuleBuilder(event_type)


# ── define_projection ─────────────────────────────────────────────────────────
class ProjectionSpec:
    """Holds the plain wire ProjectionDef — pass `.definition` to put_projection."""

    def __init__(self, definition: Dict[str, Any]):
        self.definition = definition


def _infer(columns: Dict[str, str], col: str, col_type: str) -> None:
    existing = columns.get(col)
    if existing is not None and existing != col_type:
        raise ValueError(
            "foldbase: column '%s' inferred as both '%s' and '%s' — declare it explicitly"
            % (col, existing, col_type)
        )
    columns[col] = col_type


def _col_type_from_path(catalog: EventCatalog, evt_type: str, path: str) -> str:
    model = catalog.schemas.get(evt_type)
    if model is None:
        raise ValueError("foldbase: unknown event type '%s'" % evt_type)
    parts = path[2:].split(".")
    field = model.model_fields.get(parts[0])
    if field is None:
        raise ValueError("foldbase: event '%s' has no field '%s' (path %s)" % (evt_type, parts[0], path))
    if len(parts) != 1:
        return "text"  # nested path folds to JSON text
    return _col_type_of(field.annotation)


def _col_type_from_literal(val: Any) -> str:
    if isinstance(val, bool):
        return "integer"
    if isinstance(val, int):
        return "integer"
    if isinstance(val, float):
        return "real"
    return "text"


def define_projection(
    name: str,
    catalog: EventCatalog,
    build: Callable[[_On], Dict[str, Dict[str, Any]]],
    columns: Optional[Dict[str, str]] = None,
    table: Optional[str] = None,
) -> ProjectionSpec:
    """Build a projection from an event catalog. Column types are inferred from
    the event field feeding each column (a literal like 'todo' → text, a
    counter → integer); a conflict raises. Pass `columns` to override any."""
    rules = build(_On(catalog))
    fixed = columns or {}
    cols: Dict[str, str] = dict(fixed)
    wire_on: Dict[str, Any] = {}

    for evt_type, raw in rules.items():
        if raw["op"] == "delete":
            wire_on[evt_type] = {"op": "delete"}
            continue
        wset: Dict[str, Any] = {}
        for col, val in (raw.get("_set") or {}).items():
            path = _path_of(val)
            if path is not None:
                wset[col] = path
                if col not in fixed:
                    _infer(cols, col, _col_type_from_path(catalog, raw["_evt"], path))
            else:
                wset[col] = val
                if col not in fixed:
                    _infer(cols, col, _col_type_from_literal(val))
        for col in (raw.get("_inc") or {}):
            if col not in fixed:
                _infer(cols, col, "integer")
        rule: Dict[str, Any] = {"op": "upsert"}
        if wset:
            rule["set"] = wset
        if raw.get("_inc"):
            rule["inc"] = raw["_inc"]
        wire_on[evt_type] = rule

    definition: Dict[str, Any] = {"name": name, "columns": cols, "on": wire_on}
    if table:
        definition["table"] = table
    return ProjectionSpec(definition)


# ── typed emit ────────────────────────────────────────────────────────────────
class TypedClient:
    """A catalog-bound wrapper: `emit` validates the payload against the event's
    pydantic model before appending, and stamps the aggregate's stream_type."""

    def __init__(self, client: Any, catalog: EventCatalog):
        self._c = client
        self._cat = catalog

    def emit(
        self,
        stream_id: str,
        expected_version: int,
        event_type: str,
        payload: Dict[str, Any],
        actor: str = "system",
        id: Optional[str] = None,
        causation_id: Optional[str] = None,
        correlation_id: Optional[str] = None,
        metadata: Optional[Dict[str, Any]] = None,
    ) -> Dict[str, Any]:
        model = self._cat.schemas.get(event_type)
        if model is None:
            raise ValueError("foldbase: unknown event type '%s'" % event_type)
        # client-side contract validation; mode='json' makes enums/datetimes wire-safe
        validated = model.model_validate(payload).model_dump(mode="json")
        event: Dict[str, Any] = {"type": event_type, "streamId": stream_id, "actor": actor, "payload": validated}
        if id:
            event["id"] = id
        if causation_id:
            event["causationId"] = causation_id
        if correlation_id:
            event["correlationId"] = correlation_id
        if metadata:
            event["metadata"] = metadata
        return self._c.append(stream_id, expected_version, [event], stream_type=self._cat.stream_type)
