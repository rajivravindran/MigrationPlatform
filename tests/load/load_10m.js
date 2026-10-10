// 10M-row load test — intended for the k8s Job defined in k6-job.yaml.
//
// Starts a single job against an already-uploaded source and polls until
// completion, recording rows/sec every 30 seconds.
//
// Request/response shapes match the implemented API:
//   POST /jobs  {"rule_template_id": <int>, "source_ref": <object from POST /files>}
//   GET  /jobs/:id -> {"status": "...", "totals_json": {"processed", "failed", "total"}}
//
// Required env:
//   API=https://migration.example.com
//   TOKEN=<bearer>
//   RULE_TEMPLATE_ID=<id of a published template>
//   SOURCE_REF_JSON='{"type":"minio","bucket":"migration","key":"uploads/1/ab/<sha>","filename":"rows.csv",...}'
//                  (the `source_ref` object returned by POST /files, verbatim)
//   EXPECTED_TOTAL_ROWS=10000000   (soft check)
//
// Throughput thresholds are targets to measure against, not verified
// capabilities. Counters reported here are platform-side; use the integrity
// harness (tests/harness) for destination-side reconciliation.

import { check, sleep, fail } from "k6";
import http from "k6/http";
import { Trend, Rate, Counter } from "k6/metrics";

const API = __ENV.API;
const TOKEN = __ENV.TOKEN;
const TEMPLATE_ID = Number(__ENV.RULE_TEMPLATE_ID);
const SOURCE_REF_JSON = __ENV.SOURCE_REF_JSON;
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

function totals(job) {
  const t = job.totals_json || {};
  return { processed: t.processed || 0, failed: t.failed || 0, total: t.total || 0 };
}

export default function () {
  if (!API || !TOKEN || !TEMPLATE_ID || !SOURCE_REF_JSON) {
    fail("API, TOKEN, RULE_TEMPLATE_ID, SOURCE_REF_JSON are required");
  }
  let sourceRef;
  try {
    sourceRef = JSON.parse(SOURCE_REF_JSON);
  } catch (e) {
    fail(`SOURCE_REF_JSON is not valid JSON: ${e}`);
  }

  const start = http.post(
    `${API}/jobs`,
    JSON.stringify({ rule_template_id: TEMPLATE_ID, source_ref: sourceRef }),
    { headers: auth({ "Content-Type": "application/json" }) }
  );
  check(start, { "job start 2xx": (r) => r.status < 300 }, { critical: true });
  const jobId = start.json("id");
  console.log(`started job ${jobId}`);

  let lastDone = 0;
  let lastTick = Date.now();
  const t0 = lastTick;
  let finalStatus = null;
  let last = { processed: 0, failed: 0, total: 0 };

  while ((Date.now() - t0) / 1000 < MAX_DURATION_S) {
    sleep(30);
    const r = http.get(`${API}/jobs/${jobId}`, { headers: auth() });
    if (r.status !== 200) { apiErrors.add(1); continue; }
    const b = r.json();
    last = totals(b);
    const now = Date.now();
    const done = last.processed + last.failed;
    const secs = (now - lastTick) / 1000;
    if (secs > 0) rps.add((done - lastDone) / secs);
    failureRate.add(last.total > 0 ? last.failed / last.total : 0);
    lastDone = done;
    lastTick = now;
    console.log(
      `[${((now - t0) / 1000).toFixed(0)}s] status=${b.status} processed=${last.processed} failed=${last.failed} total=${last.total}`
    );
    finalStatus = b.status;
    if (["succeeded", "failed", "cancelled"].includes(finalStatus)) break;
  }

  check(
    { finalStatus, done: last.processed + last.failed },
    {
      "job terminal": (x) => x.finalStatus === "succeeded",
      "processed >= expected": (x) => x.done >= EXPECTED_TOTAL * 0.999
    },
    { critical: true }
  );
}
