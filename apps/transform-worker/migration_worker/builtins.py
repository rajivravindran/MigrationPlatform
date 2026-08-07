"""Builtin transforms available to rule templates via `fn: "builtin.<name>"`.

Every builtin is:
    * pure (no IO, no randomness unless documented)
    * total (returns a deterministic value or raises ``TransformError``)
    * safe to call millions of times in a hot loop (minimal allocations)

Add new builtins to ``REGISTRY`` so they become dispatchable from the
orchestrator.
"""
from __future__ import annotations

import hashlib
import re
from collections.abc import Callable, Mapping
from datetime import datetime
from typing import Any


class TransformError(ValueError):
    """Raised when a builtin transform cannot produce a value."""


BuiltinFn = Callable[[Any, Mapping[str, Any]], Any]


def _as_str(value: Any) -> str:
    if value is None:
        return ""
    return value if isinstance(value, str) else str(value)


def lowercase(value: Any, _args: Mapping[str, Any]) -> str:
    return _as_str(value).lower()


def uppercase(value: Any, _args: Mapping[str, Any]) -> str:
    return _as_str(value).upper()


def trim(value: Any, args: Mapping[str, Any]) -> str:
    chars = args.get("chars")
    s = _as_str(value)
    return s.strip(chars) if chars else s.strip()


def regex_replace(value: Any, args: Mapping[str, Any]) -> str:
    pattern = args.get("pattern")
    repl = args.get("replacement", "")
    if not pattern:
        raise TransformError("regex_replace requires 'pattern'")
    flags = 0
    if args.get("ignore_case"):
        flags |= re.IGNORECASE
    return re.sub(pattern, repl, _as_str(value), flags=flags)


def date_format(value: Any, args: Mapping[str, Any]) -> str:
    fmt_in = args.get("input")
    fmt_out = args.get("output", "%Y-%m-%d")
    s = _as_str(value).strip()
    if not s:
        return ""
    if fmt_in:
        parsed = datetime.strptime(s, fmt_in)
    else:
        parsed = datetime.fromisoformat(s)
    return parsed.strftime(fmt_out)


def lookup(value: Any, args: Mapping[str, Any]) -> Any:
    table = args.get("table") or {}
    default = args.get("default")
    return table.get(_as_str(value), default)


def map_lookup(value: Any, args: Mapping[str, Any]) -> Any:
    return lookup(value, args)


def concat(value: Any, args: Mapping[str, Any]) -> str:
    parts = args.get("values", [])
    separator = args.get("separator", "")
    head = _as_str(value) if args.get("include_value") else ""
    extras = [_as_str(p) for p in parts]
    return separator.join([head, *extras]) if head else separator.join(extras)


def split(value: Any, args: Mapping[str, Any]) -> list[str]:
    sep = args.get("separator", ",")
    maxsplit = int(args.get("maxsplit", -1))
    return _as_str(value).split(sep, maxsplit)


def default(value: Any, args: Mapping[str, Any]) -> Any:
    if value is None or value == "":
        return args.get("value")
    return value


def coerce(value: Any, args: Mapping[str, Any]) -> Any:
    target = args.get("to", "string")
    if value is None:
        return None
    try:
        if target == "string":
            return _as_str(value)
        if target == "integer":
            return int(value)
        if target == "number":
            return float(value)
        if target == "boolean":
            if isinstance(value, bool):
                return value
            return _as_str(value).strip().lower() in {"1", "true", "yes", "y"}
    except (TypeError, ValueError) as exc:
        raise TransformError(f"cannot coerce {value!r} to {target}") from exc
    raise TransformError(f"unknown target type {target!r}")


def null_if(value: Any, args: Mapping[str, Any]) -> Any:
    sentinel = args.get("equals")
    if _as_str(value) == _as_str(sentinel):
        return None
    return value


def hash_fn(value: Any, args: Mapping[str, Any]) -> str:
    algo = args.get("algorithm", "sha256")
    if algo not in {"md5", "sha1", "sha256", "sha512"}:
        raise TransformError(f"unsupported hash algorithm {algo}")
    hasher = hashlib.new(algo)
    hasher.update(_as_str(value).encode("utf-8"))
    return hasher.hexdigest()


def substring(value: Any, args: Mapping[str, Any]) -> str:
    start = int(args.get("start", 0))
    end = args.get("end")
    s = _as_str(value)
    return s[start:end] if end is not None else s[start:]


REGISTRY: dict[str, BuiltinFn] = {
    "builtin.lowercase": lowercase,
    "builtin.uppercase": uppercase,
    "builtin.trim": trim,
    "builtin.regex_replace": regex_replace,
    "builtin.date_format": date_format,
    "builtin.lookup": lookup,
    "builtin.map_lookup": map_lookup,
    "builtin.concat": concat,
    "builtin.split": split,
    "builtin.default": default,
    "builtin.coerce": coerce,
    "builtin.null_if": null_if,
    "builtin.hash": hash_fn,
    "builtin.substring": substring,
}


def apply(fn_name: str, value: Any, args: Mapping[str, Any] | None = None) -> Any:
    try:
        fn = REGISTRY[fn_name]
    except KeyError as exc:
        raise TransformError(f"unknown builtin {fn_name!r}") from exc
    return fn(value, args or {})
