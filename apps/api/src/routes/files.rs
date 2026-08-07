//! File upload handler. Streams multipart bodies into MinIO/S3 while computing
//! a SHA-256 content hash that can be used as the object key for idempotent
//! uploads.
//!
//! Also exposes `POST /files/sample`, which fetches the first chunk of a
//! previously uploaded object out of S3 and asks `crate::sampling` to derive a
//! column palette + a few preview rows for the Designer UI.

use axum::extract::{Multipart, State};
use axum::response::IntoResponse;
use axum::routing::post;
use axum::{Json, Router};
use serde::{Deserialize, Serialize};
use serde_json::json;
use sha2::{Digest, Sha256};

use crate::error::{ApiError, ApiResult};
use crate::middleware::auth::{require_role, AuthUser};
use crate::sampling::{self, DEFAULT_BYTE_CAP, DEFAULT_ROW_CAP, MAX_ROW_CAP};
use crate::state::AppState;

pub fn router() -> Router<AppState> {
    Router::new()
        .route("/files", post(upload_file))
        .route("/files/sample", post(sample_file))
}

#[derive(Serialize)]
pub struct FileRef {
    #[serde(rename = "type")]
    pub kind: &'static str,
    pub bucket: String,
    pub key: String,
    pub size: i64,
    pub sha256: String,
    pub filename: String,
}

async fn upload_file(
    State(state): State<AppState>,
    AuthUser(claims): AuthUser,
    mut multipart: Multipart,
) -> ApiResult<impl IntoResponse> {
    require_role(&claims, &["admin", "editor", "operator"])?;
    while let Some(field) = multipart
        .next_field()
        .await
        .map_err(|e| ApiError::BadRequest(format!("multipart: {e}")))?
    {
        if field.name() != Some("file") {
            continue;
        }
        let filename = field
            .file_name()
            .map(str::to_string)
            .unwrap_or_else(|| "upload.bin".to_string());
        let bytes = field
            .bytes()
            .await
            .map_err(|e| ApiError::BadRequest(format!("read: {e}")))?;

        let mut hasher = Sha256::new();
        hasher.update(&bytes);
        let digest = hasher.finalize();
        let sha = hex::encode(digest);
        let key = format!("uploads/{}/{}/{}", claims.org, &sha[..2], sha);

        state
            .s3
            .put_object()
            .bucket(&state.cfg.minio_bucket)
            .key(&key)
            .body(bytes.clone().into())
            .content_type("application/octet-stream")
            .metadata("original-filename", &filename)
            .send()
            .await
            .map_err(|e| ApiError::External(format!("s3 put: {e}")))?;

        return Ok(Json(json!({
            "source_ref": FileRef {
                kind: "minio",
                bucket: state.cfg.minio_bucket.clone(),
                key,
                size: bytes.len() as i64,
                sha256: sha,
                filename,
            }
        })));
    }
    Err(ApiError::BadRequest("missing file part".to_string()))
}

#[derive(Debug, Deserialize)]
pub struct SampleRequest {
    /// Bucket the object lives in. Defaults to the configured upload bucket
    /// so callers can pass just `key` for the common case.
    pub bucket: Option<String>,
    pub key: String,
    /// `csv | json | xml`. If omitted we sniff from the key suffix.
    pub format: Option<String>,
    /// How many rows the Designer wants to display. Clamped to `MAX_ROW_CAP`.
    pub rows: Option<usize>,
    /// XML only -- which element marks one record. Defaults to `record`,
    /// matching the orchestrator's XML connector.
    pub record_tag: Option<String>,
}

#[derive(Debug, Serialize)]
pub struct SampleResponse {
    pub format: &'static str,
    pub columns: Vec<sampling::SampleColumn>,
    pub rows: Vec<serde_json::Map<String, serde_json::Value>>,
    pub row_count: usize,
    pub truncated: bool,
    pub bytes_read: usize,
    /// Echoed so the UI can persist the source identity alongside the derived
    /// schema.
    pub source: SourceEcho,
}

#[derive(Debug, Serialize)]
pub struct SourceEcho {
    pub bucket: String,
    pub key: String,
}

async fn sample_file(
    State(state): State<AppState>,
    AuthUser(claims): AuthUser,
    Json(req): Json<SampleRequest>,
) -> ApiResult<impl IntoResponse> {
    require_role(&claims, &["admin", "editor", "operator", "viewer"])?;

    if req.key.trim().is_empty() {
        return Err(ApiError::BadRequest("key is required".into()));
    }
    let bucket = req
        .bucket
        .clone()
        .unwrap_or_else(|| state.cfg.minio_bucket.clone());
    let row_cap = req.rows.unwrap_or(DEFAULT_ROW_CAP).min(MAX_ROW_CAP).max(1);
    let format = req
        .format
        .clone()
        .or_else(|| sniff_format_from_key(&req.key))
        .ok_or_else(|| {
            ApiError::BadRequest(
                "could not determine format; pass `format` (csv|json|xml)".into(),
            )
        })?;
    let format = format.to_ascii_lowercase();

    // Fetch only the first chunk via a Range request so we never pull a full
    // 10M-row CSV across the wire.
    let range = format!("bytes=0-{}", DEFAULT_BYTE_CAP - 1);
    let get_out = state
        .s3
        .get_object()
        .bucket(&bucket)
        .key(&req.key)
        .range(range)
        .send()
        .await
        .map_err(|e| ApiError::External(format!("s3 get: {e}")))?;

    let total_size = get_out.content_length().unwrap_or(-1);
    let body_bytes = get_out
        .body
        .collect()
        .await
        .map_err(|e| ApiError::External(format!("s3 read: {e}")))?
        .into_bytes();

    let truncated_input = total_size < 0 || (total_size as usize) > body_bytes.len();

    let result = match format.as_str() {
        "csv" => sampling::csv::sample(&body_bytes, row_cap, truncated_input),
        "json" | "ndjson" | "jsonl" => sampling::json::sample(&body_bytes, row_cap, truncated_input),
        "xml" => sampling::xml::sample(
            &body_bytes,
            row_cap,
            req.record_tag.as_deref().unwrap_or("record"),
            truncated_input,
        ),
        other => return Err(ApiError::BadRequest(format!("unsupported format: {other}"))),
    };

    let result = result.map_err(|e| match e {
        sampling::SampleError::Empty => ApiError::ValidationFailed("file is empty".into()),
        sampling::SampleError::Invalid { format, message } => {
            ApiError::ValidationFailed(format!("{format}: {message}"))
        }
        sampling::SampleError::Unsupported(s) => ApiError::BadRequest(s),
    })?;

    Ok(Json(SampleResponse {
        format: result.format,
        columns: result.columns,
        rows: result.rows,
        row_count: result.row_count,
        truncated: result.truncated,
        bytes_read: result.bytes_read,
        source: SourceEcho {
            bucket,
            key: req.key,
        },
    }))
}

fn sniff_format_from_key(key: &str) -> Option<String> {
    let lower = key.to_ascii_lowercase();
    if lower.ends_with(".csv") || lower.ends_with(".tsv") {
        Some("csv".into())
    } else if lower.ends_with(".json") {
        Some("json".into())
    } else if lower.ends_with(".ndjson") || lower.ends_with(".jsonl") {
        Some("ndjson".into())
    } else if lower.ends_with(".xml") {
        Some("xml".into())
    } else {
        None
    }
}
