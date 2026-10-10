"""Execution of user-supplied Python snippets in a short-lived child process.

Each call starts ``python -I sandbox_child.py`` with an empty environment and
no inherited file descriptors. The child applies CPU, address-space, file-size
and file-descriptor limits before running the snippet. The parent enforces a
wall-clock deadline and an output size cap, and always kills and reaps the
child before returning.

RestrictedPython (configured in :mod:`migration_worker.sandbox_child`) is a
guard layer that narrows what snippets can express; it is not an isolation
boundary. The process and its container are the boundary. Deployments hosting
mutually untrusted tenants should additionally run this worker under a
hardened runtime such as gVisor or Firecracker.

``run`` blocks the calling thread; async callers must run it in an executor.
"""
from __future__ import annotations

import contextlib
import json
import logging
import os
import selectors
import signal
import subprocess
import sys
import threading
import time
from pathlib import Path
from typing import Any

from .settings import env_float, env_int

LOGGER = logging.getLogger("migration_worker.sandbox")

DEFAULT_TIMEOUT_S = env_float("SANDBOX_TIMEOUT_S", 2.0)
DEFAULT_MAX_OUTPUT_BYTES = env_int("SANDBOX_MAX_OUTPUT_BYTES", 1024 * 1024)
CPU_SECONDS = env_int("SANDBOX_CPU_SECONDS", 2)
MEMORY_BYTES = env_int("SANDBOX_MEMORY_BYTES", 256 * 1024 * 1024)

_CHILD_SCRIPT = str(Path(__file__).with_name("sandbox_child.py"))
_POLL_INTERVAL_S = 0.05
_READ_CHUNK = 64 * 1024
_WRITE_CHUNK = 4096
_STDERR_CAP = 8 * 1024
_REAP_TIMEOUT_S = 5.0


class SandboxError(RuntimeError):
    pass


class SandboxTimeout(SandboxError):
    pass


class SandboxOutputTooLarge(SandboxError):
    pass


class SandboxCancelled(SandboxError):
    pass


def run(
    code: str,
    *,
    value: Any,
    row: dict[str, Any],
    timeout_s: float | None = None,
    max_output_bytes: int | None = None,
    cancel: threading.Event | None = None,
) -> Any:
    """Evaluate ``code`` with ``value`` and ``row`` in scope; return ``result``.

    Raises a :class:`SandboxError` subclass on user-code errors, timeout,
    oversized output or cancellation via ``cancel``.
    """
    timeout_s = DEFAULT_TIMEOUT_S if timeout_s is None else timeout_s
    max_output_bytes = DEFAULT_MAX_OUTPUT_BYTES if max_output_bytes is None else max_output_bytes
    if timeout_s <= 0:
        raise ValueError("timeout_s must be positive")
    if max_output_bytes <= 0:
        raise ValueError("max_output_bytes must be positive")
    if cancel is not None and cancel.is_set():
        raise SandboxCancelled("transform cancelled before start")

    request = json.dumps(
        {
            "code": code,
            "value": value,
            "row": row,
            "limits": {"cpu_seconds": CPU_SECONDS, "memory_bytes": MEMORY_BYTES},
        },
        default=str,
    ).encode("utf-8")

    started = time.monotonic()
    proc = subprocess.Popen(  # noqa: S603 - fixed interpreter and script path
        [sys.executable, "-I", _CHILD_SCRIPT],
        stdin=subprocess.PIPE,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        env={},
        close_fds=True,
        start_new_session=True,
    )
    try:
        stdout, stderr = _exchange(proc, request, started + timeout_s, max_output_bytes, cancel)
    except SandboxError as exc:
        LOGGER.warning(
            "sandbox child pid=%d stopped after %.3fs: %s",
            proc.pid, time.monotonic() - started, type(exc).__name__,
        )
        raise
    finally:
        _reap(proc)

    if proc.returncode != 0 or not stdout:
        detail = stderr.decode("utf-8", errors="replace").strip()[-500:]
        raise SandboxError(
            f"transform process exited with status {proc.returncode} without a result"
            + (f": {detail}" if detail else "")
        )
    try:
        payload = json.loads(stdout)
    except ValueError as exc:
        raise SandboxError("transform process returned malformed output") from exc
    if not payload.get("ok"):
        raise SandboxError(payload.get("error", "unknown transform error"))
    return payload.get("value")


def _exchange(
    proc: subprocess.Popen[bytes],
    request: bytes,
    deadline: float,
    max_output_bytes: int,
    cancel: threading.Event | None,
) -> tuple[bytes, bytes]:
    """Write ``request`` and drain both output pipes concurrently until EOF."""
    assert proc.stdin and proc.stdout and proc.stderr
    stdout = bytearray()
    stderr = bytearray()
    pending = memoryview(request)
    os.set_blocking(proc.stdin.fileno(), False)

    with selectors.DefaultSelector() as selector:
        selector.register(proc.stdin, selectors.EVENT_WRITE)
        selector.register(proc.stdout, selectors.EVENT_READ)
        selector.register(proc.stderr, selectors.EVENT_READ)
        while selector.get_map():
            _check_budget(deadline, cancel)
            remaining = deadline - time.monotonic()
            for key, _ in selector.select(timeout=min(remaining, _POLL_INTERVAL_S)):
                stream = key.fileobj
                if stream is proc.stdin:
                    pending = _write_chunk(proc.stdin, pending)
                    done = not pending
                else:
                    sink = stdout if stream is proc.stdout else stderr
                    done = _read_chunk(stream, sink, stderr_sink=sink is stderr)
                    if len(stdout) > max_output_bytes:
                        raise SandboxOutputTooLarge(
                            f"transform result exceeded {max_output_bytes} bytes"
                        )
                if done:
                    selector.unregister(stream)
                    _close_quietly(stream)

    while proc.poll() is None:
        _check_budget(deadline, cancel)
        with contextlib.suppress(subprocess.TimeoutExpired):
            proc.wait(timeout=min(deadline - time.monotonic(), _POLL_INTERVAL_S))
    return bytes(stdout), bytes(stderr)


def _write_chunk(stream: Any, pending: memoryview) -> memoryview:
    """Write what the pipe accepts; a child that stopped reading drops the rest."""
    try:
        written = os.write(stream.fileno(), pending[:_WRITE_CHUNK])
    except BlockingIOError:
        return pending
    except BrokenPipeError:
        return pending[len(pending):]
    return pending[written:]


def _read_chunk(stream: Any, sink: bytearray, *, stderr_sink: bool) -> bool:
    """Append available bytes to ``sink``; return True at EOF.

    stderr is only diagnostic, so it is truncated rather than failing the call.
    """
    chunk = os.read(stream.fileno(), _READ_CHUNK)
    if not chunk:
        return True
    if stderr_sink:
        sink += chunk[: max(0, _STDERR_CAP - len(sink))]
    else:
        sink += chunk
    return False


def _check_budget(deadline: float, cancel: threading.Event | None) -> None:
    if cancel is not None and cancel.is_set():
        raise SandboxCancelled("transform cancelled")
    if time.monotonic() >= deadline:
        raise SandboxTimeout("transform exceeded its wall-clock budget")


def _reap(proc: subprocess.Popen[bytes]) -> None:
    """Kill the child's process group if still running, wait, close pipes."""
    if proc.poll() is None:
        with contextlib.suppress(ProcessLookupError):
            os.killpg(proc.pid, signal.SIGKILL)
        try:
            proc.wait(timeout=_REAP_TIMEOUT_S)
        except subprocess.TimeoutExpired:
            LOGGER.error("sandbox child pid=%d did not exit after SIGKILL", proc.pid)
    for stream in (proc.stdin, proc.stdout, proc.stderr):
        if stream is not None:
            _close_quietly(stream)


def _close_quietly(stream: Any) -> None:
    with contextlib.suppress(OSError):
        stream.close()
