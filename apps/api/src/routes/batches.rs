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
use crate::sources;
use crate::state::AppState;

/// True when the bridge refused the connection, so no workflow can exist.
fn start_never_reached_bridge(e: &anyhow::Error) -> bool {
    e.downcast_ref::<reqwest::Error>()
        .is_some_and(reqwest::Error::is_connect)
}

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
    pub depends_on: Vec<String>,
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

const STAGE_COLS: &str = "id, batch_id, stage_index, stage_key, file_path, template_key, rule_template_id, rule_template_version, job_id, status::text AS status, on_stage_failure, depends_on, error_message, created_at, updated_at";

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
    crate::security::license::require_licensed()?;

    let source_req = sources::parse_source_request(&req.source_ref)?;
    let source = sources::resolve_object(&state.db, claims.org, &source_req).await?;
    let archive_name = source.filename.as_deref().unwrap_or(&source.object_key);
    if sources::format_from_filename(archive_name) != "archive" {
        return Err(ApiError::BadRequest(
            "batch source must be .tar.gz, .tgz, or .zip".into(),
        ));
    }
    if let Some(connector_id) = req.connector_id {
        sources::require_connector(&state.db, claims.org, connector_id).await?;
    }
    let source_ref = source.source_ref();

    let workflow_id = format!("batch-{}-{}", claims.org, uuid::Uuid::new_v4().simple());

    let mut row: BatchRow = sqlx::query_as(&format!(
        "INSERT INTO batches (org_id, connector_id, source_ref, status, temporal_workflow_id, started_at)
         VALUES ($1, $2, $3, 'pending'::batch_status, $4, now())
         RETURNING {BATCH_COLS}"
    ))
    .bind(claims.org)
    .bind(req.connector_id)
    .bind(&source_ref)
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
                "sourceId": source.id,
                "bucket": source.bucket,
                "key": source.object_key,
                "filename": archive_name,
                "etag": source.etag.clone().unwrap_or_default(),
                "size": source.size_bytes.unwrap_or(0),
            }),
        )
        .await;

    // Same lifecycle contract as jobs (ADR 0004, architecture review A2):
    // only a refused connection proves nothing started, so only that marks
    // the batch failed; an ambiguous start leaves it pending for the
    // reconciler, and a successful start never overwrites a batch that a
    // fast workflow already finalized.
    let run_id = match start {
        Ok(run_id) => run_id,
        Err(e) if !start_never_reached_bridge(&e) => {
            tracing::warn!(batch_id = row.id, workflow_id, error = %e, "batch workflow start outcome unknown");
            return Err(ApiError::External(format!(
                "temporal: {e}; batch {} will be reconciled",
                row.id
            )));
        }
        Err(e) => {
            sqlx::query(
                "UPDATE batches SET status = 'failed'::batch_status, finished_at = now(),
                 error_message = $1, updated_at = now()
                 WHERE id = $2 AND status = 'pending'::batch_status",
            )
            .bind(format!("workflow start failed: {e}"))
            .bind(row.id)
            .execute(&state.db)
            .await?;
            return Err(ApiError::External(format!("temporal: {e}")));
        }
    };

    row = sqlx::query_as(&format!(
        "UPDATE batches SET
           status = CASE WHEN status = 'pending'::batch_status THEN 'running'::batch_status ELSE status END,
           temporal_run_id = $1, started_at = COALESCE(started_at, now()), updated_at = now()
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
