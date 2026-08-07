"""Sandboxed execution of user-supplied Python snippets.

Two layers of isolation:

1. ``RestrictedPython`` compiles the source under a safe AST transformer that
   rejects imports, attribute access to dunder names, exec/eval, and so on.
2. The compiled code runs inside a ``multiprocessing.Process`` subprocess that
   applies ``resource.setrlimit`` (CPU, memory, file descriptor) and a hard
   wall-clock timeout. The parent process only reads the JSON-encoded result
   back through a pipe, so even if sandbox escape succeeded the blast radius
   is confined to a short-lived child.

This is a best-effort defense. Deployments that host fully-untrusted tenants
should run the transform-worker in a separate Kubernetes namespace with gVisor
or Firecracker based runtimes layered on top.
"""
from __future__ import annotations

import json
import multiprocessing as mp
import resource
import time
from typing import Any

from RestrictedPython import compile_restricted_exec
from RestrictedPython.Guards import safe_builtins, safe_globals


class SandboxError(RuntimeError):
    pass


SAFE_GLOBALS = {
    "__builtins__": safe_builtins,
    **safe_globals,
}
# We expose a few commonly-useful pure modules by binding explicitly. We do
# NOT allow full module import from user code.
import datetime as _datetime  # noqa: E402
import re as _re  # noqa: E402

SAFE_GLOBALS["datetime"] = _datetime
SAFE_GLOBALS["re"] = _re


def _worker(code: str, row_data: dict[str, Any], value: Any, conn: mp.connection.Connection) -> None:
    try:
        # Limit CPU seconds and address space so a runaway script cannot wedge
        # the worker. A 2s CPU budget and 256MB of RSS is ample for per-row
        # transforms while still bounding abuse.
        resource.setrlimit(resource.RLIMIT_CPU, (2, 2))
        resource.setrlimit(resource.RLIMIT_AS, (256 * 1024 * 1024, 256 * 1024 * 1024))
        resource.setrlimit(resource.RLIMIT_NOFILE, (32, 32))

        compiled = compile_restricted_exec(code)
        if compiled.errors:
            raise SandboxError("; ".join(compiled.errors))
        local_env: dict[str, Any] = {"value": value, "row": row_data, "result": None}
        exec(compiled.code, SAFE_GLOBALS, local_env)  # noqa: S102
        payload = {"ok": True, "value": local_env.get("result")}
    except BaseException as exc:  # noqa: BLE001
        payload = {"ok": False, "error": f"{type(exc).__name__}: {exc}"}
    try:
        conn.send(json.dumps(payload, default=str))
    finally:
        conn.close()


def run(code: str, *, value: Any, row: dict[str, Any], timeout_s: float = 2.0) -> Any:
    parent_conn, child_conn = mp.Pipe(duplex=False)
    ctx = mp.get_context("spawn")
    proc = ctx.Process(target=_worker, args=(code, row, value, child_conn), daemon=True)
    proc.start()
    child_conn.close()

    deadline = time.monotonic() + timeout_s
    while proc.is_alive() and time.monotonic() < deadline:
        proc.join(0.05)
    if proc.is_alive():
        proc.terminate()
        proc.join(1)
        raise SandboxError(f"transform exceeded {timeout_s}s wall-clock budget")

    if not parent_conn.poll(0.1):
        raise SandboxError("transform exited without producing a result")
    raw = parent_conn.recv()
    parent_conn.close()
    payload = json.loads(raw)
    if not payload["ok"]:
        raise SandboxError(payload["error"])
    return payload["value"]
