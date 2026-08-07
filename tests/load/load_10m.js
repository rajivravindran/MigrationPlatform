// 10M-row load test — intended for the k8s Job defined in k6-job.yaml.
//
// Assumes input shards were uploaded to the MinIO/S3 bucket referenced by
// the connector `WATCHED_PREFIX_CONNECTOR_ID`. Kicks off a single job and
// polls until completion, recording rows/sec every 30 seconds.
//
// Required env:
//   API=https://migration.example.com
//   TOKEN=<bearer>
//   RULE_TEMPLATE_ID=<id>
//   WATCHED_PREFIX_CONNECTOR_ID=<id>
//   EXPECTED_TOTAL_ROWS=10000000   (soft check)

import { check, sleep, fail } from "k6";
import http from "k6/http";
import { Trend, Rate, Counter } from "k6/metrics";

const API = __ENV.API;
const TOKEN = __ENV.TOKEN;
const TEMPLATE_ID = Number(__ENV.RULE_TEMPLATE_ID);
const CONNECTOR_ID = Number(__ENV.WATCHED_PREFIX_CONNECTOR_ID);
const EXPECTED_TOTAL = Number(__ENV.EXPECTED_TOTAL_ROWS || 10_000_000);
const MAX_DURATION_S = Number(__ENV.MAX_DURATION_S || 3 * 60 * 60);

export const options = {
  vus: 1,
  iterations: 1,
  thresholds: {
    rows_per_second: ["avg>2500"],
    failure_rate: ["rate<0.001"],
    "checks{critical:true}": ["rate==1.0"]
  },
  setupTimeout: "5m"
};

const rps = new Trend("rows_per_second");
const failureRate = new Rate("failure_rate");
const apiErrors = new Counter("api_errors");

function auth(extra) {
  return Object.assign({ Authorization: `Bearer ${TOKEN}` }, extra || {});
}

export default function () {
  if (!API || !TOKEN || !TEMPLATE_ID || !CONNECTOR_ID) {
    fail("API, TOKEN, RULE_TEMPLATE_ID, WATCHED_PREFIX_CONNECTOR_ID are required");
  }

  const body = JSON.stringify({
    rule_template_id: TEMPLATE_ID,
    source_ref: JSON.stringify({ kind: "connector", connector_id: CONNECTOR_ID })
  });
  const start = http.post(`${API}/jobs`, body, {
    headers: auth({ "Content-Type": "application/json" })
  });
  check(start, { "job start 2xx": (r) => r.status < 300 }, { critical: true });
  const jobId = start.json("id");
  console.log(`started job ${jobId}`);

  let lastProcessed = 0;
  let lastTick = Date.now();
  const t0 = lastTick;
  let finalStatus = null;

  while ((Date.now() - t0) / 1000 < MAX_DURATION_S) {
    sleep(30);
    const r = http.get(`${API}/jobs/${jobId}`, { headers: auth() });
    if (r.status !== 200) { apiErrors.add(1); continue; }
    const b = r.json();
    const now = Date.now();
    const delta = (b.processed + b.failed) - lastProcessed;
    const secs = (now - lastTick) / 1000;
    if (secs > 0) rps.add(delta / secs);
    failureRate.add(b.failed > 0 && b.total_rows > 0 ? b.failed / b.total_rows : 0);
    lastProcessed = b.processed + b.failed;
    lastTick = now;
    console.log(
      `[${((now - t0) / 1000).toFixed(0)}s] status=${b.status} processed=${b.processed} failed=${b.failed} total=${b.total_rows}`
    );
    finalStatus = b.status;
    if (["succeeded", "failed", "cancelled"].includes(finalStatus)) break;
  }

  check(
    { finalStatus, lastProcessed, total: lastProcessed },
    {
      "job terminal": (x) => x.finalStatus === "succeeded",
      "processed >= expected": (x) => x.lastProcessed >= EXPECTED_TOTAL * 0.999
    },
    { critical: true }
  );
}
