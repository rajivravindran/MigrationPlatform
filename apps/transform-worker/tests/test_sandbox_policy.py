"""Guard-policy tests run in-process against the same code the child uses."""
import os
import types

import pytest

from migration_worker.sandbox_child import (
    execute,
    guarded_getattr,
    guarded_getitem,
    guarded_inplacevar,
)

ROW = {
    "a": 1,
    "b": 2,
    "country": "usa",
    "l": [1, 2, 3, 4],
    "nested": {"k": ["first", "second"]},
    "d": {"y": 2, "x": 1},
    "s": "order-1234",
    "_id": "abc",
}


@pytest.mark.parametrize(
    ("code", "expected"),
    [
        ("result = row['a'] + row['b']", 3),
        ("result = row['country'][:2].upper()", "US"),
        ("result = row['l'][-1]", 4),
        ("result = row['l'][::2]", [1, 3]),
        ("result = row['l'][1:3]", [2, 3]),
        ("result = row['nested']['k'][0]", "first"),
        ("result = (1, 2, 3)[1]", 2),
        ("result = row['_id']", "abc"),
        ("result = list(range(10)[2:4])", [2, 3]),
        ("result = re.search(r'(\\d+)', row['s'])[1]", "1234"),
        ("result = [x * 2 for x in row['l']]", [2, 4, 6, 8]),
        ("result = {k: v for k, v in row['d'].items()}", {"y": 2, "x": 1}),
        ("f = 2\nresult = sum(x * f for x in row['l'])", 20),
        ("t = 0\nfor x in row['l']:\n    t += x\nresult = t", 10),
        ("a, b = row['l'][:2]\nresult = [b, a]", [2, 1]),
        ("result = [k for k, v in row['d'].items() if v > 1]", ["y"]),
        ("d = {}\nd['x'] = 1\nl = []\nl.append(2)\nl[0] = 5\nresult = [d, l]", [{"x": 1}, [5]]),
        ("result = ', '.join(sorted(row['d']))", "x, y"),
        ("result = datetime.date(2020, 1, 2).isoformat()", "2020-01-02"),
        (
            "result = [min(row['l']), max(row['l']), any(row['l']), all(row['l'])]",
            [1, 4, True, True],
        ),
        ("result = [i for i, _ in enumerate(row['l'])]", [0, 1, 2, 3]),
        ("result = row.get('missing', 'n/a')", "n/a"),
    ],
)
def test_supported_expressions(code, expected):
    out = execute(code, None, ROW)
    assert out == {"ok": True, "value": expected}


def test_value_methods():
    assert execute("result = value.strip().lower()", "  Foo ", {})["value"] == "foo"


@pytest.mark.parametrize(
    ("code", "error_prefix"),
    [
        ("import os", "ImportError"),
        ("result = __import__('os')", "SyntaxError"),
        ("result = open('/etc/passwd').read()", "NameError"),
        ("result = exec('x = 1')", "SyntaxError"),
        ("result = eval('1')", "SyntaxError"),
        ("result = (1).__class__", "SyntaxError"),
        ("result = ().__class__.__bases__[0].__subclasses__()", "SyntaxError"),
        ("result = row.__getitem__", "SyntaxError"),
        ("result = re.compile.__globals__", "SyntaxError"),
        ("result = (x for x in [1]).gi_frame", "SyntaxError"),
        ("result = getattr(row, '__class__')", "NameError"),
        ("result = type(row)", "NameError"),
        ("result = globals()", "NameError"),
        ("result = vars()", "NameError"),
        ("setattr(re, 'x', 1)", "NameError"),
        ("print('x')", "NameError"),
        ("class A:\n    pass", "NameError"),
        ("result = value['__class__']", "TypeError"),
        ("result = re['compile']", "TypeError"),
        ("result = '{0.__class__}'.format(row)", "NotImplementedError"),
        ("result = '{x.__class__}'.format_map({'x': row})", "NotImplementedError"),
        ("result = str.format('{0.__class__}', 1)", "NotImplementedError"),
        ("result = re.enum.bltns.open('/etc/passwd')", "AttributeError"),
        ("result = datetime.sys", "AttributeError"),
        ("re.compile = None", "TypeError"),
        ("result = value.no_such_attribute", "AttributeError"),
    ],
)
def test_forbidden_operations(code, error_prefix):
    out = execute(code, 1, ROW)
    assert out["ok"] is False
    assert out["error"].startswith(error_prefix), out["error"]


def test_dunder_dict_key_is_plain_data():
    # Dict keys are row data; a dunder key is a lookup, not an attribute escape.
    out = execute("result = row['__class__']", None, {"a": 1})
    assert out["ok"] is False and out["error"].startswith("KeyError")


def test_getattr_never_exposes_modules():
    with pytest.raises(AttributeError):
        guarded_getattr(os, "path")
    with pytest.raises(AttributeError):
        guarded_getattr(types.SimpleNamespace(m=os), "m")


def test_getitem_refuses_non_container_types():
    with pytest.raises(TypeError):
        guarded_getitem(os.environ, "PATH")
    assert guarded_getitem({"k": 1}, "k") == 1


def test_inplacevar_rejects_unknown_operator():
    with pytest.raises(TypeError):
        guarded_inplacevar("=>", 1, 2)
    assert guarded_inplacevar("+=", 1, 2) == 3
