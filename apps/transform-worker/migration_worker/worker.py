"""Temporal worker hosting the transformation activities.

The Go orchestrator dispatches ``preprocess_batch`` and ``build_payload_batch``
activities onto the ``transforms`` task queue whenever it encounters Python
transformations. Keeping these in a dedicated worker lets us scale the Python
sandbox tier independently of the Go workflow/IO tier.
"""
from __future__ import annotations

import asyncio
import logging
import os
import time
from typing import Any

from prometheus_client import Counter, Histogram, start_http_server
from temporalio import activity, workflow
from temporalio.client import Client
from temporalio.worker import Worker

from . import transforms
from .template import PreprocessStep, RuleTemplate

TASK_QUEUE = "transforms"
LOGGER = logging.getLogger("migration_worker")

PREPROCESS_LAT = Histogram("preprocess_batch_seconds", "Latency of preprocess_batch activity")
PAYLOAD_LAT = Histogram("payload_batch_seconds", "Latency of build_payload_batch activity")
ROWS_PROCESSED = Counter("transform_rows_processed_total", "Rows processed through transforms")


@activity.defn(name="preprocess_batch")
async def preprocess_batch_activity(payload: dict[str, Any]) -> list[dict[str, Any]]:
    rows = payload["rows"]
    steps = [PreprocessStep(**s) for s in payload["steps"]]
    start = time.perf_counter()
    try:
        out = [transforms.preprocess_row(row, steps) for row in rows]
        ROWS_PROCESSED.inc(len(out))
        return out
    finally:
        PREPROCESS_LAT.observe(time.perf_counter() - start)
        activity.heartbeat(len(rows))


@activity.defn(name="build_payload_batch")
async def build_payload_batch_activity(payload: dict[str, Any]) -> list[dict[str, Any]]:
    rows = payload["rows"]
    template = RuleTemplate.model_validate(payload["template"])
    start = time.perf_counter()
    try:
        out = [transforms.build_payload(row, template.model_dump(by_alias=True)) for row in rows]
        return out
    finally:
        PAYLOAD_LAT.observe(time.perf_counter() - start)
        activity.heartbeat(len(rows))


@activity.defn(name="render_payload_batch")
async def render_payload_batch_activity(payload: dict[str, Any]) -> list[dict[str, Any]]:
    """Evaluate the remaining $py expressions in a partially-rendered payload.

    Input: {"rows": [row, ...], "payload": partially-rendered payload template}.
    Output: one fully-rendered payload per row.
    """
    rows = payload["rows"]
    tpl = payload["payload"]
    start = time.perf_counter()
    try:
        return [transforms.render_py_expressions(tpl, row) for row in rows]
    finally:
        PAYLOAD_LAT.observe(time.perf_counter() - start)
        activity.heartbeat(len(rows))


async def main() -> None:
    logging.basicConfig(
        level=logging.INFO,
        format='{"time":"%(asctime)s","level":"%(levelname)s","msg":"%(message)s"}',
    )
    metrics_port = int(os.environ.get("METRICS_PORT", "9464"))
    start_http_server(metrics_port)
    LOGGER.info("prometheus metrics listening on :%d", metrics_port)

    client = await Client.connect(
        os.environ.get("TEMPORAL_ADDRESS", "localhost:7233"),
        namespace=os.environ.get("TEMPORAL_NAMESPACE", "default"),
    )
    worker = Worker(
        client,
        task_queue=TASK_QUEUE,
        activities=[
            preprocess_batch_activity,
            build_payload_batch_activity,
            render_payload_batch_activity,
        ],
        max_concurrent_activities=int(os.environ.get("ACTIVITY_CONCURRENCY", "64")),
    )
    LOGGER.info("transform-worker listening on queue %s", TASK_QUEUE)
    await worker.run()


# Workflow registration is not required - this worker hosts activities only.
workflow._NOT_REGISTERED = True  # noqa: SLF001 - sentinel to document intent


if __name__ == "__main__":
    asyncio.run(main())
