//! Route-level tests for job lifecycle ownership (MP-05): conditional
//! transitions, cancel when the workflow is gone, and export-only retries.
//! Run with: `cargo test --test job_lifecycle -- --ignored`

mod common;

use std::time::Duration;

use axum::body::Body;
use axum::http::{Request, StatusCode};
use common::*;
use serde_json::{json, Value};
use tower::ServiceExt;
use wiremock::matchers::{method, path};
use wiremock::{Mock, ResponseTemplate};

async fn seed_job(h: &Harness, org: &Org, status: &str) -> (i64, String) {
    let workflow_id = format!("mig-{}-{}", org.id, uuid::Uuid::new_v4());
    let job_id: i64 = sqlx::query_scalar(
        "INSERT INTO jobs (org_id, rule_template_id, rule_template_version, source_ref, status, temporal_workflow_id)
         VALUES ($1, $2, 1, '{}'::jsonb, $3::job_status, $4) RETURNING id",
    )
    .bind(org.id)
    .bind(org.template_id)
    .bind(status)
    .bind(&workflow_id)
    .fetch_one(&h.db)
    .await
    .unwrap();
    sqlx::query(
        "INSERT INTO job_rows (job_id, row_index, status) VALUES
         ($1, 0, 'succeeded'::row_status), ($1, 1, 'failed'::row_status)",
    )
    .bind(job_id)
    .execute(&h.db)
    .await
    .unwrap();
    (job_id, workflow_id)
}

async fn job_status(h: &Harness, job_id: i64) -> String {
    sqlx::query_scalar("SELECT status::text FROM jobs WHERE id = $1")
        .bind(job_id)
        .fetch_one(&h.db)
        .await
        .unwrap()
}

async fn bridge_calls(h: &Harness, bridge_path: &str) -> Vec<Value> {
    h.mocks
        .received_requests()
        .await
        .unwrap_or_default()
        .into_iter()
        .filter(|r| r.url.path() == bridge_path)
        .map(|r| serde_json::from_slice(&r.body).unwrap())
        .collect()
}

async fn mount_bridge(h: &Harness, bridge_path: &str, status: u16, body: Value) {
    Mock::given(method("POST"))
        .and(path(bridge_path))
        .respond_with(ResponseTemplate::new(status).set_body_json(body))
        .with_priority(1)
        .mount(&h.mocks)
        .await;
}

#[tokio::test]
#[ignore] // requires docker
async fn a_fast_workflow_outcome_is_not_overwritten_as_running() {
    let h = setup().await;
    let up = h.upload(&h.a, "customers.csv").await;
    // The bridge answers slowly; meanwhile the "workflow" finalizes the job.
    Mock::given(method("POST"))
        .and(path("/v1/workflows/start"))
        .respond_with(
            ResponseTemplate::new(200)
                .set_body_json(json!({"runId": "run-fast"}))
                .set_delay(Duration::from_millis(800)),
        )
        .with_priority(1)
        .mount(&h.mocks)
        .await;

    let req = Request::builder()
        .method("POST")
        .uri("/jobs")
        .header("authorization", format!("Bearer {}", h.a.token))
        .header("content-type", "application/json")
        .body(Body::from(
            h.job_body(&h.a, json!({"source_id": up["source_id"]}))
                .to_string(),
        ))
        .unwrap();
    let create = tokio::spawn(h.router.clone().oneshot(req));

    let job_id = loop {
        let id: Option<i64> =
            sqlx::query_scalar("SELECT id FROM jobs WHERE org_id = $1 AND status = 'pending'")
                .bind(h.a.id)
                .fetch_optional(&h.db)
                .await
                .unwrap();
        if let Some(id) = id {
            break id;
        }
        tokio::time::sleep(Duration::from_millis(20)).await;
    };
    sqlx::query("UPDATE jobs SET status = 'succeeded', finished_at = now() WHERE id = $1")
        .bind(job_id)
        .execute(&h.db)
        .await
        .unwrap();

    let resp = create.await.unwrap().unwrap();
    assert_eq!(resp.status(), StatusCode::OK);
    assert_eq!(job_status(&h, job_id).await, "succeeded");
    let run_id: Option<String> =
        sqlx::query_scalar("SELECT temporal_run_id FROM jobs WHERE id = $1")
            .bind(job_id)
            .fetch_one(&h.db)
            .await
            .unwrap();
    assert_eq!(
        run_id.as_deref(),
        Some("run-fast"),
        "the run is still recorded"
    );
}

#[tokio::test]
#[ignore] // requires docker
async fn an_ambiguous_start_leaves_the_job_for_the_reconciler() {
    let h = setup().await;
    let up = h.upload(&h.a, "customers.csv").await;
    mount_bridge(
        &h,
        "/v1/workflows/start",
        500,
        json!({"error": "deadline exceeded"}),
    )
    .await;

    let (status, body) = h
        .call(
            &h.a,
            "POST",
            "/jobs",
            Some(h.job_body(&h.a, json!({"source_id": up["source_id"]}))),
        )
        .await;
    assert_eq!(status, StatusCode::BAD_GATEWAY, "{body}");
    let (status, last_error): (String, Option<String>) =
        sqlx::query_as("SELECT status::text, last_error FROM jobs WHERE org_id = $1")
            .bind(h.a.id)
            .fetch_one(&h.db)
            .await
            .unwrap();
    assert_eq!(status, "pending", "the workflow may exist; do not guess");
    assert!(last_error.is_none());
}

#[tokio::test]
#[ignore] // requires docker
async fn operator_actions_only_apply_to_matching_states() {
    let h = setup().await;
    mount_bridge(&h, "/v1/workflows/signal", 200, json!({"ok": true})).await;
    mount_bridge(&h, "/v1/workflows/cancel", 200, json!({"ok": true})).await;

    let (done, _) = seed_job(&h, &h.a, "succeeded").await;
    for action in ["pause", "resume", "cancel"] {
        let (code, body) = h
            .call(&h.a, "POST", &format!("/jobs/{done}/{action}"), None)
            .await;
        assert_eq!(
            code,
            StatusCode::CONFLICT,
            "{action} a finished job: {body}"
        );
    }
    assert_eq!(job_status(&h, done).await, "succeeded");
    assert!(bridge_calls(&h, "/v1/workflows/signal").await.is_empty());
    assert!(bridge_calls(&h, "/v1/workflows/cancel").await.is_empty());

    let (job, _) = seed_job(&h, &h.a, "running").await;
    let uri = |a: &str| format!("/jobs/{job}/{a}");
    let (code, body) = h.call(&h.a, "POST", &uri("resume"), None).await;
    assert_eq!(code, StatusCode::CONFLICT, "resume a running job: {body}");
    // A pause/resume is recorded only once the workflow confirms it (A3):
    // the bridge answers "paused" while the pause is in flight, then "running".
    mount_bridge(
        &h,
        "/v1/workflows/query",
        200,
        json!({"result": {"paused": true, "acknowledged": true}}),
    )
    .await;
    let (code, body) = h.call(&h.a, "POST", &uri("pause"), None).await;
    assert_eq!(code, StatusCode::OK, "{body}");
    assert_eq!(body["status"], "paused");
    let signal = bridge_calls(&h, "/v1/workflows/signal").await;
    assert_eq!(signal.len(), 1);
    assert!(
        signal[0]["payload"]["commandId"]
            .as_str()
            .is_some_and(|s| !s.is_empty()),
        "the signal carries the command id: {}",
        signal[0]
    );
    let (code, _) = h.call(&h.a, "POST", &uri("pause"), None).await;
    assert_eq!(code, StatusCode::CONFLICT, "pause twice");
    h.mocks.reset().await;
    mount_bridge(&h, "/v1/workflows/signal", 200, json!({"ok": true})).await;
    mount_bridge(&h, "/v1/workflows/cancel", 200, json!({"ok": true})).await;
    mount_bridge(
        &h,
        "/v1/workflows/query",
        200,
        json!({"result": {"paused": false}}),
    )
    .await;
    let (code, body) = h.call(&h.a, "POST", &uri("resume"), None).await;
    assert_eq!(code, StatusCode::OK, "{body}");
    assert_eq!(body["status"], "running");

    let (code, body) = h.call(&h.b, "POST", &uri("cancel"), None).await;
    assert_eq!(code, StatusCode::NOT_FOUND, "org B: {body}");
    let (code, body) = h.call(&h.a, "POST", &uri("cancel"), None).await;
    assert_eq!(code, StatusCode::OK, "{body}");
    assert_eq!(body["status"], "cancelled");
    assert_eq!(body["totals_json"]["total"], 2);
    assert_eq!(bridge_calls(&h, "/v1/workflows/cancel").await.len(), 1);
}

#[tokio::test]
#[ignore] // requires docker
async fn cancel_settles_a_job_whose_workflow_is_gone() {
    let h = setup().await;
    let (job, _) = seed_job(&h, &h.a, "running").await;

    mount_bridge(
        &h,
        "/v1/workflows/cancel",
        502,
        json!({"error": "bridge down"}),
    )
    .await;
    let (code, body) = h
        .call(&h.a, "POST", &format!("/jobs/{job}/cancel"), None)
        .await;
    assert_eq!(code, StatusCode::BAD_GATEWAY, "{body}");
    assert_eq!(
        job_status(&h, job).await,
        "running",
        "unknown bridge state changes nothing"
    );

    h.mocks.reset().await;
    mount_bridge(
        &h,
        "/v1/workflows/cancel",
        404,
        json!({"error": "workflow not found"}),
    )
    .await;
    let (code, body) = h
        .call(&h.a, "POST", &format!("/jobs/{job}/cancel"), None)
        .await;
    assert_eq!(code, StatusCode::OK, "{body}");
    assert_eq!(body["status"], "cancelled");
    let after: Value = sqlx::query_scalar(
        "SELECT after_json FROM audit_log WHERE entity = 'job' AND action = 'cancel' AND entity_id = $1",
    )
    .bind(job.to_string())
    .fetch_one(&h.db)
    .await
    .unwrap();
    assert_eq!(after["workflow_found"], false);
}

#[tokio::test]
#[ignore] // requires docker
async fn export_retry_starts_only_an_export() {
    let h = setup().await;
    let (running, _) = seed_job(&h, &h.a, "running").await;
    let (code, body) = h
        .call(&h.a, "POST", &format!("/jobs/{running}/export"), None)
        .await;
    assert_eq!(code, StatusCode::CONFLICT, "{body}");

    let (job, workflow_id) = seed_job(&h, &h.a, "failed").await;
    let (code, body) = h
        .call(&h.b, "POST", &format!("/jobs/{job}/export"), None)
        .await;
    assert_eq!(code, StatusCode::NOT_FOUND, "org B: {body}");
    assert!(h.workflow_starts().await.is_empty());

    let (code, body) = h
        .call(&h.a, "POST", &format!("/jobs/{job}/export"), None)
        .await;
    assert_eq!(code, StatusCode::ACCEPTED, "{body}");
    let starts = h.workflow_starts().await;
    assert_eq!(starts.len(), 1);
    assert_eq!(starts[0]["workflowType"], "ExportResultsWorkflow");
    assert_eq!(starts[0]["workflowId"], format!("{workflow_id}-export"));
    assert_eq!(starts[0]["input"], json!({"jobId": job}));
    assert_eq!(
        job_status(&h, job).await,
        "failed",
        "the outcome is untouched"
    );

    mount_bridge(
        &h,
        "/v1/workflows/start",
        409,
        json!({"error": "already running"}),
    )
    .await;
    let (code, body) = h
        .call(&h.a, "POST", &format!("/jobs/{job}/export"), None)
        .await;
    assert_eq!(code, StatusCode::CONFLICT, "{body}");
}

#[tokio::test]
#[ignore] // requires docker
async fn unconfirmed_pause_is_recorded_not_guessed() {
    let h = setup().await;
    let (job, _) = seed_job(&h, &h.a, "running").await;
    mount_bridge(&h, "/v1/workflows/signal", 200, json!({"ok": true})).await;
    // The signal is accepted but the confirmation cannot be read.
    mount_bridge(
        &h,
        "/v1/workflows/query",
        502,
        json!({"error": "bridge down"}),
    )
    .await;

    let (code, body) = h
        .call(&h.a, "POST", &format!("/jobs/{job}/pause"), None)
        .await;
    assert_eq!(code, StatusCode::CONFLICT, "{body}");
    assert_eq!(
        job_status(&h, job).await,
        "running",
        "status follows the workflow, not the request"
    );
    let (requested, command_id): (Option<String>, Option<String>) =
        sqlx::query_as("SELECT control_requested, control_command_id FROM jobs WHERE id = $1")
            .bind(job)
            .fetch_one(&h.db)
            .await
            .unwrap();
    assert_eq!(requested.as_deref(), Some("pause"), "the intent is durable");
    let command_id = command_id.expect("command id recorded");

    // Retrying the same command keeps its identity and completes once confirmed.
    h.mocks.reset().await;
    mount_bridge(&h, "/v1/workflows/signal", 200, json!({"ok": true})).await;
    mount_bridge(
        &h,
        "/v1/workflows/query",
        200,
        json!({"result": {"paused": true, "lastCommandId": command_id, "acknowledged": true}}),
    )
    .await;
    let (code, body) = h
        .call(&h.a, "POST", &format!("/jobs/{job}/pause"), None)
        .await;
    assert_eq!(code, StatusCode::OK, "{body}");
    assert_eq!(body["status"], "paused");
    let signal = bridge_calls(&h, "/v1/workflows/signal").await;
    assert_eq!(
        signal[0]["payload"]["commandId"], command_id,
        "the retry reuses the command id"
    );
    let cleared: Option<String> =
        sqlx::query_scalar("SELECT control_requested FROM jobs WHERE id = $1")
            .bind(job)
            .fetch_one(&h.db)
            .await
            .unwrap();
    assert!(cleared.is_none(), "a confirmed request is cleared");
}

// Architecture review A2: batch admission follows the same transition rules
// as jobs. An ambiguous start leaves the batch pending for the reconciler;
// a successful start never overwrites a batch a fast workflow already
// finalized.
#[tokio::test]
#[ignore] // requires docker
async fn batch_start_transitions_are_conditional() {
    let h = setup().await;
    let up = h.upload(&h.a, "wave-01.tar.gz").await;
    let source_id = up["source_id"].as_i64().unwrap();

    mount_bridge(
        &h,
        "/v1/workflows/start",
        500,
        json!({"error": "deadline exceeded"}),
    )
    .await;
    let (status, body) = h
        .call(
            &h.a,
            "POST",
            "/batches",
            Some(json!({"source_ref": {"source_id": source_id}})),
        )
        .await;
    assert_eq!(status, StatusCode::BAD_GATEWAY, "{body}");
    let (ambiguous_id, batch_status, error): (i64, String, Option<String>) =
        sqlx::query_as("SELECT id, status::text, error_message FROM batches WHERE org_id = $1")
            .bind(h.a.id)
            .fetch_one(&h.db)
            .await
            .unwrap();
    assert_eq!(
        batch_status, "pending",
        "the workflow may exist; do not guess"
    );
    assert!(error.is_none());

    // A fast workflow finalizes the batch while the start response is in flight.
    h.mocks.reset().await;
    Mock::given(method("POST"))
        .and(path("/v1/workflows/start"))
        .respond_with(
            ResponseTemplate::new(200)
                .set_body_json(json!({"runId": "run-fast"}))
                .set_delay(Duration::from_millis(800)),
        )
        .with_priority(1)
        .mount(&h.mocks)
        .await;
    let req = Request::builder()
        .method("POST")
        .uri("/batches")
        .header("authorization", format!("Bearer {}", h.a.token))
        .header("content-type", "application/json")
        .body(Body::from(
            json!({"source_ref": {"source_id": source_id}}).to_string(),
        ))
        .unwrap();
    let create = tokio::spawn(h.router.clone().oneshot(req));
    let batch_id = loop {
        let id: Option<i64> = sqlx::query_scalar(
            "SELECT id FROM batches WHERE org_id = $1 AND status = 'pending' AND id <> $2",
        )
        .bind(h.a.id)
        .bind(ambiguous_id)
        .fetch_optional(&h.db)
        .await
        .unwrap();
        if let Some(id) = id {
            break id;
        }
        tokio::time::sleep(Duration::from_millis(20)).await;
    };
    sqlx::query("UPDATE batches SET status = 'succeeded', finished_at = now() WHERE id = $1")
        .bind(batch_id)
        .execute(&h.db)
        .await
        .unwrap();
    let resp = create.await.unwrap().unwrap();
    assert_eq!(resp.status(), StatusCode::OK);
    let (final_status, run_id): (String, Option<String>) =
        sqlx::query_as("SELECT status::text, temporal_run_id FROM batches WHERE id = $1")
            .bind(batch_id)
            .fetch_one(&h.db)
            .await
            .unwrap();
    assert_eq!(
        final_status, "succeeded",
        "a terminal batch is not put back to running"
    );
    assert_eq!(
        run_id.as_deref(),
        Some("run-fast"),
        "the run is still recorded"
    );
}
