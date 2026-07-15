"""foldbase Python client — append, query, and definitions over HTTP.

The core client is dependency-free (stdlib only). The typed authoring layer
(`define_aggregate` / `define_projection`, event-schema-driven columns) is an
optional extra requiring pydantic — install with `pip install foldbase[schema]`.
Wire-compatible with openapi.yaml.
"""
from .client import Foldbase
from .errors import FoldbaseError
from .id import new_stream_id, uuidv7

__all__ = ["Foldbase", "FoldbaseError", "new_stream_id", "uuidv7"]
__version__ = "0.1.0"

try:  # authoring layer — only available when pydantic is installed
    from .schema import (
        EventCatalog,
        ProjectionSpec,
        TypedClient,
        define_aggregate,
        define_events,
        define_projection,
    )

    __all__ += [
        "define_events",
        "define_aggregate",
        "define_projection",
        "EventCatalog",
        "ProjectionSpec",
        "TypedClient",
    ]
except ImportError:  # pydantic not installed — core client still works
    pass
