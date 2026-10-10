import asyncio
import time
from concurrent.futures import ThreadPoolExecutor

import pytest
from temporalio import activity
from temporalio.testing import ActivityEnvironment

from migration_worker import transforms
from migration_worker.worker import TransformActivities
from tests.helpers import child_pids


@pytest.fixture
def executor():
    pool = ThreadPoolExecutor(max_workers=4, thread_name_prefix="test-transform")
    yield pool
    pool.shutdown(wait=True, cancel_futures=True)


def _heartbeat_recorder(env: ActivityEnvironment) -> list[dict]:
    beats: list[dict] = []
    env.on_heartbeat = lambda *details: beats.append(details[0])
    return beats


def test_activity_names_match_orchestrator_contract(executor):
    acts = TransformActivities(executor)
    names = {
        activity._Definition.must_from_callable(fn).name  # noqa: SLF001
        for fn in (acts.preprocess_batch, acts.build_payload_batch, acts.render_payload_batch)
    }
    assert names == {"preprocess_batch", "build_payload_batch", "render_payload_batch"}


async def test_preprocess_batch_runs_python_steps(executor, no_leaked_children):
    acts = TransformActivities(executor)
    env = ActivityEnvironment()
    beats = _heartbeat_recorder(env)
    out = await env.run(
        acts.preprocess_batch,
        {
            "rows": [{"country": "usa"}, {"country": "can"}],
            "steps": [
                {"id": "1", "field": "country", "fn": "python", "code": "result = value[:2]"},
                {"id": "2", "field": "country", "fn": "builtin.uppercase"},
            ],
        },
    )
    assert out == [{"country": "US"}, {"country": "CA"}]
    assert beats[-1] == {"rows_processed": 2, "rows_total": 2}


async def test_heartbeats_report_progress_during_batch(executor, monkeypatch):
    def slow_render(tpl, row, *, cancel=None):
        time.sleep(0.15)
        return {"i": row["i"]}

    monkeypatch.setattr(transforms, "render_py_expressions", slow_render)
    acts = TransformActivities(executor, heartbeat_interval_s=0.05)
    env = ActivityEnvironment()
    beats = _heartbeat_recorder(env)

    payload = {"rows": [{"i": n} for n in range(3)], "payload": {}}
    out = await env.run(acts.render_payload_batch, payload)

    assert out == [{"i": 0}, {"i": 1}, {"i": 2}]
    processed = [b["rows_processed"] for b in beats]
    assert processed == sorted(processed)
    assert 0 in processed, "no heartbeat while the first row was still running"
    assert any(0 < p < 3 for p in processed)
    assert beats[-1] == {"rows_processed": 3, "rows_total": 3}


async def test_slow_sandbox_work_does_not_block_event_loop(executor, no_leaked_children):
    acts = TransformActivities(executor, heartbeat_interval_s=0.1)
    payload = {
        "rows": [{"a": 1}, {"a": 2}],
        "payload": {"v": {"$py": "row['a'] + len([i for i in range(1000000)])"}},
    }
    envs = [ActivityEnvironment() for _ in range(4)]
    beats = [_heartbeat_recorder(env) for env in envs]

    ticks: list[float] = []
    stop = asyncio.Event()

    async def ticker():
        while not stop.is_set():
            ticks.append(time.monotonic())
            await asyncio.sleep(0.02)

    ticker_task = asyncio.create_task(ticker())
    start = time.monotonic()
    results = await asyncio.gather(*(env.run(acts.render_payload_batch, payload) for env in envs))
    elapsed = time.monotonic() - start
    stop.set()
    await ticker_task

    assert results == [[{"v": 1000001}, {"v": 1000002}]] * 4
    # Blocking execution would stall the loop for roughly the whole run.
    max_gap = max(b - a for a, b in zip(ticks, ticks[1:], strict=False))
    assert max_gap < 0.25, f"event loop stalled for {max_gap:.3f}s"
    assert max_gap * 4 < elapsed, f"stall {max_gap:.3f}s vs run {elapsed:.3f}s"
    assert all(any(b["rows_processed"] < 2 for b in env_beats) for env_beats in beats)


async def test_cancellation_kills_inflight_sandbox_child(executor, no_leaked_children):
    acts = TransformActivities(executor, heartbeat_interval_s=0.05)
    env = ActivityEnvironment()
    step = {"id": "1", "field": "x", "fn": "python", "code": "while True:\n    pass"}
    task = asyncio.create_task(
        env.run(acts.preprocess_batch, {"rows": [{"x": 1}, {"x": 2}], "steps": [step]})
    )
    deadline = time.monotonic() + 5
    while not child_pids():
        assert time.monotonic() < deadline, "sandbox child never started"
        await asyncio.sleep(0.01)

    cancelled_at = time.monotonic()
    env.cancel()
    with pytest.raises(asyncio.CancelledError):
        await task
    assert time.monotonic() - cancelled_at < 1.0
    assert child_pids() == []
