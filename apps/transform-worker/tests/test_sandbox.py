import contextlib
import os
import threading
import time

import pytest

from migration_worker import sandbox
from tests.helpers import wait_for


def test_sandbox_runs_simple_expr():
    out = sandbox.run("result = value.upper()", value="abc", row={})
    assert out == "ABC"


def test_sandbox_row_access():
    out = sandbox.run("result = row['a'] + row['b']", value=None, row={"a": 1, "b": 2})
    assert out == 3


def test_sandbox_indexing_slicing_and_iteration(no_leaked_children):
    code = "result = [row['country'][:2].upper(), [x * 2 for x in row['l'][1:]]]"
    out = sandbox.run(code, value=None, row={"country": "usa", "l": [1, 2, 3]})
    assert out == ["US", [4, 6]]


def test_sandbox_blocks_dunder_import():
    # Import statements compile, but no __import__ builtin is provided.
    with pytest.raises(sandbox.SandboxError, match="ImportError"):
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


def test_sandbox_blocks_module_traversal_to_builtins():
    with pytest.raises(sandbox.SandboxError, match="AttributeError"):
        sandbox.run("result = re.enum.bltns.open('/etc/passwd').read()", value=None, row={})


def test_timeout_kills_child(no_leaked_children):
    start = time.monotonic()
    with pytest.raises(sandbox.SandboxTimeout):
        sandbox.run("while True: pass", value=None, row={}, timeout_s=0.5)
    assert time.monotonic() - start < 2.0
    assert no_leaked_children() == []


def test_user_exception_leaves_no_child(no_leaked_children):
    with pytest.raises(sandbox.SandboxError, match="ZeroDivisionError"):
        sandbox.run("result = 1 / 0", value=None, row={})
    assert no_leaked_children() == []


def test_result_larger_than_pipe_capacity_is_returned(no_leaked_children):
    out = sandbox.run("result = 'x' * 300000", value=None, row={})
    assert out == "x" * 300000


def test_oversized_result_fails_with_size_error(no_leaked_children):
    with pytest.raises(sandbox.SandboxOutputTooLarge):
        sandbox.run("result = 'x' * 200000", value=None, row={}, max_output_bytes=100_000)
    assert no_leaked_children() == []


def test_cancel_kills_running_child(no_leaked_children):
    cancel = threading.Event()
    outcome: list[BaseException] = []

    def target():
        try:
            sandbox.run("while True: pass", value=None, row={}, timeout_s=10, cancel=cancel)
        except BaseException as exc:  # noqa: BLE001
            outcome.append(exc)

    thread = threading.Thread(target=target)
    thread.start()
    wait_for(lambda: no_leaked_children() != [])
    cancelled_at = time.monotonic()
    cancel.set()
    thread.join(timeout=3)
    assert not thread.is_alive()
    assert time.monotonic() - cancelled_at < 1.0
    assert len(outcome) == 1 and isinstance(outcome[0], sandbox.SandboxCancelled)
    assert no_leaked_children() == []


def test_already_cancelled_does_not_spawn(no_leaked_children):
    cancel = threading.Event()
    cancel.set()
    with pytest.raises(sandbox.SandboxCancelled):
        sandbox.run("result = 1", value=None, row={}, cancel=cancel)


def test_child_exit_without_result(monkeypatch, tmp_path, no_leaked_children):
    script = tmp_path / "exits.py"
    script.write_text("import sys\nsys.stderr.write('boom')\nsys.exit(3)\n")
    monkeypatch.setattr(sandbox, "_CHILD_SCRIPT", str(script))
    with pytest.raises(sandbox.SandboxError, match="status 3 without a result: boom"):
        sandbox.run("result = 1", value=None, row={})


def test_child_malformed_output(monkeypatch, tmp_path, no_leaked_children):
    script = tmp_path / "garbage.py"
    script.write_text("import sys\nsys.stdin.read()\nsys.stdout.write('not json')\n")
    monkeypatch.setattr(sandbox, "_CHILD_SCRIPT", str(script))
    with pytest.raises(sandbox.SandboxError, match="malformed output"):
        sandbox.run("result = 1", value=None, row={})


def test_failure_paths_do_not_leak_file_descriptors(no_leaked_children):
    def open_fds() -> int:
        return len(os.listdir("/proc/self/fd"))

    sandbox.run("result = 1", value=None, row={})
    before = open_fds()
    for code, kwargs in [
        ("result = 1", {}),
        ("result = 1 / 0", {}),
        ("while True: pass", {"timeout_s": 0.3}),
        ("result = 'x' * 200000", {"max_output_bytes": 1000}),
    ]:
        with contextlib.suppress(sandbox.SandboxError):
            sandbox.run(code, value=None, row={}, **kwargs)
    assert open_fds() == before


def test_rejects_non_positive_limits():
    with pytest.raises(ValueError):
        sandbox.run("result = 1", value=None, row={}, timeout_s=0)
    with pytest.raises(ValueError):
        sandbox.run("result = 1", value=None, row={}, max_output_bytes=0)
