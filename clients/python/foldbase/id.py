"""UUIDv7 generation for stream ids (aggregate identity) and client-supplied
event ids. A stream id is ALWAYS client-generated — the server can't mint it,
since it is the aggregate's identity chosen at creation.

No type prefix: the read model (`read_<name>`) already carries the type, so the
stream id is a bare, time-ordered UUIDv7 that becomes the read-model PK.
Stdlib only — no dependency on pydantic or anything else.
"""
from __future__ import annotations

import os
import time


def uuidv7() -> str:
    """A time-ordered UUIDv7 string (48-bit unix-ms prefix + random tail)."""
    ts = int(time.time() * 1000)
    b = bytearray(os.urandom(16))
    b[0] = (ts >> 40) & 0xFF
    b[1] = (ts >> 32) & 0xFF
    b[2] = (ts >> 24) & 0xFF
    b[3] = (ts >> 16) & 0xFF
    b[4] = (ts >> 8) & 0xFF
    b[5] = ts & 0xFF
    b[6] = (b[6] & 0x0F) | 0x70  # version 7
    b[8] = (b[8] & 0x3F) | 0x80  # variant 10
    h = b.hex()
    return "%s-%s-%s-%s-%s" % (h[0:8], h[8:12], h[12:16], h[16:20], h[20:32])


# Mint a fresh stream id (aggregate identity). Alias of uuidv7.
new_stream_id = uuidv7
