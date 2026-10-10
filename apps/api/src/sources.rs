//! Organization-owned source registry (ADR 0001, MP-01).
//!
//! Public requests *name* a source; they never describe where to read from.
//! [`parse_source_request`] accepts exactly three shapes and [`resolve`] turns
//! them into a registry row (or an org-owned connector) for the caller's
//! organization. The workflow input is then built from the resolved value, so
//! client JSON is never forwarded to the orchestrator.

use serde::Serialize;
use serde_json::{json, Map, Value};
use sqlx::PgPool;

use crate::error::{ApiError, ApiResult};

/// Query options a connector-backed job may set. Endpoints and credentials
/// always come from the stored connector.
const CONNECTOR_EXTRA_KEYS: &[&str] = &["soql", "cursorField", "cursor"];

/// Connector kinds that can feed a job directly (watched kinds run via schedules).
const JOB_CONNECTOR_KINDS: &[&str] = &["salesforce"];

#[derive(Debug, Clone, PartialEq)]
pub enum SourceRequest {
    ById(i64),
    Object {
        bucket: String,
        key: String,
    },
    Connector {
        connector_id: i64,
        extra: Map<String, Value>,
    },
}

#[derive(Debug, Clone, sqlx::FromRow, Serialize)]
pub struct RegisteredSource {
    pub id: i64,
    pub org_id: i64,
    pub bucket: String,
    pub object_key: String,
    pub sha256: Option<String>,
    pub etag: Option<String>,
    pub size_bytes: Option<i64>,
    pub filename: Option<String>,
    pub format: Option<String>,
}

#[derive(Debug, Clone)]
pub enum ResolvedSource {
    Object(RegisteredSource),
    Connector {
        connector_id: i64,
        kind: String,
        extra: Map<String, Value>,
    },
}

const ACCEPTED: &str = "expected {\"source_id\": N}, an upload source_ref \
     ({\"type\":\"minio\",\"bucket\",\"key\"} from POST /files), or \
     {\"type\":\"connector\",\"connector_id\": N, \"extra\": {...}}";

/// Parse a client `source_ref` without touching the database.
pub fn parse_source_request(v: &Value) -> ApiResult<SourceRequest> {
    let obj = v.as_object().ok_or_else(|| {
        ApiError::BadRequest(format!("source_ref must be a JSON object; {ACCEPTED}"))
    })?;

    if let Some(id) = obj.get("source_id") {
        let id = id
            .as_i64()
            .filter(|n| *n > 0)
            .ok_or_else(|| ApiError::BadRequest("source_id must be a positive integer".into()))?;
        return Ok(SourceRequest::ById(id));
    }

    for forbidden in ["path", "url", "secret"] {
        if obj.contains_key(forbidden) {
            return Err(ApiError::BadRequest(format!(
                "source_ref.{forbidden} is not accepted; {ACCEPTED}"
            )));
        }
    }

    let kind = obj.get("type").and_then(Value::as_str).unwrap_or("");
    let connector_id = obj
        .get("connector_id")
        .or_else(|| obj.get("connectorId"))
        .map(|v| {
            v.as_i64().filter(|n| *n > 0).ok_or_else(|| {
                ApiError::BadRequest("connector_id must be a positive integer".into())
            })
        })
        .transpose()?;

    match (kind, connector_id) {
        ("minio", _) => {
            let field = |name: &str| {
                obj.get(name)
                    .and_then(Value::as_str)
                    .map(str::trim)
                    .filter(|s| !s.is_empty())
                    .map(str::to_string)
                    .ok_or_else(|| ApiError::BadRequest(format!("source_ref.{name} is required")))
            };
            Ok(SourceRequest::Object {
                bucket: field("bucket")?,
                key: field("key")?,
            })
        }
        ("connector", Some(id)) | ("salesforce", Some(id)) => {
            let extra = match obj.get("extra") {
                None | Some(Value::Null) => Map::new(),
                Some(Value::Object(m)) => m.clone(),
                Some(_) => {
                    return Err(ApiError::BadRequest(
                        "source_ref.extra must be an object".into(),
                    ))
                }
            };
            for (k, v) in &extra {
                if !CONNECTOR_EXTRA_KEYS.contains(&k.as_str()) {
                    return Err(ApiError::BadRequest(format!(
                        "source_ref.extra.{k} is not allowed; allowed: {}",
                        CONNECTOR_EXTRA_KEYS.join(", ")
                    )));
                }
                if !v.is_string() {
                    return Err(ApiError::BadRequest(format!(
                        "source_ref.extra.{k} must be a string"
                    )));
                }
            }
            Ok(SourceRequest::Connector {
                connector_id: id,
                extra,
            })
        }
        ("connector", None) => Err(ApiError::BadRequest(
            "source_ref.connector_id is required for connector sources".into(),
        )),
        _ => Err(ApiError::BadRequest(format!(
            "unsupported source_ref type {kind:?}; {ACCEPTED}"
        ))),
    }
}

const SOURCE_COLS: &str =
    "id, org_id, bucket, object_key, sha256, etag, size_bytes, filename, format";

/// Resolve a request against the caller's organization. Sources owned by
/// another organization are indistinguishable from missing ones.
pub async fn resolve(db: &PgPool, org: i64, req: &SourceRequest) -> ApiResult<ResolvedSource> {
    match req {
        SourceRequest::ById(id) => {
            let row: Option<RegisteredSource> = sqlx::query_as(&format!(
                "SELECT {SOURCE_COLS} FROM sources WHERE id = $1 AND org_id = $2"
            ))
            .bind(id)
            .bind(org)
            .fetch_optional(db)
            .await?;
            row.map(ResolvedSource::Object).ok_or_else(not_registered)
        }
        SourceRequest::Object { bucket, key } => lookup_object(db, org, bucket, key)
            .await?
            .map(ResolvedSource::Object)
            .ok_or_else(not_registered),
        SourceRequest::Connector {
            connector_id,
            extra,
        } => {
            let kind = require_connector(db, org, *connector_id).await?;
            if !JOB_CONNECTOR_KINDS.contains(&kind.as_str()) {
                return Err(ApiError::BadRequest(format!(
                    "connector kind {kind} cannot feed a job directly; use a schedule"
                )));
            }
            Ok(ResolvedSource::Connector {
                connector_id: *connector_id,
                kind,
                extra: extra.clone(),
            })
        }
    }
}

/// Fail unless `connector_id` belongs to `org`; returns the connector kind.
pub async fn require_connector(db: &PgPool, org: i64, connector_id: i64) -> ApiResult<String> {
    let kind: Option<String> = sqlx::query_scalar(
        "SELECT connector_kind::text FROM connectors WHERE id = $1 AND org_id = $2",
    )
    .bind(connector_id)
    .bind(org)
    .fetch_optional(db)
    .await?;
    kind.ok_or_else(|| ApiError::BadRequest(format!("connector {connector_id} not found")))
}

/// Resolve a request that must name a stored object (sampling, batches).
pub async fn resolve_object(
    db: &PgPool,
    org: i64,
    req: &SourceRequest,
) -> ApiResult<RegisteredSource> {
    match resolve(db, org, req).await? {
        ResolvedSource::Object(src) => Ok(src),
        ResolvedSource::Connector { .. } => Err(ApiError::BadRequest(
            "this operation needs an uploaded file source, not a connector".into(),
        )),
    }
}

pub async fn lookup_object(
    db: &PgPool,
    org: i64,
    bucket: &str,
    key: &str,
) -> ApiResult<Option<RegisteredSource>> {
    Ok(sqlx::query_as(&format!(
        "SELECT {SOURCE_COLS} FROM sources WHERE org_id = $1 AND bucket = $2 AND object_key = $3"
    ))
    .bind(org)
    .bind(bucket)
    .bind(key)
    .fetch_optional(db)
    .await?)
}

fn not_registered() -> ApiError {
    ApiError::BadRequest(
        "source not found for this organization; upload the file with POST /files first".into(),
    )
}

pub struct NewUpload<'a> {
    pub org: i64,
    pub created_by: Option<i64>,
    pub bucket: &'a str,
    pub key: &'a str,
    pub sha256: &'a str,
    pub etag: Option<&'a str>,
    pub size: i64,
    pub filename: &'a str,
}

/// Record a completed upload. The object key is content-addressed per org, so
/// a repeat upload of the same bytes refreshes the display filename and ETag.
pub async fn register_upload(db: &PgPool, u: NewUpload<'_>) -> ApiResult<RegisteredSource> {
    Ok(sqlx::query_as(&format!(
        "INSERT INTO sources (org_id, bucket, object_key, sha256, etag, size_bytes, filename, format, provenance, created_by)
         VALUES ($1, $2, $3, $4, $5, $6, $7, $8, 'upload', $9)
         ON CONFLICT (org_id, bucket, object_key) DO UPDATE
           SET filename = EXCLUDED.filename, format = EXCLUDED.format,
               etag = COALESCE(EXCLUDED.etag, sources.etag)
         RETURNING {SOURCE_COLS}"
    ))
    .bind(u.org)
    .bind(u.bucket)
    .bind(u.key)
    .bind(u.sha256)
    .bind(u.etag)
    .bind(u.size)
    .bind(u.filename)
    .bind(format_from_filename(u.filename))
    .bind(u.created_by)
    .fetch_one(db)
    .await?)
}

/// Same rules as the orchestrator's `connectors.FormatFromFilename`, plus
/// `archive` for batch packages.
pub fn format_from_filename(name: &str) -> &'static str {
    let lower = name.to_ascii_lowercase();
    if lower.ends_with(".tar.gz") || lower.ends_with(".tgz") || lower.ends_with(".zip") {
        "archive"
    } else if lower.ends_with(".json") || lower.ends_with(".ndjson") || lower.ends_with(".jsonl") {
        "json"
    } else if lower.ends_with(".xml") {
        "xml"
    } else {
        "csv"
    }
}

impl RegisteredSource {
    /// Canonical object reference stored on jobs and sent to the workflow.
    pub fn source_ref(&self) -> Value {
        json!({
            "type": "minio",
            "source_id": self.id,
            "bucket": self.bucket,
            "key": self.object_key,
            "filename": self.filename.clone().unwrap_or_else(|| file_name(&self.object_key)),
            "size": self.size_bytes,
            "sha256": self.sha256,
            "etag": self.etag,
        })
    }
}

impl ResolvedSource {
    pub fn source_ref(&self) -> Value {
        match self {
            ResolvedSource::Object(src) => src.source_ref(),
            ResolvedSource::Connector {
                connector_id,
                kind,
                extra,
            } => json!({
                "type": "connector",
                "connector_id": connector_id,
                "connector_kind": kind,
                "extra": extra,
            }),
        }
    }
}

fn file_name(key: &str) -> String {
    key.rsplit('/').next().unwrap_or(key).to_string()
}

/// The only object namespace an organization may configure (watched prefixes,
/// SFTP staging). Mirrors the orchestrator's `activities.OrgObjectPrefix`.
pub fn org_object_prefix(org: i64) -> String {
    format!("orgs/{org}/")
}

/// Validate user-configured object locations on watched connectors. The
/// orchestrator enforces the same rule at execution time.
pub fn check_connector_locations(
    kind: &str,
    cfg: &Value,
    org: i64,
    allowed_buckets: &[&str],
) -> ApiResult<()> {
    let text = |k: &str| cfg.get(k).and_then(Value::as_str).unwrap_or("").trim();
    let (bucket, prefix, field) = match kind {
        "watched_prefix" => {
            let bucket = Some(text("bucket"))
                .filter(|b| !b.is_empty())
                .unwrap_or(text("s3_bucket"));
            (bucket, text("prefix"), "prefix")
        }
        "watched_sftp" => {
            let bucket = [text("staging_bucket"), text("bucket")]
                .into_iter()
                .find(|b| !b.is_empty())
                .unwrap_or("migration");
            let prefix = text("staging_prefix");
            if prefix.trim_matches('/').is_empty() || prefix.trim_matches('/') == "sftp-landing" {
                return check_bucket(bucket, allowed_buckets);
            }
            (bucket, prefix, "staging_prefix")
        }
        _ => return Ok(()),
    };
    check_bucket(bucket, allowed_buckets)?;
    let p = prefix.trim_start_matches('/');
    let required = org_object_prefix(org);
    if p.contains('\\') || p.split('/').any(|seg| seg == "..") || !p.starts_with(&required) {
        return Err(ApiError::BadRequest(format!(
            "{field} must start with {required} (got {prefix:?})"
        )));
    }
    Ok(())
}

fn check_bucket(bucket: &str, allowed: &[&str]) -> ApiResult<()> {
    if allowed.contains(&bucket) {
        Ok(())
    } else {
        Err(ApiError::BadRequest(format!(
            "bucket {bucket:?} is not allowed for watched connectors; allowed: {}",
            allowed.join(", ")
        )))
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn bad(v: Value) -> String {
        match parse_source_request(&v) {
            Err(ApiError::BadRequest(m)) => m,
            other => panic!("expected BadRequest, got {other:?}"),
        }
    }

    #[test]
    fn accepts_source_id() {
        assert_eq!(
            parse_source_request(&json!({"source_id": 7})).unwrap(),
            SourceRequest::ById(7)
        );
    }

    #[test]
    fn accepts_upload_shape() {
        let v = json!({"type":"minio","bucket":"migration","key":"uploads/1/ab/x","filename":"a.csv","size":3});
        assert_eq!(
            parse_source_request(&v).unwrap(),
            SourceRequest::Object {
                bucket: "migration".into(),
                key: "uploads/1/ab/x".into()
            }
        );
    }

    #[test]
    fn accepts_connector_shapes() {
        let a = parse_source_request(
            &json!({"type":"connector","connector_id":3,"extra":{"soql":"SELECT Id FROM Account"}}),
        )
        .unwrap();
        let b = parse_source_request(
            &json!({"type":"salesforce","connectorId":3,"extra":{"soql":"SELECT Id FROM Account"}}),
        )
        .unwrap();
        assert_eq!(a, b);
    }

    #[test]
    fn rejects_non_object_and_json_string() {
        assert!(bad(json!("{\"type\":\"minio\"}")).contains("must be a JSON object"));
        assert!(bad(json!(null)).contains("must be a JSON object"));
    }

    #[test]
    fn rejects_local_paths_urls_and_inline_secrets() {
        assert!(bad(json!({"type":"csv","path":"/etc/passwd"})).contains("path"));
        assert!(bad(json!({"type":"csv","url":"http://169.254.169.254/"})).contains("url"));
        assert!(
            bad(json!({"type":"salesforce","secret":"tok","extra":{"soql":"x"}}))
                .contains("secret")
        );
    }

    #[test]
    fn rejects_unknown_types_and_missing_fields() {
        assert!(bad(json!({"type":"watched_prefix"})).contains("unsupported"));
        assert!(bad(json!({"type":"minio","bucket":"b"})).contains("key"));
        assert!(bad(json!({"type":"connector"})).contains("connector_id"));
        assert!(bad(json!({"source_id": -1})).contains("positive"));
    }

    #[test]
    fn connector_extra_cannot_override_endpoints_or_credentials() {
        assert!(bad(
            json!({"type":"connector","connector_id":1,"extra":{"instanceUrl":"http://evil"}})
        )
        .contains("not allowed"));
        assert!(
            bad(json!({"type":"connector","connector_id":1,"extra":{"tokenUrl":"x"}}))
                .contains("not allowed")
        );
        assert!(
            bad(json!({"type":"connector","connector_id":1,"extra":{"soql":1}})).contains("string")
        );
    }

    #[test]
    fn connector_locations_are_confined_to_the_org_namespace() {
        let allowed = ["migration", "inbound"];
        let check = |kind: &str, cfg: Value| check_connector_locations(kind, &cfg, 7, &allowed);
        assert!(check(
            "watched_prefix",
            json!({"bucket":"migration","prefix":"orgs/7/incoming/"})
        )
        .is_ok());
        assert!(check(
            "watched_prefix",
            json!({"bucket":"inbound","prefix":"/orgs/7/"})
        )
        .is_ok());
        assert!(check("watched_sftp", json!({"staging_prefix":"sftp-landing/"})).is_ok());
        assert!(check(
            "watched_sftp",
            json!({"staging_bucket":"migration","staging_prefix":"orgs/7/landing/"})
        )
        .is_ok());
        assert!(check("salesforce", json!({"instance_url":"https://x"})).is_ok());

        for bad in [
            json!({"bucket":"migration","prefix":"uploads/1/"}),
            json!({"bucket":"migration","prefix":""}),
            json!({"bucket":"migration","prefix":"orgs/70/"}),
            json!({"bucket":"migration","prefix":"orgs/7/../1/"}),
            json!({"bucket":"backups","prefix":"orgs/7/"}),
        ] {
            assert!(check("watched_prefix", bad.clone()).is_err(), "{bad}");
        }
        assert!(check("watched_sftp", json!({"staging_prefix":"uploads/1/"})).is_err());
        assert!(check("watched_sftp", json!({"staging_bucket":"backups"})).is_err());
    }

    #[test]
    fn format_detection_matches_orchestrator() {
        assert_eq!(format_from_filename("a.CSV"), "csv");
        assert_eq!(format_from_filename("a.ndjson"), "json");
        assert_eq!(format_from_filename("a.xml"), "xml");
        assert_eq!(format_from_filename("pkg.tar.gz"), "archive");
        assert_eq!(format_from_filename("noext"), "csv");
    }
}
