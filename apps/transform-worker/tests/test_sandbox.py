import pytest

from migration_worker import sandbox


def test_sandbox_runs_simple_expr():
    out = sandbox.run("result = value.upper()", value="abc", row={})
    assert out == "ABC"


def test_sandbox_row_access():
    out = sandbox.run("result = row['a'] + row['b']", value=None, row={"a": 1, "b": 2})
    assert out == 3


def test_sandbox_blocks_dunder_import():
    # Import statements are stripped at compile time by RestrictedPython.
    with pytest.raises(sandbox.SandboxError):
        sandbox.run("import os\nresult = 1", value=None, row={})


def test_sandbox_blocks_attribute_access():
    with pytest.raises(sandbox.SandboxError):
        sandbox.run("result = (1).__class__", value=None, row={})


def test_sandbox_times_out_infinite_loop():
    with pytest.raises(sandbox.SandboxError):
        sandbox.run("while True: pass", value=None, row={}, timeout_s=1.0)


def test_sandbox_blocks_file_read():
    with pytest.raises(sandbox.SandboxError):
        sandbox.run("result = open('/etc/passwd').read()", value=None, row={})
