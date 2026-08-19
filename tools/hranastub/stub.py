#!/usr/bin/env python3
"""A minimal Hrana v2 server, so the real remote libSQL driver can be exercised
locally.

This exists because the deploy to Fly failed in a way no local test could catch:
local runs used an embedded SQLite driver, while production goes over Hrana-HTTP
with baton/stream semantics that no embedded driver has. Any bug in that seam is
invisible until it reaches Turso.

The baton behaviour is the part that matters and is reproduced faithfully:

  - A pipeline that leaves no transaction open returns NO baton. The Go driver
    treats an empty baton as "stream closed" and marks that connection dead, so
    the next request on it fails with driver.ErrBadConn.
  - A pipeline that leaves a transaction open returns a baton, and the driver
    continues the stream with it.

database/sql retries ErrBadConn for pool-level calls, which is why plain
Exec/Query survive. It does NOT retry on a pinned sql.Conn, which is exactly how
the migration runner broke.

Usage:
    stub.py <port> <sqlite-file>
"""

import base64
import json
import sqlite3
import sys
import threading
import uuid
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

DB_PATH = ":memory:"
LOCK = threading.Lock()

# Sessions keyed by baton. A session holds a connection whose transaction is
# still open, which is the only reason a stream needs to persist.
SESSIONS = {}


class Session:
    def __init__(self):
        self.conn = sqlite3.connect(DB_PATH, check_same_thread=False, isolation_level=None)
        self.conn.execute("PRAGMA foreign_keys=ON")

    def in_transaction(self):
        return self.conn.in_transaction

    def close(self):
        try:
            self.conn.close()
        except Exception:
            pass


def to_hrana_value(v):
    if v is None:
        return {"type": "null"}
    if isinstance(v, bool):
        return {"type": "integer", "value": str(int(v))}
    if isinstance(v, int):
        # Hrana carries integers as strings, to survive 64-bit precision.
        return {"type": "integer", "value": str(v)}
    if isinstance(v, float):
        return {"type": "float", "value": v}
    if isinstance(v, bytes):
        # Hrana carries blobs as base64 WITHOUT padding.
        return {"type": "blob", "base64": base64.b64encode(v).decode().rstrip("=")}
    return {"type": "text", "value": str(v)}


def from_hrana_value(v):
    kind = v.get("type")
    if kind == "null":
        return None
    if kind == "integer":
        return int(v.get("value"))
    if kind == "float":
        return float(v.get("value"))
    if kind == "blob":
        raw = v.get("base64") or ""
        # The protocol omits padding, so restore it before decoding. Getting this
        # wrong makes every BLOB write fail with "Incorrect padding", which looks
        # exactly like an application bug.
        return base64.b64decode(raw + "=" * (-len(raw) % 4))
    return v.get("value")


def execute_stmt(session, stmt):
    sql = stmt.get("sql") or ""
    args = [from_hrana_value(a) for a in (stmt.get("args") or [])]

    named = {}
    for na in stmt.get("named_args") or []:
        named[na.get("name")] = from_hrana_value(na.get("value"))

    cur = session.conn.cursor()
    if named:
        cur.execute(sql, named)
    else:
        cur.execute(sql, args)

    cols = []
    rows = []
    if cur.description:
        cols = [{"name": d[0], "decltype": None} for d in cur.description]
        if stmt.get("want_rows"):
            rows = [[to_hrana_value(v) for v in row] for row in cur.fetchall()]
        else:
            cur.fetchall()

    last_id = cur.lastrowid
    return {
        "cols": cols,
        "rows": rows,
        "affected_row_count": cur.rowcount if cur.rowcount and cur.rowcount > 0 else 0,
        "last_insert_rowid": str(last_id) if last_id else None,
        "replication_index": None,
    }


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, fmt, *args):
        pass

    def do_GET(self):
        if self.path.startswith("/health"):
            self.reply(200, b"ok", "text/plain")
            return
        if self.path == "/v2":
            self.reply(200, b"{}", "application/json")
            return
        self.send_error(404)

    def do_POST(self):
        path = self.path.split("?", 1)[0]
        if path not in ("/v2/pipeline", "/v3/pipeline"):
            self.send_error(404)
            return

        length = int(self.headers.get("Content-Length", 0))
        try:
            request = json.loads(self.rfile.read(length) or b"{}")
        except Exception:
            request = {}

        with LOCK:
            self.handle_pipeline(request)

    def handle_pipeline(self, request):
        baton = request.get("baton") or ""

        session = SESSIONS.pop(baton, None) if baton else None
        if session is None:
            if baton:
                # An unknown baton means the stream is gone.
                self.reply_json({"baton": "", "results": [
                    {"type": "error", "error": {"message": "stream expired"}}
                ]})
                return
            session = Session()

        results = []
        closed = False

        for req in request.get("requests") or []:
            kind = req.get("type")

            if kind == "close":
                closed = True
                results.append({"type": "ok", "response": {"type": "close"}})
                continue

            if kind != "execute":
                results.append({
                    "type": "error",
                    "error": {"message": "unsupported request type %r" % kind},
                })
                continue

            try:
                result = execute_stmt(session, req.get("stmt") or {})
                results.append({
                    "type": "ok",
                    "response": {"type": "execute", "result": result},
                })
            except Exception as exc:
                results.append({
                    "type": "error",
                    "error": {"message": "%s" % exc},
                })

        # The whole point of this stub: hand back a baton only while a
        # transaction is open. Otherwise the stream ends, and the Go driver
        # marks the connection dead.
        if closed or not session.in_transaction():
            session.close()
            new_baton = ""
        else:
            new_baton = uuid.uuid4().hex
            SESSIONS[new_baton] = session

        self.reply_json({"baton": new_baton, "results": results})

    def reply_json(self, payload):
        self.reply(200, json.dumps(payload).encode(), "application/json")

    def reply(self, status, body, content_type):
        self.send_response(status)
        self.send_header("Content-Type", content_type)
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)


def main():
    global DB_PATH
    port = int(sys.argv[1])
    DB_PATH = sys.argv[2]

    # Touch the file so every session opens the same database.
    sqlite3.connect(DB_PATH).close()

    ThreadingHTTPServer(("127.0.0.1", port), Handler).serve_forever()


if __name__ == "__main__":
    main()
