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
use crate::sources;
use crate::state::AppState;
use crate::temporal::{WorkflowAlreadyRunning, WorkflowNotFound};

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
        .route("/jobs/:id/export", post(retry_export))
        .route("/jobs/:id/stream", get(stream_updates))
}

/// Columns on `jobs` that map to [`JobRow`], including batch provenance.
const JOB_COLS: &str = "id, org_id, rule_template_id, rule_template_version, schedule_id, source_ref, status::text AS status, totals_json, temporal_workflow_id, temporal_run_id, started_at, paused_at, finished_at, created_by, created_at, updated_at, batch_id, batch_stage_id, results_ref, last_error, export_status, export_error";

const JOB_JOIN_COLS: &str = "j.id, j.org_id, j.rule_template_id, j.rule_template_version, j.schedule_id, j.source_ref, j.status::text AS status, j.totals_json, j.temporal_workflow_id, j.temporal_run_id, j.started_at, j.paused_at, j.finished_at, j.created_by, j.created_at, j.updated_at, j.batch_id, j.batch_stage_id, j.results_ref, j.last_error, j.export_status, j.export_error, b.source_ref AS batch_source_ref, s.stage_key AS batch_stage_key, s.file_path AS batch_stage_file";

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

    let tpl_version =
        crate::admission::admit_template(&state.db, claims.org, req.rule_template_id, None)
            .await?
            .version;

    let created_by: i64 = claims.sub.parse().map_err(|_| ApiError::Unauthorized)?;
    let source_req = sources::parse_source_request(&req.source_ref)?;
    let resolved = sources::resolve(&state.db, claims.org, &source_req).await?;
    if let sources::ResolvedSource::Object(src) = &resolved {
        let template_type: String = sqlx::query_scalar(
            "SELECT COALESCE(schema_json->'source'->>'type', '') FROM rule_templates WHERE id = $1",
        )
        .bind(req.rule_template_id)
        .fetch_one(&state.db)
        .await?;
        let filename = src.filename.clone().unwrap_or_default();
        let format = src
            .format
            .clone()
            .unwrap_or_else(|| sources::format_from_filename(&filename).to_string());
        if let Some(msg) = crate::routes::plans::format_mismatch(&template_type, &format, &filename)
        {
            return Err(ApiError::BadRequest(msg));
        }
    }
    let source_ref = resolved.source_ref();
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
    .bind(&source_ref)
    .bind(&workflow_id)
    .bind(created_by)
    .fetch_one(&state.db)
    .await?;

    let input = json!({
        "orgId": claims.org,
        "jobId": row.id,
        "ruleTemplateId": req.rule_template_id,
        "ruleTemplateVersion": tpl_version,
        "sourceRef": source_ref,
    });
    // Starting the workflow and recording it run on a detached task: if the
    // client disconnects, dropping this handler midway would leave a pending
    // job whose workflow may or may not exist.
    let task_state = state.clone();
    let job_id = row.id;
    row = tokio::spawn(async move { start_job(&task_state, job_id, &workflow_id, &input).await })
        .await
        .map_err(|e| ApiError::Internal(anyhow::anyhow!("job start task: {e}")))??;

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

/// Starts the workflow for a pending job and records the run. Transitions
/// are conditional on `pending`: a fast workflow may already have finalized
/// the job, and that terminal state must not be overwritten.
async fn start_job(
    state: &AppState,
    job_id: i64,
    workflow_id: &str,
    input: &Value,
) -> ApiResult<JobRow> {
    let run_id = match state
        .temporal
        .start_migration_workflow(workflow_id, input)
        .await
    {
        Ok(run_id) => run_id,
        Err(e) if !start_never_reached_bridge(&e) => {
            // The start may have happened (timeout, lost response). The job
            // stays pending; the reconciler settles it from Temporal's view.
            tracing::warn!(job_id, workflow_id, error = %e, "workflow start outcome unknown");
            return Err(ApiError::External(format!(
                "temporal: {e}; job {job_id} will be reconciled"
            )));
        }
        Err(e) => {
            let message = format!("workflow start failed: {e}");
            sqlx::query(
                "UPDATE jobs SET status = 'failed'::job_status, last_error = $2,
                   finished_at = now(), updated_at = now()
                 WHERE id = $1 AND status = 'pending'::job_status",
            )
            .bind(job_id)
            .bind(&message)
            .execute(&state.db)
            .await?;
            return Err(ApiError::External(format!("temporal: {e}")));
        }
    };

    Ok(sqlx::query_as(&format!(
        "UPDATE jobs SET
           status = CASE WHEN status = 'pending'::job_status THEN 'running'::job_status ELSE status END,
           temporal_run_id = $1, started_at = COALESCE(started_at, now()), updated_at = now()
         WHERE id = $2
         RETURNING {JOB_COLS}"
    ))
    .bind(&run_id)
    .bind(job_id)
    .fetch_one(&state.db)
    .await?)
}

/// True when the bridge refused the connection, so no workflow can exist.
fn start_never_reached_bridge(e: &anyhow::Error) -> bool {
    e.downcast_ref::<reqwest::Error>()
        .is_some_and(reqwest::Error::is_connect)
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
    let row: Option<(Option<Value>, Option<String>, i64)> = sqlx::query_as(
        "SELECT results_ref, export_status, outcome_revision FROM jobs WHERE id = $1 AND org_id = $2",
    )
    .bind(id)
    .bind(claims.org)
    .fetch_optional(&state.db)
    .await?;
    let Some((results_ref, export_status, outcome_revision)) = row else {
        return Err(ApiError::NotFound);
    };
    // Exported files are current only while the latest export succeeded
    // (NULL: exported before export_status existed) and describe the job's
    // current outcome revision (F5); an export without a revision predates
    // revisions and is trusted only when nothing has changed since
    // (revision 0). Otherwise read rows.
    let results_ref = results_ref
        .filter(|_| export_status.as_deref().map_or(true, |s| s == "succeeded"))
        .filter(|r| r.get("revision").and_then(Value::as_i64).unwrap_or(0) == outcome_revision);

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
             WHERE job_id = $1 AND status IN ('failed'::row_status, 'unresolved'::row_status)
             ORDER BY row_index",
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

/// The job states an operator action may start from.
const PAUSABLE: &[&str] = &["running"];
const RESUMABLE: &[&str] = &["paused"];
const CANCELLABLE: &[&str] = &["pending", "running", "paused"];

/// Loads the job's workflow id and refuses the action unless the job is in
/// one of `from`.
async fn action_target(
    state: &AppState,
    org: i64,
    id: i64,
    action: &str,
    from: &[&str],
) -> ApiResult<Option<String>> {
    let row: Option<(Option<String>, String)> = sqlx::query_as(
        "SELECT temporal_workflow_id, status::text FROM jobs WHERE id = $1 AND org_id = $2",
    )
    .bind(id)
    .bind(org)
    .fetch_optional(&state.db)
    .await?;
    let Some((workflow_id, status)) = row else {
        return Err(ApiError::NotFound);
    };
    if !from.contains(&status.as_str()) {
        return Err(ApiError::Conflict(format!(
            "job {id} is {status}; cannot {action} it"
        )));
    }
    Ok(workflow_id)
}

/// The job moved on (usually finished) between the check and the update.
async fn transition_lost(state: &AppState, org: i64, id: i64, action: &str) -> ApiError {
    let status: Option<String> =
        sqlx::query_scalar("SELECT status::text FROM jobs WHERE id = $1 AND org_id = $2")
            .bind(id)
            .bind(org)
            .fetch_optional(&state.db)
            .await
            .ok()
            .flatten();
    ApiError::Conflict(format!(
        "job {id} became {} before it could {action}",
        status.as_deref().unwrap_or("unavailable")
    ))
}

/// Pause/resume as a control command with a stable identity (architecture
/// review A3). The request is recorded on the job before the signal is sent
/// and cleared once the workflow confirms it, so an HTTP response can never
/// claim both effects at once:
///
/// 1. record `control_requested` / `control_command_id` (conditional on the
///    job being in a state the command applies to, or on the same command
///    being retried);
/// 2. signal the workflow with that command id (idempotent in the workflow);
/// 3. confirm the workflow's pause state through its query and persist the
///    status it reports, clearing the request.
///
/// If any step after 1 fails, the request stays recorded and the reconciler
/// converges the job from the workflow's own pause state, re-sending the
/// signal if it never arrived. Retrying the same command from the API is
/// safe at every point.
async fn signal_and_record(
    state: &AppState,
    claims: &crate::security::Claims,
    id: i64,
    signal_name: &str,
    new_status: &str,
    action: &str,
    from: &[&str],
) -> ApiResult<JobRow> {
    let Some(workflow_id) = action_target(state, claims.org, id, action, from).await? else {
        return Err(ApiError::NotFound);
    };
    let requested = if new_status == "paused" {
        "pause"
    } else {
        "resume"
    };
    let from_states: Vec<String> = from.iter().map(|s| s.to_string()).collect();

    // 1. Record the intent. A retry of an unconfirmed request of the same
    //    kind keeps its command id so the workflow sees one decision.
    let command_id: Option<String> = sqlx::query_scalar(
        "UPDATE jobs SET
           control_command_id = CASE WHEN control_requested = $4 AND control_command_id IS NOT NULL
                                     THEN control_command_id ELSE $5 END,
           control_requested = $4,
           control_requested_at = now(),
           updated_at = now()
         WHERE id = $1 AND org_id = $2
           AND (status::text = ANY($3) OR control_requested = $4)
         RETURNING control_command_id",
    )
    .bind(id)
    .bind(claims.org)
    .bind(&from_states)
    .bind(requested)
    .bind(uuid::Uuid::new_v4().simple().to_string())
    .fetch_optional(&state.db)
    .await?;
    let Some(command_id) = command_id else {
        return Err(transition_lost(state, claims.org, id, action).await);
    };

    // 2. Deliver the command.
    if let Err(e) = state
        .temporal
        .signal_workflow(&workflow_id, signal_name, &json!({"commandId": command_id}))
        .await
    {
        return Err(match e.downcast_ref::<WorkflowNotFound>() {
            Some(_) => ApiError::Conflict(format!(
                "job {id}'s workflow has ended; it will be reconciled"
            )),
            None => ApiError::External(format!(
                "temporal: {e}; the {action} request is recorded and will be reconciled"
            )),
        });
    }

    // 3. Confirm from the workflow's own state. The query may lag the signal
    //    by a workflow task; an unconfirmed request is left recorded for the
    //    reconciler rather than guessed.
    let acknowledged = match state.temporal.query_pause_state(&workflow_id).await {
        Ok(pause) => {
            pause.last_command_id.as_deref() == Some(command_id.as_str())
                || pause.paused == (new_status == "paused")
        }
        Err(e) => {
            tracing::warn!(job_id = id, error = %e, "pause state query failed; leaving the request for the reconciler");
            false
        }
    };
    if !acknowledged {
        return Err(ApiError::Conflict(format!(
            "job {id}: the {action} request is recorded but the workflow has not confirmed it yet; retry or wait for reconciliation"
        )));
    }

    let row: Option<JobRow> = sqlx::query_as(&format!(
        "UPDATE jobs SET status = $1::job_status, updated_at = now(),
           paused_at = CASE WHEN $1 = 'paused' THEN now() ELSE paused_at END,
           control_requested = NULL, control_command_id = NULL, control_requested_at = NULL
         WHERE id = $2 AND org_id = $3 AND control_command_id = $4
           AND status IN ('running'::job_status, 'paused'::job_status)
         RETURNING {JOB_COLS}"
    ))
    .bind(new_status)
    .bind(id)
    .bind(claims.org)
    .bind(&command_id)
    .fetch_optional(&state.db)
    .await?;
    let Some(row) = row else {
        return Err(transition_lost(state, claims.org, id, action).await);
    };

    record_audit(
        &state.db,
        claims.org,
        &claims.sub,
        "job",
        Some(id.to_string()),
        action,
        None,
        Some(&json!({"command_id": command_id, "status": row.status})),
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
        signal_and_record(&s, &c, id, "pause", "paused", "pause", PAUSABLE).await?,
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
        signal_and_record(&s, &c, id, "resume", "running", "resume", RESUMABLE).await?,
    ))
}

/// Cancels a non-terminal job. When the workflow no longer exists (it never
/// started, or ended without finalizing) nothing is running, so the job is
/// cancelled directly instead of failing the request.
async fn cancel_job(
    State(s): State<AppState>,
    AuthUser(c): AuthUser,
    Path(id): Path<i64>,
) -> ApiResult<Json<JobRow>> {
    require_role(&c, &["admin", "editor", "operator"])?;
    let workflow_id = action_target(&s, c.org, id, "cancel", CANCELLABLE).await?;
    let mut workflow_found = workflow_id.is_some();
    if let Some(wid) = workflow_id {
        if let Err(e) = s.temporal.cancel_workflow(&wid).await {
            if e.downcast_ref::<WorkflowNotFound>().is_none() {
                return Err(ApiError::External(format!("temporal: {e}")));
            }
            workflow_found = false;
        }
    }
    let row: Option<JobRow> = sqlx::query_as(&format!(
        "UPDATE jobs SET status = 'cancelled'::job_status, finished_at = now(), updated_at = now(),
           totals_json = (
             SELECT jsonb_build_object(
               'processed', count(*) FILTER (WHERE status = 'succeeded'::row_status),
               'failed', count(*) FILTER (WHERE status = 'failed'::row_status),
               'unresolved', count(*) FILTER (WHERE status = 'unresolved'::row_status),
               'total', count(*))
             FROM job_rows WHERE job_id = $1)
         WHERE id = $1 AND org_id = $2
           AND status IN ('pending'::job_status, 'running'::job_status, 'paused'::job_status)
         RETURNING {JOB_COLS}"
    ))
    .bind(id)
    .bind(c.org)
    .fetch_optional(&s.db)
    .await?;
    let Some(row) = row else {
        return Err(transition_lost(&s, c.org, id, "cancel").await);
    };
    let mut after = serde_json::to_value(&row)?;
    after["workflow_found"] = json!(workflow_found);
    record_audit(
        &s.db,
        c.org,
        &c.sub,
        "job",
        Some(id.to_string()),
        "cancel",
        None,
        Some(&after),
    )
    .await?;
    Ok(Json(row))
}

fn export_workflow_id(job_workflow_id: &str) -> String {
    format!("{job_workflow_id}-export")
}

/// Re-runs only the results export of a finished job. The export workflow
/// reads job_rows and uploads files; it makes no destination calls.
async fn retry_export(
    State(state): State<AppState>,
    AuthUser(claims): AuthUser,
    Path(id): Path<i64>,
) -> ApiResult<impl IntoResponse> {
    require_role(&claims, &["admin", "editor", "operator"])?;
    let row: Option<(Option<String>, String)> = sqlx::query_as(
        "SELECT temporal_workflow_id, status::text FROM jobs WHERE id = $1 AND org_id = $2",
    )
    .bind(id)
    .bind(claims.org)
    .fetch_optional(&state.db)
    .await?;
    let Some((Some(job_workflow_id), status)) = row else {
        return Err(ApiError::NotFound);
    };
    if CANCELLABLE.contains(&status.as_str()) {
        return Err(ApiError::Conflict(format!(
            "job {id} is {status}; its results are exported when it finishes"
        )));
    }

    let workflow_id = export_workflow_id(&job_workflow_id);
    let start = state
        .temporal
        .start_workflow(&workflow_id, "ExportResultsWorkflow", &json!({"jobId": id}))
        .await;
    if let Err(e) = start {
        if e.downcast_ref::<WorkflowAlreadyRunning>().is_some() {
            return Err(ApiError::Conflict(format!(
                "an export of job {id} is already running"
            )));
        }
        return Err(ApiError::External(format!("temporal: {e}")));
    }

    record_audit(
        &state.db,
        claims.org,
        &claims.sub,
        "job",
        Some(id.to_string()),
        "retry_export",
        None,
        Some(&json!({"workflow_id": workflow_id})),
    )
    .await?;
    Ok((
        StatusCode::ACCEPTED,
        Json(json!({"job_id": id, "workflow_id": workflow_id})),
    ))
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
    /// The operator checked the destination and found no effect from the
    /// row's unresolved operation, so it may be sent again (ADR 0003).
    #[serde(default)]
    pub confirm_not_applied: bool,
}

struct RetryTarget {
    workflow_id: String,
    rule_template_id: i64,
    rule_template_version: i32,
}

/// Load an owned job that may be retried. Retries of one job share one
/// workflow ID, so a second retry while one runs is refused by Temporal; a
/// job that is still running is refused here.
async fn retry_target(state: &AppState, org: i64, id: i64) -> ApiResult<RetryTarget> {
    let row: Option<(Option<String>, i64, i32, String)> = sqlx::query_as(
        "SELECT temporal_workflow_id, rule_template_id, rule_template_version, status::text
         FROM jobs WHERE id = $1 AND org_id = $2",
    )
    .bind(id)
    .bind(org)
    .fetch_optional(&state.db)
    .await?;
    let Some((Some(workflow_id), rule_template_id, rule_template_version, status)) = row else {
        return Err(ApiError::NotFound);
    };
    if matches!(status.as_str(), "pending" | "running" | "paused") {
        return Err(ApiError::Conflict(format!(
            "job {id} is {status}; retry rows after it finishes"
        )));
    }
    Ok(RetryTarget {
        workflow_id,
        rule_template_id,
        rule_template_version,
    })
}

fn retry_workflow_id(job_workflow_id: &str) -> String {
    format!("{job_workflow_id}-retry")
}

fn start_error(e: anyhow::Error) -> ApiError {
    match e.downcast_ref::<WorkflowAlreadyRunning>() {
        Some(_) => ApiError::Conflict("a retry of this job is already running".into()),
        None => ApiError::External(format!("temporal: {e}")),
    }
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
    let target = retry_target(&state, claims.org, id).await?;

    let row_status: Option<String> = sqlx::query_scalar(
        "SELECT status::text FROM job_rows WHERE job_id = $1 AND row_index = $2",
    )
    .bind(id)
    .bind(row_index)
    .fetch_optional(&state.db)
    .await?;
    if row_status.as_deref() == Some("unresolved") && !opts.confirm_not_applied {
        return Err(ApiError::Conflict(format!(
            "row {row_index} is unresolved: the destination may already have applied it. \
             Check the destination, then retry with confirm_not_applied=true if it did not"
        )));
    }

    // The row keeps its status until the retry persists a new outcome.
    state
        .temporal
        .start_workflow(
            &retry_workflow_id(&target.workflow_id),
            "RetryRowWorkflow",
            &json!({
                "orgId": claims.org,
                "jobId": id,
                "ruleTemplateId": target.rule_template_id,
                "ruleTemplateVersion": target.rule_template_version,
                "sourceRef": {},
                "retryRow": {
                    "jobId": id,
                    "rowIndex": row_index,
                    "fromStart": opts.from_start,
                    "confirmNotApplied": opts.confirm_not_applied,
                },
            }),
        )
        .await
        .map_err(start_error)?;

    record_audit(
        &state.db,
        claims.org,
        &claims.sub,
        "job_row",
        Some(format!("{id}:{row_index}")),
        "retry",
        row_status.map(|s| json!({"status": s})).as_ref(),
        Some(&json!({
            "from_start": opts.from_start,
            "confirm_not_applied": opts.confirm_not_applied,
        })),
    )
    .await?;
    Ok(Json(json!({
        "job_id": id,
        "row_index": row_index,
        "queued": true,
        "from_start": opts.from_start,
        "confirm_not_applied": opts.confirm_not_applied,
    })))
}

/// Retries `failed` rows. `unresolved` rows are excluded: each needs an
/// operator decision through the single-row retry.
async fn retry_all_failed(
    State(state): State<AppState>,
    AuthUser(claims): AuthUser,
    Path(id): Path<i64>,
    body: Option<Json<RetryOptions>>,
) -> ApiResult<impl IntoResponse> {
    require_role(&claims, &["admin", "editor", "operator"])?;
    crate::security::license::require_licensed()?;
    let opts = body.map(|Json(b)| b).unwrap_or_default();
    if opts.confirm_not_applied {
        return Err(ApiError::BadRequest(
            "confirm_not_applied is decided per row; use the single-row retry".into(),
        ));
    }
    let target = retry_target(&state, claims.org, id).await?;

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
    state
        .temporal
        .start_workflow(
            &retry_workflow_id(&target.workflow_id),
            "RetryFailedRowsWorkflow",
            &json!({
                "orgId": claims.org,
                "jobId": id,
                "ruleTemplateId": target.rule_template_id,
                "ruleTemplateVersion": target.rule_template_version,
                "fromStart": opts.from_start,
            }),
        )
        .await
        .map_err(start_error)?;

    record_audit(
        &state.db,
        claims.org,
        &claims.sub,
        "job",
        Some(id.to_string()),
        "retry_all_failed",
        None,
        Some(&json!({"from_start": opts.from_start, "rows": n})),
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
