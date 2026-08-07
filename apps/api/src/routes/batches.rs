//! Batches: list/get archive batch runs (P2). Manual start from an existing MinIO object.

use axum::extract::{Path, Query, State};
use axum::routing::get;
use axum::{Json, Router};
use chrono::{DateTime, Utc};
use serde::{Deserialize, Serialize};
use serde_json::{json, Value};
use sqlx::FromRow;

use crate::error::{ApiError, ApiResult};
use crate::middleware::auth::{require_role, AuthUser};
use crate::security::audit::record_audit;
use crate::state::AppState;

pub fn router() -> Router<AppState> {
    Router::new()
        .route("/batches", get(list_batches).post(create_batch))
        .route("/batches/:id", get(get_batch))
}

#[derive(Debug, Clone, FromRow, Serialize)]
pub struct BatchRow {
    pub id: i64,
    pub org_id: i64,
    pub schedule_id: Option<i64>,
    pub connector_id: Option<i64>,
    pub batch_key: Option<String>,
    pub source_ref: Value,
    pub status: String,
    pub on_stage_failure: String,
    pub manifest_json: Option<Value>,
    pub temporal_workflow_id: Option<String>,
    pub temporal_run_id: Option<String>,
    pub error_message: Option<String>,
    pub quarantine_ref: Option<Value>,
    pub started_at: Option<DateTime<Utc>>,
    pub finished_at: Option<DateTime<Utc>>,
    pub created_at: DateTime<Utc>,
    pub updated_at: DateTime<Utc>,
}

#[derive(Debug, Clone, FromRow, Serialize)]
pub struct BatchStageRow {
    pub id: i64,
    pub batch_id: i64,
    pub stage_index: i32,
    pub stage_key: String,
    pub file_path: String,
    pub template_key: String,
    pub rule_template_id: Option<i64>,
    pub rule_template_version: Option<i32>,
    pub job_id: Option<i64>,
    pub status: String,
    pub on_stage_failure: Option<String>,
    pub error_message: Option<String>,
    pub created_at: DateTime<Utc>,
    pub updated_at: DateTime<Utc>,
}

#[derive(Serialize)]
pub struct BatchDetail {
    #[serde(flatten)]
    pub batch: BatchRow,
    pub stages: Vec<BatchStageRow>,
}

#[derive(Deserialize)]
pub struct ListQuery {
    pub cursor: Option<i64>,
    pub limit: Option<i64>,
    pub status: Option<String>,
}

const BATCH_COLS: &str = "id, org_id, schedule_id, connector_id, batch_key, source_ref, status::text AS status, on_stage_failure, manifest_json, temporal_workflow_id, temporal_run_id, error_message, quarantine_ref, started_at, finished_at, created_at, updated_at";

const STAGE_COLS: &str = "id, batch_id, stage_index, stage_key, file_path, template_key, rule_template_id, rule_template_version, job_id, status::text AS status, on_stage_failure, error_message, created_at, updated_at";

async fn list_batches(
    State(state): State<AppState>,
    AuthUser(claims): AuthUser,
    Query(q): Query<ListQuery>,
) -> ApiResult<Json<Value>> {
    let limit = q.limit.unwrap_or(50).clamp(1, 200);
    let cursor = q.cursor.unwrap_or(i64::MAX);
    let items: Vec<BatchRow> = if let Some(status) = &q.status {
        sqlx::query_as(&format!(
            "SELECT {BATCH_COLS} FROM batches
             WHERE org_id = $1 AND id < $2 AND status = $3::batch_status
             ORDER BY id DESC LIMIT $4"
        ))
        .bind(claims.org)
        .bind(cursor)
        .bind(status)
        .bind(limit)
        .fetch_all(&state.db)
        .await?
    } else {
        sqlx::query_as(&format!(
            "SELECT {BATCH_COLS} FROM batches
             WHERE org_id = $1 AND id < $2
             ORDER BY id DESC LIMIT $3"
        ))
        .bind(claims.org)
        .bind(cursor)
        .bind(limit)
        .fetch_all(&state.db)
        .await?
    };
    Ok(Json(json!({ "items": items })))
}

async fn get_batch(
    State(state): State<AppState>,
    AuthUser(claims): AuthUser,
    Path(id): Path<i64>,
) -> ApiResult<Json<BatchDetail>> {
    let batch: BatchRow = sqlx::query_as(&format!(
        "SELECT {BATCH_COLS} FROM batches WHERE id = $1 AND org_id = $2"
    ))
    .bind(id)
    .bind(claims.org)
    .fetch_optional(&state.db)
    .await?
    .ok_or(ApiError::NotFound)?;

    let stages: Vec<BatchStageRow> = sqlx::query_as(&format!(
        "SELECT {STAGE_COLS} FROM batch_stages WHERE batch_id = $1 ORDER BY stage_index"
    ))
    .bind(id)
    .fetch_all(&state.db)
    .await?;

    Ok(Json(BatchDetail { batch, stages }))
}

#[derive(Deserialize)]
pub struct BatchCreate {
    /// MinIO/S3 source for an archive (.tar.gz / .tgz / .zip).
    pub source_ref: Value,
    pub connector_id: Option<i64>,
}

async fn create_batch(
    State(state): State<AppState>,
    AuthUser(claims): AuthUser,
    Json(req): Json<BatchCreate>,
) -> ApiResult<Json<BatchRow>> {
    require_role(&claims, &["admin", "editor", "operator"])?;

    let src_type = req
        .source_ref
        .get("type")
        .and_then(|v| v.as_str())
        .unwrap_or("");
    if src_type != "minio" {
        return Err(ApiError::BadRequest(
            "source_ref.type must be minio for batch packages".into(),
        ));
    }
    let bucket = req
        .source_ref
        .get("bucket")
        .and_then(|v| v.as_str())
        .unwrap_or("")
        .to_string();
    let key = req
        .source_ref
        .get("key")
        .and_then(|v| v.as_str())
        .unwrap_or("")
        .to_string();
    if bucket.is_empty() || key.is_empty() {
        return Err(ApiError::BadRequest(
            "source_ref.bucket and source_ref.key are required".into(),
        ));
    }
    let lower = key.to_ascii_lowercase();
    if !(lower.ends_with(".tar.gz") || lower.ends_with(".tgz") || lower.ends_with(".zip")) {
        return Err(ApiError::BadRequest(
            "batch source must be .tar.gz, .tgz, or .zip".into(),
        ));
    }
    let etag = req
        .source_ref
        .get("etag")
        .and_then(|v| v.as_str())
        .unwrap_or("")
        .to_string();
    let size = req
        .source_ref
        .get("size")
        .and_then(|v| v.as_i64())
        .unwrap_or(0);

    let workflow_id = format!(
        "batch-{}-{}",
        claims.org,
        uuid::Uuid::new_v4().simple()
    );

    let mut row: BatchRow = sqlx::query_as(&format!(
        "INSERT INTO batches (org_id, connector_id, source_ref, status, temporal_workflow_id, started_at)
         VALUES ($1, $2, $3, 'pending'::batch_status, $4, now())
         RETURNING {BATCH_COLS}"
    ))
    .bind(claims.org)
    .bind(req.connector_id)
    .bind(&req.source_ref)
    .bind(&workflow_id)
    .fetch_one(&state.db)
    .await?;

    let start = state
        .temporal
        .start_workflow(
            &workflow_id,
            "BatchWorkflow",
            &json!({
                "orgId": claims.org,
                "batchId": row.id,
                "connectorId": req.connector_id,
                "bucket": bucket,
                "key": key,
                "etag": etag,
                "size": size,
            }),
        )
        .await;

    let run_id = match start {
        Ok(run_id) => run_id,
        Err(e) => {
            sqlx::query(
                "UPDATE batches SET status = 'failed'::batch_status, finished_at = now(),
                 error_message = $1, updated_at = now() WHERE id = $2",
            )
            .bind(format!("temporal: {e}"))
            .bind(row.id)
            .execute(&state.db)
            .await?;
            return Err(ApiError::External(format!("temporal: {e}")));
        }
    };

    row = sqlx::query_as(&format!(
        "UPDATE batches SET status = 'running'::batch_status, temporal_run_id = $1, updated_at = now()
         WHERE id = $2
         RETURNING {BATCH_COLS}"
    ))
    .bind(&run_id)
    .bind(row.id)
    .fetch_one(&state.db)
    .await?;

    record_audit(
        &state.db,
        claims.org,
        &claims.sub,
        "batch",
        Some(row.id.to_string()),
        "create",
        None,
        Some(&serde_json::to_value(&row)?),
    )
    .await?;

    Ok(Json(row))
}
