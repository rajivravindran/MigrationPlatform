// Laptop smoke test: upload a small CSV, start a job, wait for completion.
//
// Env:
//   API=http://localhost:8080
//   TOKEN=<bearer token>
//   RULE_TEMPLATE_ID=<id of a PUBLISHED template>
//   SOURCE_CSV=/path/to/file.csv
//
// Success:
//   - HTTP upload 2xx
//   - Job reaches 'succeeded' within 120s
//   - processed + failed == total_rows
//   - failed / total_rows < 0.01
//
// Run:
//   k6 run tests/load/smoke_laptop.js

import { check, sleep, fail } from "k6";
import http from "k6/http";
import { FormData } from "https://jslib.k6.io/formdata/0.0.2/index.js";
import { Trend } from "k6/metrics";

const API = __ENV.API || "http://localhost:8080";
const TOKEN = __ENV.TOKEN;
const TEMPLATE_ID = Number(__ENV.RULE_TEMPLATE_ID || 1);
const SOURCE = __ENV.SOURCE_CSV || "/tmp/smoke.csv";

export const options = {
  vus: 1,
  iterations: 1,
  thresholds: {
    checks: ["rate==1.0"],
    job_wait_seconds: ["p(95)<120"]
  }
};

const jobWait = new Trend("job_wait_seconds", true);

function authHeaders(extra) {
  return Object.assign({ Authorization: `Bearer ${TOKEN}` }, extra || {});
}

export default function () {
  if (!TOKEN) fail("TOKEN env is required");

  const csv = open(SOURCE, "b");
  const fd = new FormData();
  fd.append("file", { data: csv, filename: "smoke.csv", content_type: "text/csv" });
  const upload = http.post(`${API}/files`, fd.body(), {
    headers: authHeaders({ "Content-Type": `multipart/form-data; boundary=${fd.boundary}` })
  });
  check(upload, { "upload 2xx": (r) => r.status >= 200 && r.status < 300 });
  const srcRef = upload.json("source_ref");
  if (!srcRef) fail(`no source_ref in upload response: ${upload.body}`);

  const start = http.post(
    `${API}/jobs`,
    JSON.stringify({ rule_template_id: TEMPLATE_ID, source_ref: srcRef }),
    { headers: authHeaders({ "Content-Type": "application/json" }) }
  );
  check(start, { "job start 2xx": (r) => r.status >= 200 && r.status < 300 });
  const jobId = start.json("id");

  const t0 = Date.now();
  let finalStatus = null;
  let total = 0, processed = 0, failed = 0;
  for (let i = 0; i < 120; i++) {
    sleep(1);
    const js = http.get(`${API}/jobs/${jobId}`, { headers: authHeaders() });
    if (js.status !== 200) continue;
    const b = js.json();
    finalStatus = b.status;
    total = b.total_rows || 0;
    processed = b.processed || 0;
    failed = b.failed || 0;
    if (["succeeded", "failed", "cancelled"].includes(finalStatus)) break;
  }
  jobWait.add((Date.now() - t0) / 1000);

  check({ finalStatus, total, processed, failed }, {
    "job succeeded": (x) => x.finalStatus === "succeeded",
    "all rows accounted": (x) => x.processed + x.failed === x.total,
    "low failure rate": (x) => x.total === 0 || x.failed / x.total < 0.01
  });
}
