"""Temporal worker hosting the transformation activities.

The Go orchestrator dispatches ``preprocess_batch`` and ``build_payload_batch``
activities onto the ``transforms`` task queue whenever it encounters Python
transformations. Keeping these in a dedicated worker lets us scale the Python
sandbox tier independently of the Go workflow/IO tier.

Activities are async so heartbeats and cancellation are handled on the event
loop, while each row's blocking work (sandbox subprocesses, CPU transforms)
runs on a bounded thread pool. One row per activity is in flight at a time, so
pending executor work is bounded by ``ACTIVITY_CONCURRENCY``.
"""
from __future__ import annotations

import asyncio
import logging
import os
import threading
import time
from collections.abc import Callable
from concurrent.futures import Executor, ThreadPoolExecutor
from typing import Any

from prometheus_client import Counter, Histogram, start_http_server
from temporalio import activity, workflow
from temporalio.client import Client
from temporalio.worker import Worker

from . import transforms
from .settings import env_float, env_int
from .template import PreprocessStep, RuleTemplate

TASK_QUEUE = "transforms"
LOGGER = logging.getLogger("migration_worker")

PREPROCESS_LAT = Histogram("preprocess_batch_seconds", "Latency of preprocess_batch activity")
PAYLOAD_LAT = Histogram("payload_batch_seconds", "Latency of build_payload_batch activity")
ROWS_PROCESSED = Counter("transform_rows_processed_total", "Rows processed through transforms")

# Long enough for the sandbox to notice cancellation and reap its child.
_CANCEL_GRACE_S = 5.0

RowFn = Callable[[dict[str, Any], threading.Event], Any]


class TransformActivities:
    def __init__(self, executor: Executor, *, heartbeat_interval_s: float = 5.0) -> None:
        if heartbeat_interval_s <= 0:
            raise ValueError("heartbeat_interval_s must be positive")
        self._executor = executor
        self._heartbeat_interval_s = heartbeat_interval_s

    @activity.defn(name="preprocess_batch")
    async def preprocess_batch(self, payload: dict[str, Any]) -> list[dict[str, Any]]:
        steps = [PreprocessStep(**s) for s in payload["steps"]]
        start = time.perf_counter()
        try:
            return await self._run_rows(
                payload["rows"],
                lambda row, cancel: transforms.preprocess_row(row, steps, cancel=cancel),
                on_row_done=ROWS_PROCESSED.inc,
            )
        finally:
            PREPROCESS_LAT.observe(time.perf_counter() - start)

    @activity.defn(name="build_payload_batch")
    async def build_payload_batch(self, payload: dict[str, Any]) -> list[dict[str, Any]]:
        template = RuleTemplate.model_validate(payload["template"]).model_dump(by_alias=True)
        start = time.perf_counter()
        try:
            return await self._run_rows(
                payload["rows"],
                lambda row, cancel: transforms.build_payload(row, template, cancel=cancel),
            )
        finally:
            PAYLOAD_LAT.observe(time.perf_counter() - start)

    @activity.defn(name="render_payload_batch")
    async def render_payload_batch(self, payload: dict[str, Any]) -> list[dict[str, Any]]:
        """Evaluate the remaining $py expressions in a partially-rendered payload.

        Input: {"rows": [row, ...], "payload": partially-rendered payload template}.
        Output: one fully-rendered payload per row.
        """
        tpl = payload["payload"]
        start = time.perf_counter()
        try:
            return await self._run_rows(
                payload["rows"],
                lambda row, cancel: transforms.render_py_expressions(tpl, row, cancel=cancel),
            )
        finally:
            PAYLOAD_LAT.observe(time.perf_counter() - start)

    async def _run_rows(
        self,
        rows: list[dict[str, Any]],
        fn: RowFn,
        *,
        on_row_done: Callable[[], None] | None = None,
    ) -> list[Any]:
        """Apply ``fn`` to each row on the executor, heartbeating progress.

        Heartbeat details are ``{"rows_processed": n, "rows_total": total}``.
        On cancellation the in-flight row's sandbox child is killed before the
        ``CancelledError`` propagates.
        """
        loop = asyncio.get_running_loop()
        cancel = threading.Event()
        total = len(rows)
        results: list[Any] = []
        future: asyncio.Future[Any] | None = None
        next_beat = time.monotonic() + self._heartbeat_interval_s
        try:
            for row in rows:
                future = loop.run_in_executor(self._executor, fn, row, cancel)
                while not future.done():
                    await asyncio.wait({future}, timeout=max(0.0, next_beat - time.monotonic()))
                    if time.monotonic() >= next_beat:
                        activity.heartbeat(_progress(len(results), total))
                        next_beat = time.monotonic() + self._heartbeat_interval_s
                results.append(future.result())
                if on_row_done is not None:
                    on_row_done()
                if time.monotonic() >= next_beat:
                    activity.heartbeat(_progress(len(results), total))
                    next_beat = time.monotonic() + self._heartbeat_interval_s
            activity.heartbeat(_progress(total, total))
            return results
        except asyncio.CancelledError:
            cancel.set()
            if future is not None and not future.done():
                await asyncio.wait({future}, timeout=_CANCEL_GRACE_S)
                if future.done() and not future.cancelled():
                    future.exception()
            LOGGER.info(
                "transform batch cancelled activity_id=%s rows_processed=%d rows_total=%d",
                activity.info().activity_id, len(results), total,
            )
            raise


def _progress(processed: int, total: int) -> dict[str, int]:
    return {"rows_processed": processed, "rows_total": total}


async def main() -> None:
    logging.basicConfig(
        level=logging.INFO,
        format='{"time":"%(asctime)s","level":"%(levelname)s","msg":"%(message)s"}',
    )
    metrics_port = int(os.environ.get("METRICS_PORT", "9464"))
    start_http_server(metrics_port)
    LOGGER.info("prometheus metrics listening on :%d", metrics_port)

    max_workers = env_int("TRANSFORM_MAX_WORKERS", os.cpu_count() or 4)
    heartbeat_interval_s = env_float("TRANSFORM_HEARTBEAT_INTERVAL_S", 5.0)
    executor = ThreadPoolExecutor(max_workers=max_workers, thread_name_prefix="transform")
    activities = TransformActivities(executor, heartbeat_interval_s=heartbeat_interval_s)

    client = await Client.connect(
        os.environ.get("TEMPORAL_ADDRESS", "localhost:7233"),
        namespace=os.environ.get("TEMPORAL_NAMESPACE", "default"),
    )
    worker = Worker(
        client,
        task_queue=TASK_QUEUE,
        activities=[
            activities.preprocess_batch,
            activities.build_payload_batch,
            activities.render_payload_batch,
        ],
        max_concurrent_activities=env_int("ACTIVITY_CONCURRENCY", 64),
    )
    LOGGER.info(
        "transform-worker listening on queue %s max_workers=%d heartbeat_interval_s=%.1f",
        TASK_QUEUE, max_workers, heartbeat_interval_s,
    )
    try:
        await worker.run()
    finally:
        executor.shutdown(wait=True, cancel_futures=True)


# Workflow registration is not required - this worker hosts activities only.
workflow._NOT_REGISTERED = True  # noqa: SLF001 - sentinel to document intent


if __name__ == "__main__":
    asyncio.run(main())
