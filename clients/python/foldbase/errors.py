"""Error type for foldbase client."""
from __future__ import annotations

from typing import Optional


class FoldbaseError(Exception):
    """Raised on any non-2xx response.

    Attributes:
        status: HTTP status code.
        code: machine-readable error string (e.g. "concurrency_conflict").
        message: human-readable detail, if any.
        actual: present on 409 conflicts — the stream's actual current version.
    """

    def __init__(self, status: int, code: str, message: Optional[str] = None, actual: Optional[int] = None):
        super().__init__(message or code)
        self.status = status
        self.code = code
        self.message = message
        self.actual = actual

    def __repr__(self) -> str:
        return f"FoldbaseError(status={self.status}, code={self.code!r}, actual={self.actual!r})"
