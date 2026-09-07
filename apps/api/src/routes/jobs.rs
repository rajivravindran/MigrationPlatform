//! Jobs: create, pause, resume, cancel, retry-row. Plus an SSE stream that
//! bridges Redis pub/sub channel `job:{id}:progress` to clients.

use std::convert::Infallible;
use std::time::Duration;

use axum::body::Body;
use axum::extract::{Path, Query, State};
use axum::http::{header, HeaderValue, StatusCode};
use axum::response::sse::{Event, KeepAlive, Sse};
use axum::response::IntoResponse;
use axum::routing::{get, post};
use axum::{Json, Router};
use futures::stream::StreamExt;
use redis::AsyncCommands;
use serde::{Deserialize, Serialize};
use serde_json::{json, Value};
use tokio_stream::wrappers::BroadcastStream;

use crate::db::{JobRow, JobRowDetail, JobRowStepDetail};
use crate::error::{ApiError, ApiResult};
use crate::job_source::job_json_with_source;
use crate::middleware::auth::{require_role, AuthUser};
use crate::security::audit::record_audit;
use crate::state::AppState;

pub fn router() -> Router<AppState> {
    Router::new()
        .route("/jobs", get(list_jobs).post(create_job))
        .route("/jobs/:id", get(get_job))
        .route("/jobs/:id/pause", post(pause_job))
        .route("/jobs/:id/resume", post(resume_job))
        .route("/jobs/:id/cancel", post(cancel_job))
        .route("/jobs/:id/rows", get(list_rows))
        .route("/jobs/:id/rows/:row_index/steps", get(list_row_steps))
        .route("/jobs/:id/rows/:row_index/retry", post(retry_row))
        .route("/jobs/:id/retry-failed", post(retry_all_failed))
        .route("/jobs/:id/results", get(download_results))
        .route("/jobs/:id/stream", get(stream_updates))
}

/// Columns on `jobs` that map to [`JobRow`], including batch provenance.
const JOB_COLS: &str = "id, org_id, rule_template_id, rule_template_version, schedule_id, source_ref, status::text AS status, totals_json, temporal_workflow_id, temporal_run_id, started_at, paused_at, finished_at, created_by, created_at, updated_at, batch_id, batch_stage_id, results_ref";

const JOB_JOIN_COLS: &str = "j.id, j.org_id, j.rule_template_id, j.rule_template_version, j.schedule_id, j.source_ref, j.status::text AS status, j.totals_json, j.temporal_workflow_id, j.temporal_run_id, j.started_at, j.paused_at, j.finished_at, j.created_by, j.created_at, j.updated_at, j.batch_id, j.batch_stage_id, j.results_ref, b.source_ref AS batch_source_ref, s.stage_key AS batch_stage_key, s.file_path AS batch_stage_file";

#[derive(sqlx::FromRow)]
struct JobJoinRow {
    #[sqlx(flatten)]
    job: JobRow,
    batch_source_ref: Option<Value>,
    batch_stage_key: Option<String>,
    batch_stage_file: Option<String>,
}

impl JobJoinRow {
    fn into_json(self) -> Value {
        job_json_with_source(
            &self.job,
            self.batch_source_ref.as_ref(),
            self.batch_stage_key.as_deref(),
            self.batch_stage_file.as_deref(),
        )
    }
}

#[derive(Deserialize)]
pub struct JobCreate {
    pub rule_template_id: i64,
    pub source_ref: Value,
}

async fn create_job(
    State(state): State<AppState>,
    AuthUser(claims): AuthUser,
    Json(req): Json<JobCreate>,
) -> ApiResult<Json<JobRow>> {
    require_role(&claims, &["admin", "editor", "operator"])?;
    crate::security::license::require_licensed()?;

    let (tpl_version,): (i32,) = sqlx::query_as(
        "SELECT version FROM rule_templates WHERE id = $1 AND org_id = $2 AND published = true",
    )
    .bind(req.rule_template_id)
    .bind(claims.org)
    .fetch_optional(&state.db)
    .await?
    .ok_or_else(|| ApiError::BadRequest("template not found or not published".to_string()))?;

    let created_by: i64 = claims.sub.parse().map_err(|_| ApiError::Unauthorized)?;
    let workflow_id = format!("mig-{}-{}", claims.org, uuid::Uuid::new_v4());

    // Insert the job first so the workflow input can carry the real job id
    // (row/step outcomes are keyed by it).
    let mut row: JobRow = sqlx::query_as(&format!(
        "INSERT INTO jobs (org_id, rule_template_id, rule_template_version, source_ref, status, temporal_workflow_id, created_by)
         VALUES ($1, $2, $3, $4, 'pending'::job_status, $5, $6)
         RETURNING {JOB_COLS}"
    ))
    .bind(claims.org)
    .bind(req.rule_template_id)
    .bind(tpl_version)
    .bind(&req.source_ref)
    .bind(&workflow_id)
    .bind(created_by)
    .fetch_one(&state.db)
    .await?;

    let start = state
        .temporal
        .start_migration_workflow(
            &workflow_id,
            &json!({
                "orgId": claims.org,
                "jobId": row.id,
                "ruleTemplateId": req.rule_template_id,
                "ruleTemplateVersion": tpl_version,
                "sourceRef": req.source_ref,
            }),
        )
        .await;

    let run_id = match start {
        Ok(run_id) => run_id,
        Err(e) => {
            sqlx::query("UPDATE jobs SET status = 'failed'::job_status, finished_at = now(), updated_at = now() WHERE id = $1")
                .bind(row.id)
                .execute(&state.db)
                .await?;
            return Err(ApiError::External(format!("temporal: {e}")));
        }
    };

    row = sqlx::query_as(&format!(
        "UPDATE jobs SET status = 'running'::job_status, temporal_run_id = $1, started_at = now(), updated_at = now()
         WHERE id = $2
         RETURNING {JOB_COLS}"
    ))
    .bind(&run_id)
    .bind(row.id)
    .fetch_one(&state.db)
    .await?;

    record_audit(
        &state.db,
        claims.org,
        &claims.sub,
        "job",
        Some(row.id.to_string()),
        "create",
        None,
        Some(&serde_json::to_value(&row)?),
    )
    .await?;
    metrics::counter!("jobs_created_total").increment(1);

    Ok(Json(row))
}

#[derive(Deserialize)]
pub struct JobsListQuery {
    pub status: Option<String>,
    pub limit: Option<i64>,
    pub cursor: Option<i64>,
}

async fn list_jobs(
    State(state): State<AppState>,
    AuthUser(claims): AuthUser,
    Query(q): Query<JobsListQuery>,
) -> ApiResult<Json<Value>> {
    let limit = q.limit.unwrap_or(50).min(200);
    let cursor = q.cursor.unwrap_or(i64::MAX);
    let rows: Vec<JobJoinRow> = if let Some(status) = q.status {
        sqlx::query_as(&format!(
            "SELECT {JOB_JOIN_COLS}
             FROM jobs j
             LEFT JOIN batches b ON b.id = j.batch_id
             LEFT JOIN batch_stages s ON s.id = j.batch_stage_id
             WHERE j.org_id = $1 AND j.id < $2 AND j.status = $3::job_status
             ORDER BY j.id DESC LIMIT $4"
        ))
        .bind(claims.org)
        .bind(cursor)
        .bind(status)
        .bind(limit)
        .fetch_all(&state.db)
        .await?
    } else {
        sqlx::query_as(&format!(
            "SELECT {JOB_JOIN_COLS}
             FROM jobs j
             LEFT JOIN batches b ON b.id = j.batch_id
             LEFT JOIN batch_stages s ON s.id = j.batch_stage_id
             WHERE j.org_id = $1 AND j.id < $2
             ORDER BY j.id DESC LIMIT $3"
        ))
        .bind(claims.org)
        .bind(cursor)
        .bind(limit)
        .fetch_all(&state.db)
        .await?
    };
    let next = rows.last().map(|r| r.job.id);
    let items: Vec<Value> = rows.into_iter().map(JobJoinRow::into_json).collect();
    Ok(Json(json!({"items": items, "next_cursor": next})))
}

async fn get_job(
    State(state): State<AppState>,
    AuthUser(claims): AuthUser,
    Path(id): Path<i64>,
) -> ApiResult<Json<Value>> {
    let row: Option<JobJoinRow> = sqlx::query_as(&format!(
        "SELECT {JOB_JOIN_COLS}
         FROM jobs j
         LEFT JOIN batches b ON b.id = j.batch_id
         LEFT JOIN batch_stages s ON s.id = j.batch_stage_id
         WHERE j.id = $1 AND j.org_id = $2"
    ))
    .bind(id)
    .bind(claims.org)
    .fetch_optional(&state.db)
    .await?;
    row.map(|r| Json(r.into_json())).ok_or(ApiError::NotFound)
}

#[derive(Deserialize, Default)]
pub struct ResultsQuery {
    #[serde(default)]
    pub failed: bool,
}

async fn download_results(
    State(state): State<AppState>,
    AuthUser(claims): AuthUser,
    Path(id): Path<i64>,
    Query(q): Query<ResultsQuery>,
) -> ApiResult<impl IntoResponse> {
    let row: Option<(Option<Value>,)> =
        sqlx::query_as("SELECT results_ref FROM jobs WHERE id = $1 AND org_id = $2")
            .bind(id)
            .bind(claims.org)
            .fetch_optional(&state.db)
            .await?;
    let Some((results_ref,)) = row else {
        return Err(ApiError::NotFound);
    };

    let filename = if q.failed {
        format!("job-{id}-results-failed.csv")
    } else {
        format!("job-{id}-results.csv")
    };

    if let Some(ref_val) = results_ref.as_ref() {
        let bucket = ref_val
            .get("bucket")
            .and_then(Value::as_str)
            .unwrap_or(&state.cfg.minio_bucket);
        let key = if q.failed {
            ref_val
                .get("failed_key")
                .and_then(Value::as_str)
                .or_else(|| ref_val.get("key").and_then(Value::as_str))
        } else {
            ref_val.get("key").and_then(Value::as_str)
        };
        if let Some(key) = key {
            let obj = state
                .s3
                .get_object()
                .bucket(bucket)
                .key(key)
                .send()
                .await
                .map_err(|e| ApiError::External(format!("s3 get: {e}")))?;
            let bytes = obj
                .body
                .collect()
                .await
                .map_err(|e| ApiError::External(format!("s3 read: {e}")))?
                .into_bytes();
            return csv_attachment(filename, bytes.to_vec());
        }
    }

    let bytes = stream_results_from_db(&state, id, q.failed).await?;
    csv_attachment(filename, bytes)
}

fn csv_attachment(filename: String, bytes: Vec<u8>) -> ApiResult<impl IntoResponse> {
    let disp = format!("attachment; filename=\"{filename}\"");
    Ok((
        StatusCode::OK,
        [
            (
                header::CONTENT_TYPE,
                HeaderValue::from_static("text/csv; charset=utf-8"),
            ),
            (
                header::CONTENT_DISPOSITION,
                HeaderValue::from_str(&disp)
                    .unwrap_or_else(|_| HeaderValue::from_static("attachment")),
            ),
        ],
        Body::from(bytes),
    ))
}

async fn stream_results_from_db(
    state: &AppState,
    job_id: i64,
    failed_only: bool,
) -> ApiResult<Vec<u8>> {
    let rows: Vec<(i64, String, Option<String>)> = if failed_only {
        sqlx::query_as(
            "SELECT row_index, status::text, last_error FROM job_rows
             WHERE job_id = $1 AND status = 'failed'::row_status ORDER BY row_index",
        )
        .bind(job_id)
        .fetch_all(&state.db)
        .await?
    } else {
        sqlx::query_as(
            "SELECT row_index, status::text, last_error FROM job_rows
             WHERE job_id = $1 ORDER BY row_index",
        )
        .bind(job_id)
        .fetch_all(&state.db)
        .await?
    };
    let mut wtr = csv::Writer::from_writer(Vec::new());
    wtr.write_record(["row_index", "status", "error"])
        .map_err(|e| ApiError::External(e.to_string()))?;
    for (idx, status, err) in rows {
        wtr.write_record([idx.to_string(), status, err.unwrap_or_default()])
            .map_err(|e| ApiError::External(e.to_string()))?;
    }
    wtr.into_inner()
        .map_err(|e| ApiError::External(e.to_string()))
}

async fn signal_and_record(
    state: &AppState,
    claims: &crate::security::Claims,
    id: i64,
    signal_name: &str,
    new_status: &str,
    action: &str,
) -> ApiResult<JobRow> {
    let workflow_id: Option<String> =
        sqlx::query_scalar("SELECT temporal_workflow_id FROM jobs WHERE id = $1 AND org_id = $2")
            .bind(id)
            .bind(claims.org)
            .fetch_optional(&state.db)
            .await?
            .flatten();

    let Some(workflow_id) = workflow_id else {
        return Err(ApiError::NotFound);
    };

    state
        .temporal
        .signal_workflow(&workflow_id, signal_name, &json!({}))
        .await
        .map_err(|e| ApiError::External(format!("temporal: {e}")))?;

    let row: JobRow = sqlx::query_as(&format!(
        "UPDATE jobs SET status = $1::job_status, updated_at = now(),
           paused_at = CASE WHEN $1 = 'paused' THEN now() ELSE paused_at END
         WHERE id = $2 AND org_id = $3
         RETURNING {JOB_COLS}"
    ))
    .bind(new_status)
    .bind(id)
    .bind(claims.org)
    .fetch_one(&state.db)
    .await?;

    record_audit(
        &state.db,
        claims.org,
        &claims.sub,
        "job",
        Some(id.to_string()),
        action,
        None,
        Some(&serde_json::to_value(&row)?),
    )
    .await?;
    Ok(row)
}

async fn pause_job(
    State(s): State<AppState>,
    AuthUser(c): AuthUser,
    Path(id): Path<i64>,
) -> ApiResult<Json<JobRow>> {
    require_role(&c, &["admin", "editor", "operator"])?;
    Ok(Json(
        signal_and_record(&s, &c, id, "pause", "paused", "pause").await?,
    ))
}

async fn resume_job(
    State(s): State<AppState>,
    AuthUser(c): AuthUser,
    Path(id): Path<i64>,
) -> ApiResult<Json<JobRow>> {
    require_role(&c, &["admin", "editor", "operator"])?;
    crate::security::license::require_licensed()?;
    Ok(Json(
        signal_and_record(&s, &c, id, "resume", "running", "resume").await?,
    ))
}

async fn cancel_job(
    State(s): State<AppState>,
    AuthUser(c): AuthUser,
    Path(id): Path<i64>,
) -> ApiResult<Json<JobRow>> {
    require_role(&c, &["admin", "editor", "operator"])?;
    let workflow_id: Option<String> =
        sqlx::query_scalar("SELECT temporal_workflow_id FROM jobs WHERE id = $1 AND org_id = $2")
            .bind(id)
            .bind(c.org)
            .fetch_optional(&s.db)
            .await?
            .flatten();
    if let Some(wid) = workflow_id {
        s.temporal
            .cancel_workflow(&wid)
            .await
            .map_err(|e| ApiError::External(format!("temporal: {e}")))?;
    }
    let row: JobRow = sqlx::query_as(&format!(
        "UPDATE jobs SET status = 'cancelled'::job_status, finished_at = now(), updated_at = now()
         WHERE id = $1 AND org_id = $2
         RETURNING {JOB_COLS}"
    ))
    .bind(id)
    .bind(c.org)
    .fetch_one(&s.db)
    .await?;
    record_audit(
        &s.db,
        c.org,
        &c.sub,
        "job",
        Some(id.to_string()),
        "cancel",
        None,
        Some(&serde_json::to_value(&row)?),
    )
    .await?;
    Ok(Json(row))
}

#[derive(Deserialize)]
pub struct RowsQuery {
    pub status: Option<String>,
    pub limit: Option<i64>,
    pub cursor: Option<i64>,
}

#[derive(Serialize)]
pub struct RowsResponse {
    pub items: Vec<JobRowDetail>,
    pub next_cursor: Option<i64>,
}

async fn list_rows(
    State(state): State<AppState>,
    AuthUser(claims): AuthUser,
    Path(id): Path<i64>,
    Query(q): Query<RowsQuery>,
) -> ApiResult<Json<RowsResponse>> {
    // Confirm job belongs to caller's org before returning rows.
    let owned: bool =
        sqlx::query_scalar("SELECT EXISTS(SELECT 1 FROM jobs WHERE id = $1 AND org_id = $2)")
            .bind(id)
            .bind(claims.org)
            .fetch_one(&state.db)
            .await?;
    if !owned {
        return Err(ApiError::NotFound);
    }
    let limit = q.limit.unwrap_or(100).min(1000);
    let cursor = q.cursor.unwrap_or(-1);
    let items: Vec<JobRowDetail> = if let Some(status) = q.status {
        sqlx::query_as(
            "SELECT job_id, row_index, status::text AS status, attempts, last_error, idempotency_key, row_json, payload_json, response_json, updated_at
             FROM job_rows WHERE job_id = $1 AND row_index > $2 AND status = $3::row_status ORDER BY row_index ASC LIMIT $4",
        )
        .bind(id)
        .bind(cursor)
        .bind(status)
        .bind(limit)
        .fetch_all(&state.db)
        .await?
    } else {
        sqlx::query_as(
            "SELECT job_id, row_index, status::text AS status, attempts, last_error, idempotency_key, row_json, payload_json, response_json, updated_at
             FROM job_rows WHERE job_id = $1 AND row_index > $2 ORDER BY row_index ASC LIMIT $3",
        )
        .bind(id)
        .bind(cursor)
        .bind(limit)
        .fetch_all(&state.db)
        .await?
    };
    let next = items.last().map(|r| r.row_index);
    Ok(Json(RowsResponse {
        items,
        next_cursor: next,
    }))
}

/// Per-step outcomes for one row of a multi-step (chained) template: which
/// call failed, the rendered request, the response body and the error reason.
async fn list_row_steps(
    State(state): State<AppState>,
    AuthUser(claims): AuthUser,
    Path((id, row_index)): Path<(i64, i64)>,
) -> ApiResult<Json<Value>> {
    let owned: bool =
        sqlx::query_scalar("SELECT EXISTS(SELECT 1 FROM jobs WHERE id = $1 AND org_id = $2)")
            .bind(id)
            .bind(claims.org)
            .fetch_one(&state.db)
            .await?;
    if !owned {
        return Err(ApiError::NotFound);
    }
    let items: Vec<JobRowStepDetail> = sqlx::query_as(
        "SELECT job_id, row_index, step_index, step_name, status::text AS status, attempts, request_url, request_json, response_status, response_json, last_error, updated_at
         FROM job_row_steps WHERE job_id = $1 AND row_index = $2 ORDER BY step_index ASC",
    )
    .bind(id)
    .bind(row_index)
    .fetch_all(&state.db)
    .await?;
    Ok(Json(json!({"items": items})))
}

#[derive(Deserialize, Default)]
pub struct RetryOptions {
    /// Re-run the whole step chain even if earlier steps succeeded.
    #[serde(default)]
    pub from_start: bool,
}

/// Fetch (workflow_id, rule_template_id, rule_template_version) for an owned job.
async fn job_dispatch_info(state: &AppState, org: i64, id: i64) -> ApiResult<(String, i64, i32)> {
    let row: Option<(Option<String>, i64, i32)> = sqlx::query_as(
        "SELECT temporal_workflow_id, rule_template_id, rule_template_version FROM jobs WHERE id = $1 AND org_id = $2",
    )
    .bind(id)
    .bind(org)
    .fetch_optional(&state.db)
    .await?;
    let Some((Some(workflow_id), tpl_id, tpl_version)) = row else {
        return Err(ApiError::NotFound);
    };
    Ok((workflow_id, tpl_id, tpl_version))
}

async fn retry_row(
    State(state): State<AppState>,
    AuthUser(claims): AuthUser,
    Path((id, row_index)): Path<(i64, i64)>,
    body: Option<Json<RetryOptions>>,
) -> ApiResult<impl IntoResponse> {
    require_role(&claims, &["admin", "editor", "operator"])?;
    crate::security::license::require_licensed()?;
    let opts = body.map(|Json(b)| b).unwrap_or_default();
    let (workflow_id, tpl_id, tpl_version) = job_dispatch_info(&state, claims.org, id).await?;

    let retry_id = format!(
        "{}-retry-{}-{}",
        workflow_id,
        row_index,
        uuid::Uuid::new_v4().simple()
    );
    state
        .temporal
        .start_workflow(
            &retry_id,
            "RetryRowWorkflow",
            &json!({
                "orgId": claims.org,
                "jobId": id,
                "ruleTemplateId": tpl_id,
                "ruleTemplateVersion": tpl_version,
                "sourceRef": {},
                "retryRow": {"jobId": id, "rowIndex": row_index, "fromStart": opts.from_start},
            }),
        )
        .await
        .map_err(|e| ApiError::External(format!("temporal: {e}")))?;

    // Attempts are incremented by the orchestrator when the retry actually
    // persists an outcome; here we only flag the row as queued again.
    sqlx::query(
        "UPDATE job_rows SET status = 'pending'::row_status, last_error = NULL, updated_at = now()
         WHERE job_id = $1 AND row_index = $2",
    )
    .bind(id)
    .bind(row_index)
    .execute(&state.db)
    .await?;

    record_audit(
        &state.db,
        claims.org,
        &claims.sub,
        "job_row",
        Some(format!("{id}:{row_index}")),
        "retry",
        None,
        None,
    )
    .await?;
    Ok(Json(
        json!({"job_id": id, "row_index": row_index, "queued": true, "from_start": opts.from_start}),
    ))
}

async fn retry_all_failed(
    State(state): State<AppState>,
    AuthUser(claims): AuthUser,
    Path(id): Path<i64>,
    body: Option<Json<RetryOptions>>,
) -> ApiResult<impl IntoResponse> {
    require_role(&claims, &["admin", "editor", "operator"])?;
    crate::security::license::require_licensed()?;
    let opts = body.map(|Json(b)| b).unwrap_or_default();
    let (workflow_id, tpl_id, tpl_version) = job_dispatch_info(&state, claims.org, id).await?;

    let n: i64 = sqlx::query_scalar(
        "SELECT COUNT(*) FROM job_rows WHERE job_id = $1 AND status = 'failed'::row_status",
    )
    .bind(id)
    .fetch_one(&state.db)
    .await?;
    if n == 0 {
        return Ok(Json(json!({"job_id": id, "retried": 0})));
    }

    // The retry workflow enumerates failed rows itself (paged) and re-runs
    // each from persisted state, resuming at the first failed step.
    let retry_wf_id = format!(
        "{}-retryfailed-{}",
        workflow_id,
        uuid::Uuid::new_v4().simple()
    );
    state
        .temporal
        .start_workflow(
            &retry_wf_id,
            "RetryFailedRowsWorkflow",
            &json!({
                "orgId": claims.org,
                "jobId": id,
                "ruleTemplateId": tpl_id,
                "ruleTemplateVersion": tpl_version,
                "fromStart": opts.from_start,
            }),
        )
        .await
        .map_err(|e| ApiError::External(format!("temporal: {e}")))?;

    record_audit(
        &state.db,
        claims.org,
        &claims.sub,
        "job",
        Some(id.to_string()),
        "retry_all_failed",
        None,
        None,
    )
    .await?;
    Ok(Json(json!({"job_id": id, "retried": n})))
}

async fn stream_updates(
    State(state): State<AppState>,
    AuthUser(claims): AuthUser,
    Path(id): Path<i64>,
) -> ApiResult<Sse<impl futures::Stream<Item = Result<Event, Infallible>>>> {
    let owned: bool =
        sqlx::query_scalar("SELECT EXISTS(SELECT 1 FROM jobs WHERE id = $1 AND org_id = $2)")
            .bind(id)
            .bind(claims.org)
            .fetch_one(&state.db)
            .await?;
    if !owned {
        return Err(ApiError::NotFound);
    }

    let channel = format!("job:{id}:progress");
    let (tx, rx) = tokio::sync::broadcast::channel::<String>(256);

    let redis_client = state.redis.clone();
    tokio::spawn(async move {
        let mut pubsub = match redis_client.get_async_pubsub().await {
            Ok(c) => c,
            Err(err) => {
                tracing::warn!(error=%err, "redis connect for SSE failed");
                return;
            }
        };
        if let Err(err) = pubsub.subscribe(&channel).await {
            tracing::warn!(error=%err, channel=%channel, "redis subscribe failed");
            return;
        }
        let mut msg_stream = pubsub.on_message();
        while let Some(msg) = msg_stream.next().await {
            if let Ok(payload) = msg.get_payload::<String>() {
                if tx.send(payload).is_err() {
                    break;
                }
            }
        }
    });

    let stream = BroadcastStream::new(rx).filter_map(|res| async move {
        match res {
            Ok(payload) => Some(Ok(Event::default().data(payload))),
            Err(_) => None,
        }
    });
    Ok(Sse::new(stream).keep_alive(KeepAlive::new().interval(Duration::from_secs(15))))
}

/// Publish a progress event to a job's Redis channel (used by the Go
/// orchestrator via the HTTP callback endpoint in real deployments).
#[allow(dead_code)]
pub async fn publish_progress(state: &AppState, job_id: i64, payload: &Value) -> ApiResult<()> {
    let mut conn = state
        .redis
        .get_multiplexed_async_connection()
        .await
        .map_err(|e| ApiError::External(format!("redis: {e}")))?;
    let _: () = conn
        .publish(
            format!("job:{job_id}:progress"),
            serde_json::to_string(payload)?,
        )
        .await
        .map_err(|e| ApiError::External(format!("redis publish: {e}")))?;
    Ok(())
}
