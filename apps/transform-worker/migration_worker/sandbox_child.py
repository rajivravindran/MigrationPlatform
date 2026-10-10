"""Child-process side of :mod:`migration_worker.sandbox`.

The parent starts this file as ``python -I sandbox_child.py``, writes one JSON
request to stdin and reads one JSON response from stdout. Isolated mode keeps
the package directory off ``sys.path``, so this module must import only the
standard library and RestrictedPython.

RestrictedPython is a guard layer, not an isolation boundary: it rejects
obviously dangerous syntax at compile time and routes attribute access,
subscripting, iteration and writes through the guard functions below. The
isolation boundary is the short-lived child process (resource limits, empty
environment, no inherited file descriptors) and the container it runs in.

Guard policy
------------
``_getattr_``
    ``safer_getattr`` rules (no ``_``-prefixed names, no frame/code inspection
    attributes, no ``str.format``), plus: module objects are never returned or
    traversed, and missing attributes raise ``AttributeError`` instead of
    silently evaluating to ``None``.
``_getitem_``
    Indexing and slicing only on plain data containers (dict, list, tuple, str,
    range, ``re.Match``). Dict keys are row data, so ``row['_id']`` is allowed;
    every other object type is refused outright.
``_getiter_`` / ``_unpack_sequence_`` / ``_iter_unpack_sequence_``
    Plain ``iter`` plus RestrictedPython's guarded unpacking. Iteration only
    yields values that are already reachable from the iterated object.
``_write_``
    ``full_write_guard``: item/attribute writes are allowed on dict and list
    values the snippet owns and refused on every other object.
``_inplacevar_``
    Augmented assignment to local names for the standard arithmetic and
    bitwise operators.
"""
from __future__ import annotations

import datetime
import json
import operator
import re
import resource
import sys
import types
from typing import Any

from RestrictedPython import compile_restricted_exec
from RestrictedPython.Guards import (
    full_write_guard,
    guarded_iter_unpack_sequence,
    guarded_unpack_sequence,
    safe_builtins,
    safer_getattr,
)

SUBSCRIPTABLE_TYPES = frozenset({dict, list, tuple, str, range, re.Match})

_MISSING = object()

_INPLACE_OPERATORS = {
    "+=": operator.iadd,
    "-=": operator.isub,
    "*=": operator.imul,
    "/=": operator.itruediv,
    "//=": operator.ifloordiv,
    "%=": operator.imod,
    "**=": operator.ipow,
    "<<=": operator.ilshift,
    ">>=": operator.irshift,
    "&=": operator.iand,
    "|=": operator.ior,
    "^=": operator.ixor,
}


def guarded_getattr(obj: Any, name: str) -> Any:
    if isinstance(obj, types.ModuleType):
        raise AttributeError("attribute access on modules is not allowed")
    value = safer_getattr(obj, name, _MISSING)
    if value is _MISSING:
        raise AttributeError(f"{type(obj).__name__!r} object has no attribute {name!r}")
    if isinstance(value, types.ModuleType):
        raise AttributeError(f"{name!r} refers to a module, which is not allowed")
    return value


def guarded_getitem(obj: Any, key: Any) -> Any:
    if type(obj) not in SUBSCRIPTABLE_TYPES:
        raise TypeError(f"subscript access is not allowed on {type(obj).__name__!r} objects")
    return obj[key]


def guarded_inplacevar(op: str, target: Any, operand: Any) -> Any:
    func = _INPLACE_OPERATORS.get(op)
    if func is None:
        raise TypeError(f"augmented assignment {op!r} is not allowed")
    return func(target, operand)


def _namespace(module: types.ModuleType, names: tuple[str, ...]) -> types.SimpleNamespace:
    return types.SimpleNamespace(**{name: getattr(module, name) for name in names})


# Real modules transitively expose others (re -> enum -> builtins), so user
# code only ever sees curated namespaces of the members it needs.
RE_NAMESPACE = _namespace(
    re,
    (
        "compile", "escape", "findall", "finditer", "fullmatch", "match", "search",
        "split", "sub", "subn",
        "A", "ASCII", "I", "IGNORECASE", "M", "MULTILINE", "S", "DOTALL", "X", "VERBOSE",
    ),
)
DATETIME_NAMESPACE = _namespace(
    datetime, ("date", "datetime", "time", "timedelta", "timezone", "MINYEAR", "MAXYEAR")
)

_REMOVED_BUILTINS = ("__build_class__", "_getattr_", "setattr", "delattr")
_ADDED_BUILTINS = {
    "all": all,
    "any": any,
    "dict": dict,
    "enumerate": enumerate,
    "list": list,
    "max": max,
    "min": min,
    "reversed": reversed,
    "set": set,
    "sum": sum,
}
BUILTINS = {
    **{k: v for k, v in safe_builtins.items() if k not in _REMOVED_BUILTINS},
    **_ADDED_BUILTINS,
}


def build_globals() -> dict[str, Any]:
    return {
        "__builtins__": BUILTINS,
        "_getattr_": guarded_getattr,
        "_getitem_": guarded_getitem,
        "_getiter_": iter,
        "_iter_unpack_sequence_": guarded_iter_unpack_sequence,
        "_unpack_sequence_": guarded_unpack_sequence,
        "_write_": full_write_guard,
        "_inplacevar_": guarded_inplacevar,
        "re": RE_NAMESPACE,
        "datetime": DATETIME_NAMESPACE,
    }


def execute(code: str, value: Any, row: dict[str, Any]) -> dict[str, Any]:
    """Compile and run ``code`` under the guard policy; never raises."""
    try:
        compiled = compile_restricted_exec(code)
        if compiled.errors:
            return {"ok": False, "error": "SyntaxError: " + "; ".join(compiled.errors)}
        # A single namespace keeps snippet-level names visible inside
        # generator expressions and lambdas.
        env = build_globals()
        env.update({"value": value, "row": row, "result": None})
        exec(compiled.code, env)  # noqa: S102
        return {"ok": True, "value": env.get("result")}
    except BaseException as exc:  # noqa: BLE001 - every failure is reported to the parent
        return _error(exc)


def encode_response(response: dict[str, Any]) -> bytes:
    try:
        return json.dumps(response, default=str).encode("utf-8")
    except Exception as exc:  # noqa: BLE001 - e.g. circular or too deeply nested results
        return json.dumps(_error(exc)).encode("utf-8")


def _error(exc: BaseException) -> dict[str, Any]:
    return {"ok": False, "error": f"{type(exc).__name__}: {exc}"}


def apply_limits(limits: dict[str, int]) -> None:
    cpu = int(limits["cpu_seconds"])
    memory = int(limits["memory_bytes"])
    resource.setrlimit(resource.RLIMIT_CPU, (cpu, cpu))
    resource.setrlimit(resource.RLIMIT_AS, (memory, memory))
    resource.setrlimit(resource.RLIMIT_NOFILE, (16, 16))
    resource.setrlimit(resource.RLIMIT_FSIZE, (0, 0))
    resource.setrlimit(resource.RLIMIT_CORE, (0, 0))


def main() -> int:
    request = json.loads(sys.stdin.buffer.read())
    apply_limits(request["limits"])
    response = execute(request["code"], request["value"], request["row"])
    sys.stdout.buffer.write(encode_response(response))
    sys.stdout.buffer.flush()
    return 0


if __name__ == "__main__":
    sys.exit(main())
