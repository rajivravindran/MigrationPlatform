//! Route-level source authorization tests (MP-01, ADR 0001).
//!
//! Real router, real Postgres (testcontainers), wiremock standing in for both
//! S3 and the orchestrator's Temporal bridge so the exact workflow input can
//! be asserted. Run with:
//! `cargo test --test source_authz -- --ignored`

mod common;

use axum::http::StatusCode;
use common::*;
use serde_json::{json, Value};

#[tokio::test]
#[ignore] // requires docker
async fn upload_registers_source_and_job_uses_canonical_ref() {
    let h = setup().await;
    let up = h.upload(&h.a, "customers.csv").await;
    let source_id = up["source_id"].as_i64().unwrap();
    assert_eq!(up["source_ref"]["source_id"], source_id);

    let (status, job) = h
        .call(
            &h.a,
            "POST",
            "/jobs",
            Some(h.job_body(&h.a, json!({"source_id": source_id}))),
        )
        .await;
    assert_eq!(status, StatusCode::OK, "{job}");
    let key = up["source_ref"]["key"].as_str().unwrap();
    assert!(key.starts_with(&format!("uploads/{}/", h.a.id)));

    let starts = h.workflow_starts().await;
    assert_eq!(starts.len(), 1);
    let sent = &starts[0]["input"]["sourceRef"];
    assert_eq!(sent["source_id"], source_id);
    assert_eq!(sent["key"], key);
    assert_eq!(sent["etag"], "etag-1");
    assert_eq!(sent["filename"], "customers.csv");
    assert_eq!(job["source_ref"], *sent, "stored and dispatched refs agree");

    // The upload response itself is still accepted as a source_ref.
    let (status, _) = h
        .call(
            &h.a,
            "POST",
            "/jobs",
            Some(h.job_body(&h.a, up["source_ref"].clone())),
        )
        .await;
    assert_eq!(status, StatusCode::OK);

    let (status, sample) = h
        .call(
            &h.a,
            "POST",
            "/files/sample",
            Some(json!({"source_id": source_id})),
        )
        .await;
    assert_eq!(status, StatusCode::OK, "{sample}");
    assert_eq!(sample["format"], "csv");
    assert_eq!(sample["row_count"], 2);
}

#[tokio::test]
#[ignore] // requires docker
async fn other_org_cannot_sample_start_batch_download_or_retry() {
    let h = setup().await;
    let up = h.upload(&h.a, "customers.csv").await;
    let a_source_id = up["source_id"].as_i64().unwrap();
    let a_ref = up["source_ref"].clone();
    let a_key = a_ref["key"].as_str().unwrap().to_string();

    for body in [json!({"source_id": a_source_id}), json!({"key": a_key})] {
        let (status, _) = h.call(&h.b, "POST", "/files/sample", Some(body)).await;
        assert_eq!(status, StatusCode::BAD_REQUEST);
    }
    for source_ref in [json!({"source_id": a_source_id}), a_ref.clone()] {
        let (status, _) = h
            .call(&h.b, "POST", "/jobs", Some(h.job_body(&h.b, source_ref)))
            .await;
        assert_eq!(status, StatusCode::BAD_REQUEST);
    }
    let (status, _) = h
        .call(
            &h.b,
            "POST",
            "/batches",
            Some(json!({"source_ref": {"source_id": a_source_id}})),
        )
        .await;
    assert_eq!(status, StatusCode::BAD_REQUEST);
    assert_eq!(h.job_count(&h.b).await, 0);
    assert!(
        h.workflow_starts().await.is_empty(),
        "no workflow may start"
    );

    let (status, job) = h
        .call(
            &h.a,
            "POST",
            "/jobs",
            Some(h.job_body(&h.a, json!({"source_id": a_source_id}))),
        )
        .await;
    assert_eq!(status, StatusCode::OK);
    let job_id = job["id"].as_i64().unwrap();
    for (method, uri) in [
        ("GET", format!("/jobs/{job_id}")),
        ("GET", format!("/jobs/{job_id}/results")),
        ("POST", format!("/jobs/{job_id}/rows/0/retry")),
        ("POST", format!("/jobs/{job_id}/retry-failed")),
    ] {
        let (status, _) = h.call(&h.b, method, &uri, None).await;
        assert_eq!(status, StatusCode::NOT_FOUND, "{method} {uri}");
    }
    assert_eq!(
        h.workflow_starts().await.len(),
        1,
        "only org A's job started"
    );
}

#[tokio::test]
#[ignore] // requires docker
async fn malformed_and_dangerous_source_refs_are_rejected_before_any_job_row() {
    let h = setup().await;
    let cases = [
        json!("{\"type\":\"minio\"}"),
        json!({"type": "csv", "path": "/etc/passwd"}),
        json!({"type": "csv", "url": "http://169.254.169.254/latest/meta-data/"}),
        json!({"type": "minio", "bucket": BUCKET, "key": format!("uploads/{}/zz/unregistered", h.a.id)}),
        json!({"type": "watched_prefix", "connector_id": 1}),
        json!({"source_id": 999_999}),
    ];
    for source_ref in cases {
        let (status, body) = h
            .call(
                &h.a,
                "POST",
                "/jobs",
                Some(h.job_body(&h.a, source_ref.clone())),
            )
            .await;
        assert_eq!(status, StatusCode::BAD_REQUEST, "{source_ref} -> {body}");
    }
    assert_eq!(h.job_count(&h.a).await, 0);
    assert!(h.workflow_starts().await.is_empty());
}

#[tokio::test]
#[ignore] // requires docker
async fn connector_sources_are_org_scoped_and_cannot_override_endpoints() {
    let h = setup().await;
    let b_connector: i64 = sqlx::query_scalar(
        "INSERT INTO connectors (org_id, name, connector_kind, config_json)
         VALUES ($1, 'sf', 'salesforce', '{}'::jsonb) RETURNING id",
    )
    .bind(h.b.id)
    .fetch_one(&h.db)
    .await
    .unwrap();

    let (status, _) = h
        .call(
            &h.a,
            "POST",
            "/jobs",
            Some(h.job_body(
                &h.a,
                json!({"type": "connector", "connector_id": b_connector, "extra": {"soql": "SELECT Id FROM Account"}}),
            )),
        )
        .await;
    assert_eq!(
        status,
        StatusCode::BAD_REQUEST,
        "org A used org B's connector"
    );

    let schedule = |template_id: i64| {
        json!({
            "name": "nightly", "rule_template_id": template_id, "rule_template_version": 1,
            "connector_id": b_connector, "spec": {"cron": "0 2 * * *"},
        })
    };
    let (status, _) = h
        .call(&h.a, "POST", "/schedules", Some(schedule(h.a.template_id)))
        .await;
    assert_eq!(
        status,
        StatusCode::BAD_REQUEST,
        "org A scheduled org B's connector"
    );
    let (status, _) = h
        .call(&h.b, "POST", "/schedules", Some(schedule(h.a.template_id)))
        .await;
    assert_eq!(
        status,
        StatusCode::BAD_REQUEST,
        "org B scheduled org A's template"
    );
    let schedules: i64 = sqlx::query_scalar("SELECT COUNT(*) FROM schedules")
        .fetch_one(&h.db)
        .await
        .unwrap();
    assert_eq!(schedules, 0);

    let watched = |name: &str, config: Value| json!({"name": name, "connector_kind": "watched_prefix", "config_json": config});
    let own = format!("orgs/{}/incoming/", h.b.id);
    let (status, body) = h
        .call(
            &h.b,
            "POST",
            "/connectors",
            Some(watched("own", json!({"bucket": BUCKET, "prefix": own}))),
        )
        .await;
    assert_eq!(status, StatusCode::OK, "{body}");
    for (name, config) in [
        (
            "uploads",
            json!({"bucket": BUCKET, "prefix": format!("uploads/{}/", h.a.id)}),
        ),
        (
            "other-org",
            json!({"bucket": BUCKET, "prefix": format!("orgs/{}/", h.a.id)}),
        ),
        ("whole-bucket", json!({"bucket": BUCKET, "prefix": ""})),
        (
            "foreign-bucket",
            json!({"bucket": "backups", "prefix": own}),
        ),
    ] {
        let (status, _) = h
            .call(&h.b, "POST", "/connectors", Some(watched(name, config)))
            .await;
        assert_eq!(status, StatusCode::BAD_REQUEST, "watched connector {name}");
    }

    let (status, _) = h
        .call(
            &h.b,
            "POST",
            "/jobs",
            Some(h.job_body(
                &h.b,
                json!({"type": "connector", "connector_id": b_connector, "extra": {"instanceUrl": "http://127.0.0.1"}}),
            )),
        )
        .await;
    assert_eq!(status, StatusCode::BAD_REQUEST);
    assert!(h.workflow_starts().await.is_empty());

    let (status, job) = h
        .call(
            &h.b,
            "POST",
            "/jobs",
            Some(h.job_body(
                &h.b,
                json!({"type": "connector", "connector_id": b_connector, "extra": {"soql": "SELECT Id FROM Account"}}),
            )),
        )
        .await;
    assert_eq!(status, StatusCode::OK, "{job}");
    let sent = &h.workflow_starts().await[0]["input"]["sourceRef"];
    assert_eq!(sent["type"], "connector");
    assert_eq!(sent["connector_id"], b_connector);
    assert!(sent.get("secret").is_none());
}
