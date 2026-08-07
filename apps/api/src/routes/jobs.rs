//! Jobs: create, pause, resume, cancel, retry-row. Plus an SSE stream that
//! bridges Redis pub/sub channel `job:{id}:progress` to clients.

use std::convert::Infallible;
use std::time::Duration;

use axum::extract::{Path, Query, State};
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
        .route("/jobs/:id/stream", get(stream_updates))
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
    let mut row: JobRow = sqlx::query_as(
        "INSERT INTO jobs (org_id, rule_template_id, rule_template_version, source_ref, status, temporal_workflow_id, created_by)
         VALUES ($1, $2, $3, $4, 'pending'::job_status, $5, $6)
         RETURNING id, org_id, rule_template_id, rule_template_version, schedule_id, source_ref, status::text AS status, totals_json, temporal_workflow_id, temporal_run_id, started_at, paused_at, finished_at, created_by, created_at, updated_at",
    )
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

    row = sqlx::query_as(
        "UPDATE jobs SET status = 'running'::job_status, temporal_run_id = $1, started_at = now(), updated_at = now()
         WHERE id = $2
         RETURNING id, org_id, rule_template_id, rule_template_version, schedule_id, source_ref, status::text AS status, totals_json, temporal_workflow_id, temporal_run_id, started_at, paused_at, finished_at, created_by, created_at, updated_at",
    )
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
    let rows: Vec<JobRow> = if let Some(status) = q.status {
        sqlx::query_as(
            "SELECT id, org_id, rule_template_id, rule_template_version, schedule_id, source_ref, status::text AS status, totals_json, temporal_workflow_id, temporal_run_id, started_at, paused_at, finished_at, created_by, created_at, updated_at
             FROM jobs WHERE org_id = $1 AND id < $2 AND status = $3::job_status ORDER BY id DESC LIMIT $4",
        )
        .bind(claims.org)
        .bind(cursor)
        .bind(status)
        .bind(limit)
        .fetch_all(&state.db)
        .await?
    } else {
        sqlx::query_as(
            "SELECT id, org_id, rule_template_id, rule_template_version, schedule_id, source_ref, status::text AS status, totals_json, temporal_workflow_id, temporal_run_id, started_at, paused_at, finished_at, created_by, created_at, updated_at
             FROM jobs WHERE org_id = $1 AND id < $2 ORDER BY id DESC LIMIT $3",
        )
        .bind(claims.org)
        .bind(cursor)
        .bind(limit)
        .fetch_all(&state.db)
        .await?
    };
    let next = rows.last().map(|r| r.id);
    Ok(Json(json!({"items": rows, "next_cursor": next})))
}

async fn get_job(
    State(state): State<AppState>,
    AuthUser(claims): AuthUser,
    Path(id): Path<i64>,
) -> ApiResult<Json<JobRow>> {
    let row: Option<JobRow> = sqlx::query_as(
        "SELECT id, org_id, rule_template_id, rule_template_version, schedule_id, source_ref, status::text AS status, totals_json, temporal_workflow_id, temporal_run_id, started_at, paused_at, finished_at, created_by, created_at, updated_at
         FROM jobs WHERE id = $1 AND org_id = $2",
    )
    .bind(id)
    .bind(claims.org)
    .fetch_optional(&state.db)
    .await?;
    row.map(Json).ok_or(ApiError::NotFound)
}

async fn signal_and_record(
    state: &AppState,
    claims: &crate::security::Claims,
    id: i64,
    signal_name: &str,
    new_status: &str,
    action: &str,
) -> ApiResult<JobRow> {
    let workflow_id: Option<String> = sqlx::query_scalar(
        "SELECT temporal_workflow_id FROM jobs WHERE id = $1 AND org_id = $2",
    )
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

    let row: JobRow = sqlx::query_as(
        "UPDATE jobs SET status = $1::job_status, updated_at = now(),
           paused_at = CASE WHEN $1 = 'paused' THEN now() ELSE paused_at END
         WHERE id = $2 AND org_id = $3
         RETURNING id, org_id, rule_template_id, rule_template_version, schedule_id, source_ref, status::text AS status, totals_json, temporal_workflow_id, temporal_run_id, started_at, paused_at, finished_at, created_by, created_at, updated_at",
    )
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

async fn pause_job(State(s): State<AppState>, AuthUser(c): AuthUser, Path(id): Path<i64>) -> ApiResult<Json<JobRow>> {
    require_role(&c, &["admin", "editor", "operator"])?;
    Ok(Json(signal_and_record(&s, &c, id, "pause", "paused", "pause").await?))
}

async fn resume_job(State(s): State<AppState>, AuthUser(c): AuthUser, Path(id): Path<i64>) -> ApiResult<Json<JobRow>> {
    require_role(&c, &["admin", "editor", "operator"])?;
    Ok(Json(signal_and_record(&s, &c, id, "resume", "running", "resume").await?))
}

async fn cancel_job(State(s): State<AppState>, AuthUser(c): AuthUser, Path(id): Path<i64>) -> ApiResult<Json<JobRow>> {
    require_role(&c, &["admin", "editor", "operator"])?;
    let workflow_id: Option<String> = sqlx::query_scalar(
        "SELECT temporal_workflow_id FROM jobs WHERE id = $1 AND org_id = $2",
    )
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
    let row: JobRow = sqlx::query_as(
        "UPDATE jobs SET status = 'cancelled'::job_status, finished_at = now(), updated_at = now()
         WHERE id = $1 AND org_id = $2
         RETURNING id, org_id, rule_template_id, rule_template_version, schedule_id, source_ref, status::text AS status, totals_json, temporal_workflow_id, temporal_run_id, started_at, paused_at, finished_at, created_by, created_at, updated_at",
    )
    .bind(id)
    .bind(c.org)
    .fetch_one(&s.db)
    .await?;
    record_audit(&s.db, c.org, &c.sub, "job", Some(id.to_string()), "cancel", None, Some(&serde_json::to_value(&row)?)).await?;
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
    let owned: bool = sqlx::query_scalar("SELECT EXISTS(SELECT 1 FROM jobs WHERE id = $1 AND org_id = $2)")
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
    Ok(Json(RowsResponse { items, next_cursor: next }))
}

/// Per-step outcomes for one row of a multi-step (chained) template: which
/// call failed, the rendered request, the response body and the error reason.
async fn list_row_steps(
    State(state): State<AppState>,
    AuthUser(claims): AuthUser,
    Path((id, row_index)): Path<(i64, i64)>,
) -> ApiResult<Json<Value>> {
    let owned: bool = sqlx::query_scalar("SELECT EXISTS(SELECT 1 FROM jobs WHERE id = $1 AND org_id = $2)")
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
async fn job_dispatch_info(
    state: &AppState,
    org: i64,
    id: i64,
) -> ApiResult<(String, i64, i32)> {
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
    let opts = body.map(|Json(b)| b).unwrap_or_default();
    let (workflow_id, tpl_id, tpl_version) = job_dispatch_info(&state, claims.org, id).await?;

    let retry_id = format!("{}-retry-{}-{}", workflow_id, row_index, uuid::Uuid::new_v4().simple());
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
    Ok(Json(json!({"job_id": id, "row_index": row_index, "queued": true, "from_start": opts.from_start})))
}

async fn retry_all_failed(
    State(state): State<AppState>,
    AuthUser(claims): AuthUser,
    Path(id): Path<i64>,
    body: Option<Json<RetryOptions>>,
) -> ApiResult<impl IntoResponse> {
    require_role(&claims, &["admin", "editor", "operator"])?;
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
    let retry_wf_id = format!("{}-retryfailed-{}", workflow_id, uuid::Uuid::new_v4().simple());
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

    record_audit(&state.db, claims.org, &claims.sub, "job", Some(id.to_string()), "retry_all_failed", None, None).await?;
    Ok(Json(json!({"job_id": id, "retried": n})))
}

async fn stream_updates(
    State(state): State<AppState>,
    AuthUser(claims): AuthUser,
    Path(id): Path<i64>,
) -> ApiResult<Sse<impl futures::Stream<Item = Result<Event, Infallible>>>> {
    let owned: bool = sqlx::query_scalar("SELECT EXISTS(SELECT 1 FROM jobs WHERE id = $1 AND org_id = $2)")
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
        let conn = match redis_client.get_async_connection().await {
            Ok(c) => c,
            Err(err) => {
                tracing::warn!(error=%err, "redis connect for SSE failed");
                return;
            }
        };
        let mut pubsub = conn.into_pubsub();
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
    let mut conn = state.redis.get_async_connection().await.map_err(|e| ApiError::External(format!("redis: {e}")))?;
    let _: () = conn
        .publish(format!("job:{job_id}:progress"), serde_json::to_string(payload)?)
        .await
        .map_err(|e| ApiError::External(format!("redis publish: {e}")))?;
    Ok(())
}
