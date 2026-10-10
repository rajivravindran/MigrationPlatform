//! Route-level tests for ADR 0003: destination secrets and serialized,
//! confirmed retries. Run with:
//! `cargo test --test operations_api -- --ignored`

mod common;

use axum::http::StatusCode;
use common::*;
use serde_json::{json, Value};
use wiremock::matchers::{body_string_contains, method, path};
use wiremock::{Mock, ResponseTemplate};

const VALUE_A: &str = "s3cr3t-value-A";
const VALUE_B: &str = "s3cr3t-value-B";

async fn ciphertext(h: &Harness, org: &Org, name: &str) -> Vec<u8> {
    sqlx::query_scalar("SELECT ciphertext FROM secrets WHERE org_id = $1 AND name = $2")
        .bind(org.id)
        .bind(name)
        .fetch_one(&h.db)
        .await
        .unwrap()
}

async fn audit(
    h: &Harness,
    org: &Org,
    entity: &str,
) -> Vec<(String, Option<Value>, Option<Value>)> {
    sqlx::query_as(
        "SELECT action, before_json, after_json FROM audit_log
         WHERE org_id = $1 AND entity = $2 ORDER BY id",
    )
    .bind(org.id)
    .bind(entity)
    .fetch_all(&h.db)
    .await
    .unwrap()
}

#[tokio::test]
#[ignore] // requires docker
async fn destination_secrets_are_write_only_org_scoped_and_audited() {
    let h = setup().await;

    let (status, body) = h
        .call(
            &h.a,
            "PUT",
            "/secrets/crm_token",
            Some(json!({"value": VALUE_A})),
        )
        .await;
    assert_eq!(status, StatusCode::OK, "{body}");
    assert_eq!(body["name"], "crm_token");
    assert!(body["revoked_at"].is_null());
    let first = ciphertext(&h, &h.a, "crm_token").await;

    let (status, list) = h.call(&h.a, "GET", "/secrets", None).await;
    assert_eq!(status, StatusCode::OK);
    assert_eq!(list["items"].as_array().unwrap().len(), 1);
    for body in [&body, &list] {
        assert!(
            !body.to_string().contains(VALUE_A),
            "a secret value was returned"
        );
    }

    for (method, uri) in [
        ("GET", "/secrets/crm_token"),
        ("DELETE", "/secrets/crm_token"),
    ] {
        let (status, _) = h.call(&h.b, method, uri, None).await;
        assert_eq!(status, StatusCode::NOT_FOUND, "org B {method} {uri}");
    }
    let (_, list_b) = h.call(&h.b, "GET", "/secrets", None).await;
    assert!(list_b["items"].as_array().unwrap().is_empty());

    let (status, _) = h
        .call(
            &h.a,
            "PUT",
            "/secrets/crm_token",
            Some(json!({"value": VALUE_B})),
        )
        .await;
    assert_eq!(status, StatusCode::OK);
    assert_ne!(
        ciphertext(&h, &h.a, "crm_token").await,
        first,
        "rotation must replace the value"
    );

    // An internal credential's name cannot be taken over by a template secret.
    sqlx::query("INSERT INTO secrets (org_id, name, ciphertext, nonce) VALUES ($1, 'sftp-secret', 'ct', 'n')")
        .bind(h.a.id)
        .execute(&h.db)
        .await
        .unwrap();
    let (status, body) = h
        .call(
            &h.a,
            "PUT",
            "/secrets/sftp-secret",
            Some(json!({"value": VALUE_A})),
        )
        .await;
    assert_eq!(status, StatusCode::CONFLICT, "{body}");
    let (kind, ct): (String, Vec<u8>) = sqlx::query_as(
        "SELECT kind, ciphertext FROM secrets WHERE org_id = $1 AND name = 'sftp-secret'",
    )
    .bind(h.a.id)
    .fetch_one(&h.db)
    .await
    .unwrap();
    assert_eq!(
        (kind.as_str(), ct.as_slice()),
        ("internal", b"ct".as_slice())
    );
    let (status, _) = h.call(&h.a, "GET", "/secrets/sftp-secret", None).await;
    assert_eq!(
        status,
        StatusCode::NOT_FOUND,
        "internal secrets are not listed"
    );

    for bad in ["a%20b", "..%2Fetc", "tok%2Fen"] {
        let (status, _) = h
            .call(
                &h.a,
                "PUT",
                &format!("/secrets/{bad}"),
                Some(json!({"value": "x"})),
            )
            .await;
        assert!(
            status == StatusCode::BAD_REQUEST || status == StatusCode::NOT_FOUND,
            "{bad}: {status}"
        );
    }
    let (status, _) = h
        .call(&h.a, "PUT", "/secrets/empty", Some(json!({"value": ""})))
        .await;
    assert_eq!(status, StatusCode::BAD_REQUEST);

    let (status, body) = h.call(&h.a, "DELETE", "/secrets/crm_token", None).await;
    assert_eq!(status, StatusCode::OK);
    assert!(!body["revoked_at"].is_null());
    assert!(
        ciphertext(&h, &h.a, "crm_token").await.is_empty(),
        "revocation erases the value"
    );
    let (status, _) = h.call(&h.a, "DELETE", "/secrets/crm_token", None).await;
    assert_eq!(status, StatusCode::NOT_FOUND, "already revoked");

    let (status, body) = h
        .call(
            &h.a,
            "PUT",
            "/secrets/crm_token",
            Some(json!({"value": VALUE_A})),
        )
        .await;
    assert_eq!(status, StatusCode::OK);
    assert!(body["revoked_at"].is_null());

    let entries = audit(&h, &h.a, "destination_secret").await;
    let actions: Vec<&str> = entries.iter().map(|(a, _, _)| a.as_str()).collect();
    assert_eq!(actions, ["create", "rotate", "revoke", "reactivate"]);
    let all_audit: Vec<(Option<Value>, Option<Value>)> =
        sqlx::query_as("SELECT before_json, after_json FROM audit_log")
            .fetch_all(&h.db)
            .await
            .unwrap();
    let dump = format!("{all_audit:?}");
    assert!(
        !dump.contains(VALUE_A) && !dump.contains(VALUE_B),
        "audit log holds a secret value"
    );
}

async fn seed_finished_job(h: &Harness, org: &Org) -> (i64, String) {
    let workflow_id = format!("mig-{}-ops", org.id);
    let job_id: i64 = sqlx::query_scalar(
        "INSERT INTO jobs (org_id, rule_template_id, rule_template_version, source_ref, status, temporal_workflow_id)
         VALUES ($1, $2, 1, '{}'::jsonb, 'failed'::job_status, $3) RETURNING id",
    )
    .bind(org.id)
    .bind(org.template_id)
    .bind(&workflow_id)
    .fetch_one(&h.db)
    .await
    .unwrap();
    sqlx::query(
        "INSERT INTO job_rows (job_id, row_index, status, last_error) VALUES
         ($1, 0, 'unresolved'::row_status, 'outcome unknown: status 503'),
         ($1, 1, 'failed'::row_status, 'status 422')",
    )
    .bind(job_id)
    .execute(&h.db)
    .await
    .unwrap();
    (job_id, workflow_id)
}

async fn set_job_status(h: &Harness, job_id: i64, status: &str) {
    sqlx::query("UPDATE jobs SET status = $2::job_status WHERE id = $1")
        .bind(job_id)
        .bind(status)
        .execute(&h.db)
        .await
        .unwrap();
}

async fn row_status(h: &Harness, job_id: i64, row: i64) -> String {
    sqlx::query_scalar("SELECT status::text FROM job_rows WHERE job_id = $1 AND row_index = $2")
        .bind(job_id)
        .bind(row)
        .fetch_one(&h.db)
        .await
        .unwrap()
}

#[tokio::test]
#[ignore] // requires docker
async fn retries_are_serialized_and_unresolved_rows_need_confirmation() {
    let h = setup().await;
    let (job_id, workflow_id) = seed_finished_job(&h, &h.a).await;
    let row0 = format!("/jobs/{job_id}/rows/0/retry");
    let row1 = format!("/jobs/{job_id}/rows/1/retry");
    let all = format!("/jobs/{job_id}/retry-failed");

    for status in ["pending", "running", "paused"] {
        set_job_status(&h, job_id, status).await;
        for uri in [&row1, &all] {
            let (code, body) = h.call(&h.a, "POST", uri, None).await;
            assert_eq!(code, StatusCode::CONFLICT, "job {status}, {uri}: {body}");
        }
    }
    set_job_status(&h, job_id, "failed").await;
    assert!(
        h.workflow_starts().await.is_empty(),
        "nothing may start while the job runs"
    );

    let (code, body) = h.call(&h.a, "POST", &row0, None).await;
    assert_eq!(code, StatusCode::CONFLICT, "{body}");
    assert!(body.to_string().contains("confirm_not_applied"), "{body}");
    assert!(h.workflow_starts().await.is_empty());

    let (code, body) = h
        .call(
            &h.a,
            "POST",
            &all,
            Some(json!({"confirm_not_applied": true})),
        )
        .await;
    assert_eq!(
        code,
        StatusCode::BAD_REQUEST,
        "confirmation is per row: {body}"
    );

    let (code, body) = h
        .call(
            &h.a,
            "POST",
            &row0,
            Some(json!({"confirm_not_applied": true})),
        )
        .await;
    assert_eq!(code, StatusCode::OK, "{body}");
    let starts = h.workflow_starts().await;
    assert_eq!(starts.len(), 1);
    assert_eq!(starts[0]["workflowId"], format!("{workflow_id}-retry"));
    assert_eq!(starts[0]["input"]["retryRow"]["confirmNotApplied"], true);
    assert_eq!(starts[0]["input"]["retryRow"]["rowIndex"], 0);
    assert_eq!(
        row_status(&h, job_id, 0).await,
        "unresolved",
        "the row keeps its status until the retry records an outcome"
    );
    let entries = audit(&h, &h.a, "job_row").await;
    let (action, before, after) = entries.last().unwrap();
    assert_eq!(action, "retry");
    assert_eq!(before.as_ref().unwrap()["status"], "unresolved");
    assert_eq!(after.as_ref().unwrap()["confirm_not_applied"], true);

    // While that retry runs the bridge refuses the same workflow ID.
    Mock::given(method("POST"))
        .and(path("/v1/workflows/start"))
        .and(body_string_contains(format!("{workflow_id}-retry")))
        .respond_with(ResponseTemplate::new(409).set_body_json(
            json!({"error": format!("workflow {workflow_id}-retry is already running")}),
        ))
        .with_priority(1)
        .mount(&h.mocks)
        .await;
    for uri in [&row1, &all] {
        let (code, body) = h.call(&h.a, "POST", uri, None).await;
        assert_eq!(code, StatusCode::CONFLICT, "{uri}: {body}");
        assert!(body.to_string().contains("already running"), "{body}");
    }
    assert_eq!(row_status(&h, job_id, 1).await, "failed");

    let (code, _) = h
        .call(
            &h.b,
            "POST",
            &row0,
            Some(json!({"confirm_not_applied": true})),
        )
        .await;
    assert_eq!(code, StatusCode::NOT_FOUND, "other orgs cannot retry");
}
