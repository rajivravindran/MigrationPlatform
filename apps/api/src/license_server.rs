//! Vendor-side phone-home license service (trial issuance + heartbeat).
//!
//! Library module so the router can be exercised in-process by tests; the
//! `migration-license-server` binary is a thin wrapper around [`build_router`].
//!
//! Endpoints (all `POST`, bearer-authenticated with the shared activation token):
//! - `/v1/trial/start`  — email-gated 10-day trial keyed by install fingerprint.
//!   Same fingerprint reuses the original `expires_at`; expired → `402`.
//! - `/v1/heartbeat`    — re-attest a presented signed license: verify the
//!   signature, fingerprint binding, expiry and the revocation list, then
//!   re-sign it with a fresh `issued_at`. Revoked → `403`; expired → `402`.
//!
//! Storage is a single SQLite file (`trials`, `revocations`). This is an MVP
//! single-instance service: put the DB on durable storage and back it up.

use std::collections::HashMap;
use std::path::Path;
use std::sync::{Arc, Mutex};
use std::time::{Duration, Instant};

use anyhow::{bail, Context, Result};
use axum::extract::{DefaultBodyLimit, State};
use axum::http::{HeaderMap, StatusCode};
use axum::response::{IntoResponse, Response};
use axum::routing::{get, post};
use axum::{Json, Router};
use chrono::{DateTime, Duration as ChronoDuration, Utc};
use rsa::pkcs8::DecodePrivateKey;
use rsa::{RsaPrivateKey, RsaPublicKey};
use rusqlite::{params, Connection, OptionalExtension};
use serde::{Deserialize, Serialize};
use serde_json::Value;
use tower_http::timeout::TimeoutLayer;
use tracing::{info, warn};

use crate::security::license::{self, validate_trial_email, LicenseDoc, LicenseKind};

pub const DEFAULT_TRIAL_DAYS: i64 = 10;
const PER_FINGERPRINT_INTERVAL: Duration = Duration::from_secs(5);
const GLOBAL_PER_SECOND: u32 = 20;
const MIN_TOKEN_LEN: usize = 32;

/// Runtime settings for the service.
#[derive(Debug, Clone)]
pub struct ServerConfig {
    /// PKCS#8 PEM of the vendor signing key. Keep offline; mount at runtime.
    pub signing_key_pem: String,
    /// Shared bearer token customers present (`LICENSE_SERVER_TOKEN`).
    pub activation_token: String,
    pub trial_days: i64,
}

impl ServerConfig {
    pub fn validate(&self) -> Result<()> {
        if self.activation_token.len() < MIN_TOKEN_LEN {
            bail!("LICENSE_SERVER_TOKEN must contain at least {MIN_TOKEN_LEN} characters");
        }
        if !(1..=30).contains(&self.trial_days) {
            bail!("TRIAL_DAYS must be between 1 and 30");
        }
        RsaPrivateKey::from_pkcs8_pem(&self.signing_key_pem).context("parsing signing key")?;
        Ok(())
    }
}

#[derive(Clone)]
pub struct AppState {
    db: Arc<Mutex<Connection>>,
    cfg: Arc<ServerConfig>,
    /// Public half of the signing key, used to verify presented licenses.
    verify_key: Arc<RsaPublicKey>,
    per_fingerprint: Arc<Mutex<HashMap<String, Instant>>>,
    global: Arc<Mutex<(Instant, u32)>>,
}

impl AppState {
    pub fn new(db: Connection, cfg: ServerConfig) -> Result<Self> {
        cfg.validate()?;
        let private = RsaPrivateKey::from_pkcs8_pem(&cfg.signing_key_pem)?;
        Ok(Self {
            db: Arc::new(Mutex::new(db)),
            verify_key: Arc::new(RsaPublicKey::from(&private)),
            cfg: Arc::new(cfg),
            per_fingerprint: Arc::new(Mutex::new(HashMap::new())),
            global: Arc::new(Mutex::new((Instant::now(), 0))),
        })
    }
}

/// Open (creating if needed) the SQLite store and apply idempotent schema changes.
pub fn open_db(path: &str) -> Result<Connection> {
    if let Some(parent) = Path::new(path)
        .parent()
        .filter(|p| !p.as_os_str().is_empty())
    {
        std::fs::create_dir_all(parent)?;
    }
    let conn = Connection::open(path).with_context(|| format!("opening sqlite {path}"))?;
    conn.execute_batch(
        r#"
        PRAGMA busy_timeout=5000;
        PRAGMA journal_mode=WAL;
        PRAGMA synchronous=FULL;
        CREATE TABLE IF NOT EXISTS trials (
            fingerprint TEXT PRIMARY KEY NOT NULL,
            email TEXT,
            started_at TEXT NOT NULL,
            expires_at TEXT NOT NULL,
            product TEXT,
            version TEXT,
            created_at TEXT NOT NULL DEFAULT (datetime('now'))
        );
        CREATE TABLE IF NOT EXISTS revocations (
            fingerprint TEXT NOT NULL,
            licensee TEXT NOT NULL,
            reason TEXT,
            revoked_at TEXT NOT NULL,
            PRIMARY KEY (fingerprint, licensee)
        );
        "#,
    )?;
    ensure_column(&conn, "trials", "last_heartbeat_at", "TEXT")?;
    ensure_column(
        &conn,
        "trials",
        "heartbeat_count",
        "INTEGER NOT NULL DEFAULT 0",
    )?;
    ensure_column(&conn, "trials", "revoked_at", "TEXT")?;
    ensure_column(&conn, "trials", "revoked_reason", "TEXT")?;
    Ok(conn)
}

fn ensure_column(conn: &Connection, table: &str, column: &str, decl: &str) -> Result<()> {
    let mut stmt = conn.prepare(&format!("PRAGMA table_info({table})"))?;
    let exists = stmt
        .query_map([], |row| row.get::<_, String>(1))?
        .filter_map(Result::ok)
        .any(|c| c == column);
    if !exists {
        conn.execute_batch(&format!("ALTER TABLE {table} ADD COLUMN {column} {decl};"))?;
    }
    Ok(())
}

pub fn build_router(state: AppState) -> Router {
    Router::new()
        .route("/healthz", get(|| async { "ok" }))
        .route("/readyz", get(|| async { "ok" }))
        .route("/v1/trial/start", post(trial_start))
        .route("/v1/heartbeat", post(heartbeat))
        .layer(DefaultBodyLimit::max(32 * 1024))
        .layer(TimeoutLayer::with_status_code(
            StatusCode::REQUEST_TIMEOUT,
            Duration::from_secs(10),
        ))
        .with_state(state)
}

/// Add (or update) a revocation. `licensee = "*"` revokes every license for
/// the fingerprint. Trials additionally get `revoked_at` set on their row.
pub fn revoke(conn: &Connection, fingerprint: &str, licensee: &str, reason: &str) -> Result<()> {
    let fp = normalize_fingerprint(fingerprint)?;
    let now = Utc::now().to_rfc3339();
    conn.execute(
        "INSERT INTO revocations (fingerprint, licensee, reason, revoked_at) VALUES (?1, ?2, ?3, ?4)
         ON CONFLICT(fingerprint, licensee) DO UPDATE SET reason = excluded.reason, revoked_at = excluded.revoked_at",
        params![&fp, licensee, reason, &now],
    )?;
    conn.execute(
        "UPDATE trials SET revoked_at = ?2, revoked_reason = ?3 WHERE fingerprint = ?1",
        params![&fp, &now, reason],
    )?;
    Ok(())
}

/// Remove revocations for a fingerprint (all licensees when `licensee` is `None`).
pub fn unrevoke(conn: &Connection, fingerprint: &str, licensee: Option<&str>) -> Result<usize> {
    let fp = normalize_fingerprint(fingerprint)?;
    let n = match licensee {
        Some(l) => conn.execute(
            "DELETE FROM revocations WHERE fingerprint = ?1 AND licensee = ?2",
            params![&fp, l],
        )?,
        None => conn.execute(
            "DELETE FROM revocations WHERE fingerprint = ?1",
            params![&fp],
        )?,
    };
    conn.execute(
        "UPDATE trials SET revoked_at = NULL, revoked_reason = NULL WHERE fingerprint = ?1",
        params![&fp],
    )?;
    Ok(n)
}

// ---------------------------------------------------------------------------
// Request / response shapes
// ---------------------------------------------------------------------------

#[derive(Debug, Deserialize)]
struct TrialStart {
    fingerprint: String,
    #[serde(default)]
    email: Option<String>,
    #[serde(default)]
    product: Option<String>,
    #[serde(default)]
    version: Option<String>,
}

#[derive(Debug, Serialize)]
struct TrialResponse {
    /// Full signed license file (same shape as LICENSE_FILE).
    license_file: Value,
    expires_at: DateTime<Utc>,
    reused: bool,
}

#[derive(Debug, Deserialize)]
struct HeartbeatRequest {
    fingerprint: String,
    license_file: Value,
    #[serde(default)]
    product: Option<String>,
    #[serde(default)]
    version: Option<String>,
}

#[derive(Debug, Serialize)]
struct HeartbeatResponse {
    license_file: Value,
    expires_at: DateTime<Utc>,
    issued_at: DateTime<Utc>,
}

/// JSON error envelope matching the API's `{ "error": { code, message } }`.
#[derive(Debug)]
struct ServerError {
    status: StatusCode,
    code: &'static str,
    message: String,
}

impl ServerError {
    fn new(status: StatusCode, code: &'static str, message: impl Into<String>) -> Self {
        Self {
            status,
            code,
            message: message.into(),
        }
    }
    fn internal(err: impl std::fmt::Display) -> Self {
        warn!(error = %err, "license server internal error");
        Self::new(
            StatusCode::INTERNAL_SERVER_ERROR,
            "internal_error",
            "internal error",
        )
    }
}

impl IntoResponse for ServerError {
    fn into_response(self) -> Response {
        (
            self.status,
            Json(serde_json::json!({ "error": { "code": self.code, "message": self.message } })),
        )
            .into_response()
    }
}

// ---------------------------------------------------------------------------
// Shared guards
// ---------------------------------------------------------------------------

fn authorize(state: &AppState, headers: &HeaderMap) -> Result<(), ServerError> {
    let token = headers
        .get(axum::http::header::AUTHORIZATION)
        .and_then(|v| v.to_str().ok())
        .and_then(|v| v.strip_prefix("Bearer "))
        .unwrap_or("");
    if !constant_time_eq(token.as_bytes(), state.cfg.activation_token.as_bytes()) {
        return Err(ServerError::new(
            StatusCode::UNAUTHORIZED,
            "unauthorized",
            "unauthorized",
        ));
    }
    Ok(())
}

fn constant_time_eq(a: &[u8], b: &[u8]) -> bool {
    if a.len() != b.len() {
        return false;
    }
    a.iter().zip(b).fold(0u8, |acc, (x, y)| acc | (x ^ y)) == 0
}

fn normalize_fingerprint(raw: &str) -> Result<String> {
    let fp = raw.trim().to_ascii_lowercase();
    if fp.len() < 16 || fp.len() > 128 || !fp.chars().all(|c| c.is_ascii_hexdigit()) {
        bail!("fingerprint must be 16–128 hex chars");
    }
    Ok(fp)
}

fn rate_limit(state: &AppState, fp: &str) -> Result<(), ServerError> {
    let now = Instant::now();
    {
        let mut g = state
            .global
            .lock()
            .map_err(|_| ServerError::internal("global rate lock poisoned"))?;
        if now.duration_since(g.0) >= Duration::from_secs(1) {
            *g = (now, 0);
        }
        if g.1 >= GLOBAL_PER_SECOND {
            return Err(ServerError::new(
                StatusCode::TOO_MANY_REQUESTS,
                "rate_limited",
                "slow down",
            ));
        }
        g.1 += 1;
    }
    let mut per = state
        .per_fingerprint
        .lock()
        .map_err(|_| ServerError::internal("rate lock poisoned"))?;
    if let Some(prev) = per.get(fp) {
        if now.duration_since(*prev) < PER_FINGERPRINT_INTERVAL {
            return Err(ServerError::new(
                StatusCode::TOO_MANY_REQUESTS,
                "rate_limited",
                "slow down",
            ));
        }
    }
    per.insert(fp.to_string(), now);
    if per.len() > 10_000 {
        per.retain(|_, seen| now.duration_since(*seen) < Duration::from_secs(300));
    }
    Ok(())
}

fn short(fp: &str) -> &str {
    &fp[..12.min(fp.len())]
}

fn parse_stored_ts(s: &str) -> Result<DateTime<Utc>> {
    DateTime::parse_from_rfc3339(s)
        .map(|d| d.with_timezone(&Utc))
        .or_else(|_| {
            chrono::NaiveDateTime::parse_from_str(s, "%Y-%m-%d %H:%M:%S").map(|n| n.and_utc())
        })
        .with_context(|| format!("parsing stored timestamp {s}"))
}

fn sign(state: &AppState, doc: &LicenseDoc) -> Result<Value, ServerError> {
    let signed =
        license::sign_license(doc, &state.cfg.signing_key_pem).map_err(ServerError::internal)?;
    serde_json::from_str(&signed).map_err(ServerError::internal)
}

// ---------------------------------------------------------------------------
// POST /v1/trial/start
// ---------------------------------------------------------------------------

#[derive(Debug)]
struct TrialRow {
    expires_at: DateTime<Utc>,
    email: Option<String>,
    revoked_reason: Option<String>,
}

async fn trial_start(
    State(state): State<AppState>,
    headers: HeaderMap,
    Json(req): Json<TrialStart>,
) -> Result<Json<TrialResponse>, ServerError> {
    authorize(&state, &headers)?;
    let fp = normalize_fingerprint(&req.fingerprint)
        .map_err(|e| ServerError::new(StatusCode::BAD_REQUEST, "bad_request", e.to_string()))?;
    // Cheap syntactic checks run before the limiter so a typo does not cost
    // the operator a 5s wait; the limiter still guards every DB touch.
    let email = match req
        .email
        .as_deref()
        .map(str::trim)
        .filter(|e| !e.is_empty())
    {
        Some(e) => validate_trial_email(e).map_err(|e| {
            ServerError::new(StatusCode::BAD_REQUEST, "invalid_email", e.to_string())
        })?,
        None => {
            return Err(ServerError::new(
                StatusCode::BAD_REQUEST,
                "email_required",
                "a contact email is required to start a trial",
            ))
        }
    };
    rate_limit(&state, &fp)?;

    let db = state
        .db
        .lock()
        .map_err(|_| ServerError::internal("db lock poisoned"))?;

    let existing: Option<TrialRow> = db
        .query_row(
            "SELECT expires_at, email, revoked_reason FROM trials WHERE fingerprint = ?1",
            params![&fp],
            |row| {
                Ok((
                    row.get::<_, String>(0)?,
                    row.get::<_, Option<String>>(1)?,
                    row.get::<_, Option<String>>(2)?,
                ))
            },
        )
        .optional()
        .map_err(ServerError::internal)?
        .map(|(exp, email, revoked)| {
            parse_stored_ts(&exp).map(|expires_at| TrialRow {
                expires_at,
                email,
                revoked_reason: revoked,
            })
        })
        .transpose()
        .map_err(ServerError::internal)?;

    let now = Utc::now();
    let (expires_at, reused, licensee) = match existing {
        Some(row) => {
            if let Some(reason) = row.revoked_reason {
                warn!(fingerprint = short(&fp), "trial start refused: revoked");
                return Err(ServerError::new(
                    StatusCode::FORBIDDEN,
                    "revoked",
                    format!("trial for this install was revoked: {reason}"),
                ));
            }
            if row.expires_at <= now {
                warn!(fingerprint = short(&fp), expires_at = %row.expires_at, "trial start refused: expired");
                return Err(ServerError::new(
                    StatusCode::PAYMENT_REQUIRED,
                    "trial_expired",
                    format!(
                        "trial for this install already expired at {}",
                        row.expires_at
                    ),
                ));
            }
            // Keep the original lead e-mail; a different one on re-activation
            // is informational only.
            if row.email.as_deref().is_some_and(|e| e != email) {
                info!(
                    fingerprint = short(&fp),
                    "trial re-activated with a different email; keeping original"
                );
            }
            if row.email.is_none() {
                db.execute(
                    "UPDATE trials SET email = ?2 WHERE fingerprint = ?1",
                    params![&fp, &email],
                )
                .map_err(ServerError::internal)?;
            }
            info!(fingerprint = short(&fp), expires_at = %row.expires_at, "reusing trial");
            (
                row.expires_at,
                true,
                row.email.unwrap_or_else(|| email.clone()),
            )
        }
        None => {
            let expires_at = now + ChronoDuration::days(state.cfg.trial_days);
            db.execute(
                "INSERT INTO trials (fingerprint, email, started_at, expires_at, product, version, last_heartbeat_at, heartbeat_count)
                 VALUES (?1, ?2, ?3, ?4, ?5, ?6, ?3, 0)",
                params![
                    &fp,
                    &email,
                    now.to_rfc3339(),
                    expires_at.to_rfc3339(),
                    req.product.as_deref().unwrap_or("migration-platform"),
                    req.version.as_deref().unwrap_or(""),
                ],
            )
            .map_err(ServerError::internal)?;
            info!(fingerprint = short(&fp), %expires_at, "started new trial");
            (expires_at, false, email.clone())
        }
    };
    drop(db);

    let doc = LicenseDoc {
        licensee,
        expires_at,
        features: vec!["core".into()],
        max_seats: Some(5),
        issued_at: Some(now),
        kind: LicenseKind::Trial,
        fingerprint: Some(fp),
        requires_heartbeat: true,
    };
    let license_file = sign(&state, &doc)?;
    Ok(Json(TrialResponse {
        license_file,
        expires_at,
        reused,
    }))
}

// ---------------------------------------------------------------------------
// POST /v1/heartbeat
// ---------------------------------------------------------------------------

async fn heartbeat(
    State(state): State<AppState>,
    headers: HeaderMap,
    Json(req): Json<HeartbeatRequest>,
) -> Result<Json<HeartbeatResponse>, ServerError> {
    authorize(&state, &headers)?;
    let fp = normalize_fingerprint(&req.fingerprint)
        .map_err(|e| ServerError::new(StatusCode::BAD_REQUEST, "bad_request", e.to_string()))?;
    rate_limit(&state, &fp)?;

    // The presented file must be vendor-signed. `verify_license` also rejects
    // expired documents, which we surface as 402 rather than 400.
    let raw = serde_json::to_string(&req.license_file).map_err(ServerError::internal)?;
    let mut doc = match license::verify_license(&raw, &state.verify_key) {
        Ok(d) => d,
        Err(e) if e.to_string().contains("expired") => {
            return Err(ServerError::new(
                StatusCode::PAYMENT_REQUIRED,
                "expired",
                e.to_string(),
            ))
        }
        Err(e) => {
            warn!(fingerprint = short(&fp), error = %e, "heartbeat presented an invalid license");
            return Err(ServerError::new(
                StatusCode::BAD_REQUEST,
                "invalid_license",
                "presented license is not valid",
            ));
        }
    };
    if doc.fingerprint.as_deref() != Some(fp.as_str()) {
        return Err(ServerError::new(
            StatusCode::BAD_REQUEST,
            "fingerprint_mismatch",
            "license is not bound to the presenting install",
        ));
    }

    let db = state
        .db
        .lock()
        .map_err(|_| ServerError::internal("db lock poisoned"))?;

    let revoked: Option<String> = db
        .query_row(
            "SELECT reason FROM revocations WHERE fingerprint = ?1 AND (licensee = ?2 OR licensee = '*') LIMIT 1",
            params![&fp, &doc.licensee],
            |row| row.get::<_, Option<String>>(0),
        )
        .optional()
        .map_err(ServerError::internal)?
        .map(|r| r.unwrap_or_else(|| "revoked".into()));
    if let Some(reason) = revoked {
        warn!(fingerprint = short(&fp), licensee = %doc.licensee, "heartbeat refused: revoked");
        return Err(ServerError::new(
            StatusCode::FORBIDDEN,
            "revoked",
            format!("license revoked: {reason}"),
        ));
    }

    let now = Utc::now();
    if doc.kind == LicenseKind::Trial {
        let row: Option<(String, Option<String>)> = db
            .query_row(
                "SELECT expires_at, revoked_reason FROM trials WHERE fingerprint = ?1",
                params![&fp],
                |row| Ok((row.get(0)?, row.get(1)?)),
            )
            .optional()
            .map_err(ServerError::internal)?;
        match row {
            Some((_, Some(reason))) => {
                return Err(ServerError::new(
                    StatusCode::FORBIDDEN,
                    "revoked",
                    format!("trial revoked: {reason}"),
                ))
            }
            Some((exp, None)) => {
                // The server record is authoritative for trial expiry.
                let server_exp = parse_stored_ts(&exp).map_err(ServerError::internal)?;
                if server_exp <= now {
                    return Err(ServerError::new(
                        StatusCode::PAYMENT_REQUIRED,
                        "trial_expired",
                        format!("trial expired at {server_exp}"),
                    ));
                }
                doc.expires_at = server_exp;
                db.execute(
                    "UPDATE trials SET last_heartbeat_at = ?2, heartbeat_count = heartbeat_count + 1,
                        product = COALESCE(?3, product), version = COALESCE(?4, version)
                     WHERE fingerprint = ?1",
                    params![&fp, now.to_rfc3339(), req.product, req.version],
                )
                .map_err(ServerError::internal)?;
            }
            None => {
                // Signature proves vendor issuance; the row may have been lost
                // to a restore. Accept but make the gap visible.
                warn!(
                    fingerprint = short(&fp),
                    "heartbeat for trial with no server record; accepting signed document"
                );
            }
        }
    }
    drop(db);

    doc.issued_at = Some(now);
    doc.requires_heartbeat = true;
    let license_file = sign(&state, &doc)?;
    info!(fingerprint = short(&fp), licensee = %doc.licensee, kind = ?doc.kind, "heartbeat ok");
    Ok(Json(HeartbeatResponse {
        license_file,
        expires_at: doc.expires_at,
        issued_at: now,
    }))
}

#[cfg(test)]
mod tests {
    use super::*;
    use axum::body::Body;
    use axum::http::Request;
    use http_body_util::BodyExt;
    use tower::ServiceExt;

    const TOKEN: &str = "0123456789abcdef0123456789abcdef";

    fn signing_pem() -> (String, RsaPublicKey) {
        let mut rng = rand::thread_rng();
        let priv_key = RsaPrivateKey::new(&mut rng, 2048).unwrap();
        let pub_key = RsaPublicKey::from(&priv_key);
        let pem = rsa::pkcs8::EncodePrivateKey::to_pkcs8_pem(&priv_key, rsa::pkcs8::LineEnding::LF)
            .unwrap()
            .to_string();
        (pem, pub_key)
    }

    fn temp_db() -> String {
        let dir = std::env::temp_dir().join(format!("license-server-{}", uuid::Uuid::new_v4()));
        std::fs::create_dir_all(&dir).unwrap();
        dir.join("trials.db").display().to_string()
    }

    struct Harness {
        app: Router,
        state: AppState,
        pub_key: RsaPublicKey,
    }

    fn harness() -> Harness {
        let (pem, pub_key) = signing_pem();
        let db = open_db(&temp_db()).unwrap();
        let state = AppState::new(
            db,
            ServerConfig {
                signing_key_pem: pem,
                activation_token: TOKEN.into(),
                trial_days: 10,
            },
        )
        .unwrap();
        Harness {
            app: build_router(state.clone()),
            state,
            pub_key,
        }
    }

    async fn post(
        app: &Router,
        path: &str,
        token: Option<&str>,
        body: Value,
    ) -> (StatusCode, Value) {
        let mut req = Request::builder()
            .method("POST")
            .uri(path)
            .header("content-type", "application/json");
        if let Some(t) = token {
            req = req.header("authorization", format!("Bearer {t}"));
        }
        let resp = app
            .clone()
            .oneshot(req.body(Body::from(body.to_string())).unwrap())
            .await
            .unwrap();
        let status = resp.status();
        let bytes = resp.into_body().collect().await.unwrap().to_bytes();
        let json = serde_json::from_slice(&bytes).unwrap_or(Value::Null);
        (status, json)
    }

    fn fp(seed: &str) -> String {
        seed.repeat(32)
    }

    fn fresh_fp() -> String {
        uuid::Uuid::new_v4().simple().to_string() + &uuid::Uuid::new_v4().simple().to_string()
    }

    #[test]
    fn open_db_is_idempotent_and_migrates_columns() {
        let path = temp_db();
        {
            let conn = Connection::open(&path).unwrap();
            // Simulate the pre-P5.1 schema.
            conn.execute_batch(
                "CREATE TABLE trials (fingerprint TEXT PRIMARY KEY NOT NULL, email TEXT, started_at TEXT NOT NULL,
                 expires_at TEXT NOT NULL, product TEXT, version TEXT, created_at TEXT NOT NULL DEFAULT (datetime('now')));",
            )
            .unwrap();
        }
        let conn = open_db(&path).unwrap();
        let cols: Vec<String> = conn
            .prepare("PRAGMA table_info(trials)")
            .unwrap()
            .query_map([], |r| r.get::<_, String>(1))
            .unwrap()
            .map(Result::unwrap)
            .collect();
        for c in [
            "last_heartbeat_at",
            "heartbeat_count",
            "revoked_at",
            "revoked_reason",
        ] {
            assert!(cols.iter().any(|x| x == c), "missing column {c}");
        }
        drop(conn);
        open_db(&path).unwrap();
    }

    #[test]
    fn config_validation() {
        let (pem, _) = signing_pem();
        assert!(ServerConfig {
            signing_key_pem: pem.clone(),
            activation_token: "short".into(),
            trial_days: 10
        }
        .validate()
        .is_err());
        assert!(ServerConfig {
            signing_key_pem: pem.clone(),
            activation_token: TOKEN.into(),
            trial_days: 0
        }
        .validate()
        .is_err());
        assert!(ServerConfig {
            signing_key_pem: "not a key".into(),
            activation_token: TOKEN.into(),
            trial_days: 10
        }
        .validate()
        .is_err());
        assert!(ServerConfig {
            signing_key_pem: pem,
            activation_token: TOKEN.into(),
            trial_days: 10
        }
        .validate()
        .is_ok());
    }

    #[tokio::test]
    async fn trial_requires_auth_and_email() {
        let h = harness();
        let (status, body) = post(
            &h.app,
            "/v1/trial/start",
            None,
            serde_json::json!({"fingerprint": fp("a"), "email": "ops@example.com"}),
        )
        .await;
        assert_eq!(status, StatusCode::UNAUTHORIZED);
        assert_eq!(body["error"]["code"], "unauthorized");

        let (status, body) = post(
            &h.app,
            "/v1/trial/start",
            Some(TOKEN),
            serde_json::json!({"fingerprint": fp("b")}),
        )
        .await;
        assert_eq!(status, StatusCode::BAD_REQUEST);
        assert_eq!(body["error"]["code"], "email_required");

        let (status, body) = post(
            &h.app,
            "/v1/trial/start",
            Some(TOKEN),
            serde_json::json!({"fingerprint": fp("c"), "email": "not-an-email"}),
        )
        .await;
        assert_eq!(status, StatusCode::BAD_REQUEST);
        assert_eq!(body["error"]["code"], "invalid_email");

        let (status, body) = post(
            &h.app,
            "/v1/trial/start",
            Some(TOKEN),
            serde_json::json!({"fingerprint": "zz", "email": "ops@example.com"}),
        )
        .await;
        assert_eq!(status, StatusCode::BAD_REQUEST);
        assert_eq!(body["error"]["code"], "bad_request");
    }

    #[tokio::test]
    async fn trial_issues_heartbeat_bound_license_and_reuses_expiry() {
        let h = harness();
        let fp = fresh_fp();
        let (status, body) = post(
            &h.app,
            "/v1/trial/start",
            Some(TOKEN),
            serde_json::json!({"fingerprint": fp, "email": "Ops@Example.com", "product": "mp", "version": "1.0"}),
        )
        .await;
        assert_eq!(status, StatusCode::OK, "{body}");
        assert_eq!(body["reused"], false);
        let raw = body["license_file"].to_string();
        let doc = license::verify_license(&raw, &h.pub_key).unwrap();
        assert_eq!(doc.kind, LicenseKind::Trial);
        assert_eq!(doc.licensee, "ops@example.com");
        assert_eq!(doc.fingerprint.as_deref(), Some(fp.as_str()));
        assert!(doc.requires_heartbeat);
        let first_expiry = doc.expires_at;

        // Second activation for the same install: same expiry, flagged reused,
        // even with a different e-mail (original lead kept). Bypass the
        // per-fingerprint rate limit by clearing it as an operator restart would.
        h.state.per_fingerprint.lock().unwrap().clear();
        let (status, body) = post(
            &h.app,
            "/v1/trial/start",
            Some(TOKEN),
            serde_json::json!({"fingerprint": fp, "email": "someone-else@example.com"}),
        )
        .await;
        assert_eq!(status, StatusCode::OK, "{body}");
        assert_eq!(body["reused"], true);
        let doc2 = license::verify_license(&body["license_file"].to_string(), &h.pub_key).unwrap();
        assert_eq!(doc2.expires_at, first_expiry);
        assert_eq!(doc2.licensee, "ops@example.com");
    }

    #[tokio::test]
    async fn trial_rate_limited_per_fingerprint() {
        let h = harness();
        let fp = fresh_fp();
        let body = serde_json::json!({"fingerprint": fp, "email": "ops@example.com"});
        let (status, _) = post(&h.app, "/v1/trial/start", Some(TOKEN), body.clone()).await;
        assert_eq!(status, StatusCode::OK);
        let (status, err) = post(&h.app, "/v1/trial/start", Some(TOKEN), body).await;
        assert_eq!(status, StatusCode::TOO_MANY_REQUESTS);
        assert_eq!(err["error"]["code"], "rate_limited");
    }

    #[tokio::test]
    async fn expired_trial_is_refused() {
        let h = harness();
        let fp = fresh_fp();
        {
            let db = h.state.db.lock().unwrap();
            let past = (Utc::now() - ChronoDuration::days(1)).to_rfc3339();
            db.execute(
                "INSERT INTO trials (fingerprint, email, started_at, expires_at) VALUES (?1, 'x@example.com', ?2, ?2)",
                params![&fp, past],
            )
            .unwrap();
        }
        let (status, body) = post(
            &h.app,
            "/v1/trial/start",
            Some(TOKEN),
            serde_json::json!({"fingerprint": fp, "email": "x@example.com"}),
        )
        .await;
        assert_eq!(status, StatusCode::PAYMENT_REQUIRED);
        assert_eq!(body["error"]["code"], "trial_expired");
    }

    #[tokio::test]
    async fn heartbeat_refreshes_issued_at_and_tracks_trial() {
        let h = harness();
        let fp = fresh_fp();
        let (status, body) = post(
            &h.app,
            "/v1/trial/start",
            Some(TOKEN),
            serde_json::json!({"fingerprint": fp, "email": "ops@example.com"}),
        )
        .await;
        assert_eq!(status, StatusCode::OK);
        let issued_file = body["license_file"].clone();
        let issued = license::verify_license(&issued_file.to_string(), &h.pub_key).unwrap();

        tokio::time::sleep(std::time::Duration::from_millis(20)).await;
        h.state.per_fingerprint.lock().unwrap().clear();
        let (status, body) = post(
            &h.app,
            "/v1/heartbeat",
            Some(TOKEN),
            serde_json::json!({"fingerprint": fp, "license_file": issued_file, "version": "1.1"}),
        )
        .await;
        assert_eq!(status, StatusCode::OK, "{body}");
        let refreshed =
            license::verify_license(&body["license_file"].to_string(), &h.pub_key).unwrap();
        assert!(refreshed.issued_at.unwrap() > issued.issued_at.unwrap());
        assert_eq!(refreshed.expires_at, issued.expires_at);
        assert_eq!(refreshed.licensee, issued.licensee);
        assert!(refreshed.requires_heartbeat);

        let db = h.state.db.lock().unwrap();
        let (count, version): (i64, String) = db
            .query_row(
                "SELECT heartbeat_count, version FROM trials WHERE fingerprint = ?1",
                params![&fp],
                |r| Ok((r.get(0)?, r.get(1)?)),
            )
            .unwrap();
        assert_eq!(count, 1);
        assert_eq!(version, "1.1");
    }

    #[tokio::test]
    async fn heartbeat_rejects_forged_and_mismatched_licenses() {
        let h = harness();
        let fp = fresh_fp();

        // Signed by someone else's key.
        let (other_pem, _) = signing_pem();
        let doc = LicenseDoc {
            licensee: "Evil".into(),
            expires_at: Utc::now() + ChronoDuration::days(30),
            features: vec![],
            max_seats: None,
            issued_at: Some(Utc::now()),
            kind: LicenseKind::Commercial,
            fingerprint: Some(fp.clone()),
            requires_heartbeat: true,
        };
        let forged: Value =
            serde_json::from_str(&license::sign_license(&doc, &other_pem).unwrap()).unwrap();
        let (status, body) = post(
            &h.app,
            "/v1/heartbeat",
            Some(TOKEN),
            serde_json::json!({"fingerprint": fp, "license_file": forged}),
        )
        .await;
        assert_eq!(status, StatusCode::BAD_REQUEST);
        assert_eq!(body["error"]["code"], "invalid_license");

        // Genuine, but bound to a different install.
        let other_fp = fresh_fp();
        let mut bound_elsewhere = doc.clone();
        bound_elsewhere.fingerprint = Some(other_fp);
        let genuine: Value = serde_json::from_str(
            &license::sign_license(&bound_elsewhere, &h.state.cfg.signing_key_pem).unwrap(),
        )
        .unwrap();
        h.state.per_fingerprint.lock().unwrap().clear();
        let (status, body) = post(
            &h.app,
            "/v1/heartbeat",
            Some(TOKEN),
            serde_json::json!({"fingerprint": fp, "license_file": genuine}),
        )
        .await;
        assert_eq!(status, StatusCode::BAD_REQUEST);
        assert_eq!(body["error"]["code"], "fingerprint_mismatch");

        // Genuine but expired → 402.
        let mut expired = doc.clone();
        expired.expires_at = Utc::now() - ChronoDuration::seconds(5);
        let expired_file: Value = serde_json::from_str(
            &license::sign_license(&expired, &h.state.cfg.signing_key_pem).unwrap(),
        )
        .unwrap();
        h.state.per_fingerprint.lock().unwrap().clear();
        let (status, body) = post(
            &h.app,
            "/v1/heartbeat",
            Some(TOKEN),
            serde_json::json!({"fingerprint": fp, "license_file": expired_file}),
        )
        .await;
        assert_eq!(status, StatusCode::PAYMENT_REQUIRED);
        assert_eq!(body["error"]["code"], "expired");
    }

    #[tokio::test]
    async fn heartbeat_honours_revocation_for_commercial_and_trial() {
        let h = harness();
        let fp = fresh_fp();
        let doc = LicenseDoc {
            licensee: "Acme".into(),
            expires_at: Utc::now() + ChronoDuration::days(365),
            features: vec!["core".into()],
            max_seats: Some(10),
            issued_at: Some(Utc::now()),
            kind: LicenseKind::Commercial,
            fingerprint: Some(fp.clone()),
            requires_heartbeat: true,
        };
        let file: Value = serde_json::from_str(
            &license::sign_license(&doc, &h.state.cfg.signing_key_pem).unwrap(),
        )
        .unwrap();

        let (status, _) = post(
            &h.app,
            "/v1/heartbeat",
            Some(TOKEN),
            serde_json::json!({"fingerprint": fp, "license_file": file}),
        )
        .await;
        assert_eq!(status, StatusCode::OK);

        revoke(&h.state.db.lock().unwrap(), &fp, "Acme", "chargeback").unwrap();
        h.state.per_fingerprint.lock().unwrap().clear();
        let (status, body) = post(
            &h.app,
            "/v1/heartbeat",
            Some(TOKEN),
            serde_json::json!({"fingerprint": fp, "license_file": file}),
        )
        .await;
        assert_eq!(status, StatusCode::FORBIDDEN);
        assert_eq!(body["error"]["code"], "revoked");
        assert!(body["error"]["message"]
            .as_str()
            .unwrap()
            .contains("chargeback"));

        unrevoke(&h.state.db.lock().unwrap(), &fp, None).unwrap();
        h.state.per_fingerprint.lock().unwrap().clear();
        let (status, _) = post(
            &h.app,
            "/v1/heartbeat",
            Some(TOKEN),
            serde_json::json!({"fingerprint": fp, "license_file": file}),
        )
        .await;
        assert_eq!(status, StatusCode::OK);

        // Wildcard revocation blocks trial start too.
        let trial_fp = fresh_fp();
        let (status, _) = post(
            &h.app,
            "/v1/trial/start",
            Some(TOKEN),
            serde_json::json!({"fingerprint": trial_fp, "email": "t@example.com"}),
        )
        .await;
        assert_eq!(status, StatusCode::OK);
        revoke(&h.state.db.lock().unwrap(), &trial_fp, "*", "abuse").unwrap();
        h.state.per_fingerprint.lock().unwrap().clear();
        let (status, body) = post(
            &h.app,
            "/v1/trial/start",
            Some(TOKEN),
            serde_json::json!({"fingerprint": trial_fp, "email": "t@example.com"}),
        )
        .await;
        assert_eq!(status, StatusCode::FORBIDDEN);
        assert_eq!(body["error"]["code"], "revoked");
    }
}
