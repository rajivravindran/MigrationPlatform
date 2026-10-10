//! Route-level source authorization tests (MP-01, ADR 0001).
//!
//! Real router, real Postgres (testcontainers), wiremock standing in for both
//! S3 and the orchestrator's Temporal bridge so the exact workflow input can
//! be asserted. Run with:
//! `cargo test --test source_authz -- --ignored`

use axum::body::Body;
use axum::http::{Request, StatusCode};
use http_body_util::BodyExt;
use migration_api::config::Config;
use migration_api::security::{JwtKeys, MasterKey};
use migration_api::state::{AppState, AppStateInner};
use migration_api::temporal::TemporalClient;
use serde_json::{json, Value};
use sqlx::postgres::PgPoolOptions;
use sqlx::PgPool;
use testcontainers::runners::AsyncRunner;
use testcontainers_modules::postgres::Postgres as PgImage;
use tower::ServiceExt;
use wiremock::matchers::{method, path};
use wiremock::{Mock, MockServer, ResponseTemplate};

const BUCKET: &str = "migration";
const CSV: &str = "email,name\na@example.test,Ada\nb@example.test,Bo\n";

struct Org {
    id: i64,
    token: String,
    template_id: i64,
}

struct Harness {
    _pg: testcontainers::ContainerAsync<PgImage>,
    db: PgPool,
    router: axum::Router,
    mocks: MockServer,
    a: Org,
    b: Org,
}

fn config(mock_uri: &str) -> Config {
    Config {
        api_bind: "127.0.0.1:0".into(),
        metrics_bind: "127.0.0.1:0".into(),
        database_url: String::new(),
        redis_url: "redis://127.0.0.1:1/0".into(),
        temporal_host_port: String::new(),
        temporal_namespace: "default".into(),
        orchestrator_bridge_url: Some(mock_uri.into()),
        bridge_token: Some("test-bridge".into()),
        otlp_endpoint: None,
        minio_endpoint: mock_uri.into(),
        minio_access_key: "test".into(),
        minio_secret_key: "test".into(),
        minio_bucket: BUCKET.into(),
        watch_allowed_buckets: vec![],
        master_key: "0123456789abcdef0123456789abcdef".into(),
        jwt_private_key_path: None,
        jwt_public_key_path: None,
        jwt_issuer: "test".into(),
        jwt_audience: "test".into(),
        db_max_connections: 4,
    }
}

async fn seed_org(db: &PgPool, jwt: &JwtKeys, name: &str) -> Org {
    let id: i64 = sqlx::query_scalar("INSERT INTO organizations (name) VALUES ($1) RETURNING id")
        .bind(name)
        .fetch_one(db)
        .await
        .unwrap();
    let user: i64 = sqlx::query_scalar(
        "INSERT INTO users (org_id, email, role, password_hash) VALUES ($1, $2, 'admin'::user_role, 'x') RETURNING id",
    )
    .bind(id)
    .bind(format!("admin@{name}.test"))
    .fetch_one(db)
    .await
    .unwrap();
    let template_id: i64 = sqlx::query_scalar(
        "INSERT INTO rule_templates (org_id, template_key, version, name, schema_json, published, created_by)
         VALUES ($1, 'rt_customers', 1, 'Customers', '{}'::jsonb, true, $2) RETURNING id",
    )
    .bind(id)
    .bind(user)
    .fetch_one(db)
    .await
    .unwrap();
    Org {
        id,
        token: jwt.issue(user, id, "admin").unwrap(),
        template_id,
    }
}

async fn setup() -> Harness {
    let pg = PgImage::default().start().await.expect("start postgres");
    let port = pg.get_host_port_ipv4(5432).await.unwrap();
    let db = PgPoolOptions::new()
        .max_connections(4)
        .connect(&format!(
            "postgres://postgres:postgres@127.0.0.1:{port}/postgres"
        ))
        .await
        .expect("connect pg");
    sqlx::migrate!("./migrations")
        .run(&db)
        .await
        .expect("migrate");

    let mocks = MockServer::start().await;
    Mock::given(method("PUT"))
        .respond_with(ResponseTemplate::new(200).insert_header("ETag", "\"etag-1\""))
        .mount(&mocks)
        .await;
    Mock::given(method("GET"))
        .respond_with(
            ResponseTemplate::new(200)
                .insert_header("Content-Length", CSV.len().to_string().as_str())
                .set_body_string(CSV),
        )
        .mount(&mocks)
        .await;
    Mock::given(method("POST"))
        .and(path("/v1/workflows/start"))
        .respond_with(ResponseTemplate::new(200).set_body_json(json!({"runId": "run-test"})))
        .mount(&mocks)
        .await;

    let cfg = config(&mocks.uri());
    let jwt = JwtKeys::load(&cfg).unwrap();
    let s3 = aws_sdk_s3::Client::from_conf(
        aws_sdk_s3::Config::builder()
            .endpoint_url(mocks.uri())
            .region(aws_sdk_s3::config::Region::new("us-east-1"))
            .force_path_style(true)
            .credentials_provider(aws_sdk_s3::config::Credentials::new(
                "test", "test", None, None, "static",
            ))
            .behavior_version_latest()
            .build(),
    );
    let a = seed_org(&db, &jwt, "org-a").await;
    let b = seed_org(&db, &jwt, "org-b").await;
    let state = AppState::new(AppStateInner {
        temporal: TemporalClient::new(Some(&mocks.uri()), "test-bridge", "default"),
        redis: redis::Client::open(cfg.redis_url.as_str()).unwrap(),
        master_key: MasterKey::from_env_value(&cfg.master_key).unwrap(),
        metrics_handle: metrics_exporter_prometheus::PrometheusBuilder::new()
            .build_recorder()
            .handle(),
        db: db.clone(),
        s3,
        jwt,
        cfg,
    });
    Harness {
        _pg: pg,
        db,
        router: migration_api::build_router(state),
        mocks,
        a,
        b,
    }
}

impl Harness {
    async fn call(
        &self,
        org: &Org,
        method: &str,
        uri: &str,
        body: Option<Value>,
    ) -> (StatusCode, Value) {
        let mut req = Request::builder()
            .method(method)
            .uri(uri)
            .header("authorization", format!("Bearer {}", org.token));
        let body = match body {
            Some(v) => {
                req = req.header("content-type", "application/json");
                Body::from(v.to_string())
            }
            None => Body::empty(),
        };
        self.send(req.body(body).unwrap()).await
    }

    async fn send(&self, req: Request<Body>) -> (StatusCode, Value) {
        let resp = self.router.clone().oneshot(req).await.unwrap();
        let status = resp.status();
        let bytes = resp.into_body().collect().await.unwrap().to_bytes();
        let body = serde_json::from_slice(&bytes)
            .unwrap_or_else(|_| Value::String(String::from_utf8_lossy(&bytes).into()));
        (status, body)
    }

    async fn upload(&self, org: &Org, filename: &str) -> Value {
        let boundary = "harness-boundary";
        let body = format!(
            "--{boundary}\r\nContent-Disposition: form-data; name=\"file\"; filename=\"{filename}\"\r\n\
             Content-Type: text/csv\r\n\r\n{CSV}\r\n--{boundary}--\r\n"
        );
        let req = Request::builder()
            .method("POST")
            .uri("/files")
            .header("authorization", format!("Bearer {}", org.token))
            .header(
                "content-type",
                format!("multipart/form-data; boundary={boundary}"),
            )
            .body(Body::from(body))
            .unwrap();
        let (status, body) = self.send(req).await;
        assert_eq!(status, StatusCode::OK, "upload: {body}");
        body
    }

    async fn job_count(&self, org: &Org) -> i64 {
        sqlx::query_scalar("SELECT COUNT(*) FROM jobs WHERE org_id = $1")
            .bind(org.id)
            .fetch_one(&self.db)
            .await
            .unwrap()
    }

    async fn workflow_starts(&self) -> Vec<Value> {
        self.mocks
            .received_requests()
            .await
            .unwrap_or_default()
            .into_iter()
            .filter(|r| r.url.path() == "/v1/workflows/start")
            .map(|r| serde_json::from_slice(&r.body).unwrap())
            .collect()
    }

    fn job_body(&self, org: &Org, source_ref: Value) -> Value {
        json!({"rule_template_id": org.template_id, "source_ref": source_ref})
    }
}

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
