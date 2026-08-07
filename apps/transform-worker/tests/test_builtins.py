import pytest

from migration_worker.builtins import TransformError, apply


def test_case_transforms():
    assert apply("builtin.lowercase", "ABC") == "abc"
    assert apply("builtin.uppercase", "abc") == "ABC"


def test_trim_with_chars():
    assert apply("builtin.trim", "***hi***", {"chars": "*"}) == "hi"
    assert apply("builtin.trim", "  hi  ", {}) == "hi"


def test_regex_replace_flags():
    assert apply(
        "builtin.regex_replace", "HELLO", {"pattern": "L", "replacement": "x", "ignore_case": True}
    ) == "HExxO"


def test_date_format_iso():
    assert apply("builtin.date_format", "2025-01-02T03:04:05", {"output": "%Y/%m"}) == "2025/01"


def test_coerce_number_and_boolean():
    assert apply("builtin.coerce", "42", {"to": "integer"}) == 42
    assert apply("builtin.coerce", "true", {"to": "boolean"}) is True


def test_hash_produces_hex():
    out = apply("builtin.hash", "abc", {"algorithm": "sha256"})
    assert len(out) == 64 and all(c in "0123456789abcdef" for c in out)


def test_unknown_fn():
    with pytest.raises(TransformError):
        apply("builtin.bogus", "x")
