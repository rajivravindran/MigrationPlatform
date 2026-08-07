"""Core transform logic - preprocess rows, render payload templates."""
from __future__ import annotations

import copy
from collections.abc import Iterable
from typing import Any

from . import builtins, sandbox
from .template import PreprocessStep, RuleTemplate


def preprocess_row(row: dict[str, Any], steps: Iterable[PreprocessStep]) -> dict[str, Any]:
    out = copy.deepcopy(row)
    for step in steps:
        value = out.get(step.field)
        if step.fn == "python":
            if not step.code:
                continue
            out[step.field] = sandbox.run(step.code, value=value, row=out)
        elif step.fn.startswith("builtin."):
            out[step.field] = builtins.apply(step.fn, value, step.args or {})
        else:
            raise ValueError(f"unknown preprocess fn {step.fn!r}")
    return out


def build_payload(row: dict[str, Any], template_dict: dict[str, Any]) -> dict[str, Any]:
    payload_template = template_dict.get("mapping", {}).get("payload", {})
    return _render(payload_template, row)


def _render(template: Any, row: dict[str, Any]) -> Any:
    if isinstance(template, dict):
        if "$from" in template:
            field = template["$from"]
            return row.get(field)
        if "$py" in template:
            code = f"result = {template['$py']}"
            return sandbox.run(code, value=None, row=row)
        if "$const" in template:
            return template["$const"]
        return {k: _render(v, row) for k, v in template.items()}
    if isinstance(template, list):
        return [_render(v, row) for v in template]
    return template


def render_py_expressions(payload: Any, row: dict[str, Any]) -> Any:
    """Evaluate only the remaining ``$py`` sigils in an already-rendered payload.

    The Go workflow resolves ``$from``/``$fromResponse``/``$literal`` itself and
    ships the partially-rendered payload here so the sandbox tier only runs
    Python expressions (with the preprocessed row in scope).
    """
    if isinstance(payload, dict):
        if "$py" in payload:
            code = f"result = {payload['$py']}"
            return sandbox.run(code, value=None, row=row)
        return {k: render_py_expressions(v, row) for k, v in payload.items()}
    if isinstance(payload, list):
        return [render_py_expressions(v, row) for v in payload]
    return payload


def apply_batch(rows: list[dict[str, Any]], template: RuleTemplate) -> list[dict[str, Any]]:
    out = []
    for row in rows:
        pre = preprocess_row(row, template.preprocess)
        payload = build_payload(pre, template.model_dump(by_alias=True))
        out.append(payload)
    return out
