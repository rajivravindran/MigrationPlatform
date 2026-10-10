//! Route-level test harness: the real router, real Postgres
//! (testcontainers), and wiremock standing in for S3 and the orchestrator's
//! Temporal bridge so dispatched workflow input can be asserted.
#![allow(dead_code)]

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

pub const BUCKET: &str = "migration";
pub const CSV: &str = "email,name\na@example.test,Ada\nb@example.test,Bo\n";

pub struct Org {
    pub id: i64,
    pub token: String,
    pub template_id: i64,
}

pub struct Harness {
    _pg: testcontainers::ContainerAsync<PgImage>,
    pub db: PgPool,
    pub router: axum::Router,
    pub mocks: MockServer,
    pub a: Org,
    pub b: Org,
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

pub async fn seed_org(db: &PgPool, jwt: &JwtKeys, name: &str) -> Org {
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

pub async fn setup() -> Harness {
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
    pub async fn call(
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

    pub async fn send(&self, req: Request<Body>) -> (StatusCode, Value) {
        let resp = self.router.clone().oneshot(req).await.unwrap();
        let status = resp.status();
        let bytes = resp.into_body().collect().await.unwrap().to_bytes();
        let body = serde_json::from_slice(&bytes)
            .unwrap_or_else(|_| Value::String(String::from_utf8_lossy(&bytes).into()));
        (status, body)
    }

    pub async fn upload(&self, org: &Org, filename: &str) -> Value {
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

    pub async fn job_count(&self, org: &Org) -> i64 {
        sqlx::query_scalar("SELECT COUNT(*) FROM jobs WHERE org_id = $1")
            .bind(org.id)
            .fetch_one(&self.db)
            .await
            .unwrap()
    }

    pub async fn workflow_starts(&self) -> Vec<Value> {
        self.mocks
            .received_requests()
            .await
            .unwrap_or_default()
            .into_iter()
            .filter(|r| r.url.path() == "/v1/workflows/start")
            .map(|r| serde_json::from_slice(&r.body).unwrap())
            .collect()
    }

    pub fn job_body(&self, org: &Org, source_ref: Value) -> Value {
        json!({"rule_template_id": org.template_id, "source_ref": source_ref})
    }
}
