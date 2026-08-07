//! Schedule CRUD - backed by Temporal Schedules. Scheduled runs always use a
//! connector source (file uploads are rejected since they're one-shot).

use axum::extract::{Path, State};
use axum::response::IntoResponse;
use axum::routing::{get, post};
use axum::{Json, Router};
use serde::{Deserialize, Serialize};
use serde_json::{json, Value};

use crate::db::ScheduleRow;
use crate::error::{ApiError, ApiResult};
use crate::middleware::auth::{require_role, AuthUser};
use crate::security::audit::record_audit;
use crate::state::AppState;
use crate::temporal::normalise_spec;

pub fn router() -> Router<AppState> {
    Router::new()
        .route("/schedules", get(list_schedules).post(create_schedule))
        .route("/schedules/:id", get(get_schedule).put(update_schedule).delete(delete_schedule))
        .route("/schedules/:id/pause", post(pause_schedule))
        .route("/schedules/:id/resume", post(resume_schedule))
        .route("/schedules/:id/trigger", post(trigger_schedule))
        .route("/schedules/:id/runs", get(list_runs))
}

#[derive(Deserialize)]
pub struct CreateSchedule {
    pub name: String,
    pub rule_template_id: i64,
    pub rule_template_version: i32,
    pub connector_id: i64,
    pub spec: Value,
    pub timezone: Option<String>,
    pub overlap_policy: Option<String>,
    pub catchup_window_seconds: Option<i32>,
    pub enabled: Option<bool>,
}

#[derive(Serialize)]
pub struct ListResponse {
    pub items: Vec<ScheduleRow>,
}

async fn list_schedules(
    State(state): State<AppState>,
    AuthUser(claims): AuthUser,
) -> ApiResult<Json<ListResponse>> {
    let items: Vec<ScheduleRow> = sqlx::query_as(
        "SELECT id, org_id, name, rule_template_id, rule_template_version, connector_id, spec_json, timezone, overlap_policy::text AS overlap_policy, catchup_window_seconds, enabled, next_run_at, last_run_at, temporal_schedule_id, created_by, created_at, updated_at
         FROM schedules WHERE org_id = $1 ORDER BY name",
    )
    .bind(claims.org)
    .fetch_all(&state.db)
    .await?;
    Ok(Json(ListResponse { items }))
}

async fn create_schedule(
    State(state): State<AppState>,
    AuthUser(claims): AuthUser,
    Json(req): Json<CreateSchedule>,
) -> ApiResult<Json<ScheduleRow>> {
    require_role(&claims, &["admin", "editor"])?;
    crate::security::license::require_licensed()?;
    let normalised_spec = normalise_spec(&req.spec).map_err(|e| ApiError::BadRequest(e.to_string()))?;
    let timezone = req.timezone.unwrap_or_else(|| "UTC".to_string());
    let overlap = req.overlap_policy.unwrap_or_else(|| "skip".to_string());
    if !matches!(overlap.as_str(), "skip" | "buffer_one" | "buffer_all" | "cancel_other" | "allow_all") {
        return Err(ApiError::BadRequest(format!("invalid overlap_policy {overlap}")));
    }
    let catchup = req.catchup_window_seconds.unwrap_or(3600).max(0);
    let enabled = req.enabled.unwrap_or(true);

    let schedule_id_str = format!("sch-{}-{}", claims.org, uuid::Uuid::new_v4().simple());
    let created_by: i64 = claims.sub.parse().map_err(|_| ApiError::Unauthorized)?;

    // Insert Postgres row first so the Temporal payload can carry the DB schedule id.
    let row: ScheduleRow = sqlx::query_as(
        "INSERT INTO schedules (org_id, name, rule_template_id, rule_template_version, connector_id, spec_json, timezone, overlap_policy, catchup_window_seconds, enabled, temporal_schedule_id, created_by)
         VALUES ($1, $2, $3, $4, $5, $6, $7, $8::schedule_overlap, $9, $10, $11, $12)
         RETURNING id, org_id, name, rule_template_id, rule_template_version, connector_id, spec_json, timezone, overlap_policy::text AS overlap_policy, catchup_window_seconds, enabled, next_run_at, last_run_at, temporal_schedule_id, created_by, created_at, updated_at",
    )
    .bind(claims.org)
    .bind(&req.name)
    .bind(req.rule_template_id)
    .bind(req.rule_template_version)
    .bind(req.connector_id)
    .bind(&normalised_spec)
    .bind(&timezone)
    .bind(&overlap)
    .bind(catchup)
    .bind(enabled)
    .bind(&schedule_id_str)
    .bind(created_by)
    .fetch_one(&state.db)
    .await
    .map_err(|e| match &e {
        sqlx::Error::Database(db) if db.is_unique_violation() => {
            ApiError::Conflict(format!("schedule {} already exists", req.name))
        }
        _ => ApiError::Database(e),
    })?;

    if let Err(e) = state
        .temporal
        .create_schedule(
            &schedule_id_str,
            &normalised_spec,
            &json!({
                "orgId": claims.org,
                "scheduleId": row.id,
                "ruleTemplateId": req.rule_template_id,
                "ruleTemplateVersion": req.rule_template_version,
                "connectorId": req.connector_id,
            }),
            &overlap,
            catchup,
            &timezone,
        )
        .await
    {
        // Roll back the DB row if Temporal reject so we don't leave orphans.
        let _ = sqlx::query("DELETE FROM schedules WHERE id = $1")
            .bind(row.id)
            .execute(&state.db)
            .await;
        return Err(ApiError::External(format!("temporal: {e}")));
    }

    if !enabled {
        let _ = state.temporal.pause_schedule(&schedule_id_str).await;
    }

    record_audit(
        &state.db,
        claims.org,
        &claims.sub,
        "schedule",
        Some(row.id.to_string()),
        "create",
        None,
        Some(&serde_json::to_value(&row)?),
    )
    .await?;
    Ok(Json(row))
}

async fn get_schedule(
    State(state): State<AppState>,
    AuthUser(claims): AuthUser,
    Path(id): Path<i64>,
) -> ApiResult<Json<ScheduleRow>> {
    let row: Option<ScheduleRow> = sqlx::query_as(
        "SELECT id, org_id, name, rule_template_id, rule_template_version, connector_id, spec_json, timezone, overlap_policy::text AS overlap_policy, catchup_window_seconds, enabled, next_run_at, last_run_at, temporal_schedule_id, created_by, created_at, updated_at
         FROM schedules WHERE id = $1 AND org_id = $2",
    )
    .bind(id)
    .bind(claims.org)
    .fetch_optional(&state.db)
    .await?;
    row.map(Json).ok_or(ApiError::NotFound)
}

#[derive(Deserialize)]
pub struct UpdateSchedule {
    pub spec: Option<Value>,
    pub enabled: Option<bool>,
    pub catchup_window_seconds: Option<i32>,
    pub timezone: Option<String>,
    pub overlap_policy: Option<String>,
}

async fn update_schedule(
    State(state): State<AppState>,
    AuthUser(claims): AuthUser,
    Path(id): Path<i64>,
    Json(req): Json<UpdateSchedule>,
) -> ApiResult<Json<ScheduleRow>> {
    require_role(&claims, &["admin", "editor"])?;
    let mut existing: ScheduleRow = sqlx::query_as(
        "SELECT id, org_id, name, rule_template_id, rule_template_version, connector_id, spec_json, timezone, overlap_policy::text AS overlap_policy, catchup_window_seconds, enabled, next_run_at, last_run_at, temporal_schedule_id, created_by, created_at, updated_at
         FROM schedules WHERE id = $1 AND org_id = $2",
    )
    .bind(id)
    .bind(claims.org)
    .fetch_optional(&state.db)
    .await?
    .ok_or(ApiError::NotFound)?;

    if let Some(spec) = req.spec {
        let normalised = normalise_spec(&spec).map_err(|e| ApiError::BadRequest(e.to_string()))?;
        existing.spec_json = normalised.clone();
        if let Some(id) = existing.temporal_schedule_id.clone() {
            state
                .temporal
                .update_schedule(&id, &normalised, &json!({}))
                .await
                .map_err(|e| ApiError::External(format!("temporal: {e}")))?;
        }
    }
    if let Some(enabled) = req.enabled {
        existing.enabled = enabled;
        if let Some(id) = existing.temporal_schedule_id.clone() {
            if enabled {
                state.temporal.unpause_schedule(&id).await.map_err(|e| ApiError::External(e.to_string()))?;
            } else {
                state.temporal.pause_schedule(&id).await.map_err(|e| ApiError::External(e.to_string()))?;
            }
        }
    }
    if let Some(c) = req.catchup_window_seconds { existing.catchup_window_seconds = c.max(0); }
    if let Some(tz) = req.timezone { existing.timezone = tz; }
    if let Some(overlap) = req.overlap_policy {
        if !matches!(overlap.as_str(), "skip"|"buffer_one"|"buffer_all"|"cancel_other"|"allow_all") {
            return Err(ApiError::BadRequest(format!("invalid overlap_policy {overlap}")));
        }
        existing.overlap_policy = overlap;
    }

    let row: ScheduleRow = sqlx::query_as(
        "UPDATE schedules SET spec_json = $1, timezone = $2, overlap_policy = $3::schedule_overlap, catchup_window_seconds = $4, enabled = $5, updated_at = now()
         WHERE id = $6 AND org_id = $7
         RETURNING id, org_id, name, rule_template_id, rule_template_version, connector_id, spec_json, timezone, overlap_policy::text AS overlap_policy, catchup_window_seconds, enabled, next_run_at, last_run_at, temporal_schedule_id, created_by, created_at, updated_at",
    )
    .bind(&existing.spec_json)
    .bind(&existing.timezone)
    .bind(&existing.overlap_policy)
    .bind(existing.catchup_window_seconds)
    .bind(existing.enabled)
    .bind(id)
    .bind(claims.org)
    .fetch_one(&state.db)
    .await?;
    record_audit(&state.db, claims.org, &claims.sub, "schedule", Some(id.to_string()), "update", None, Some(&serde_json::to_value(&row)?)).await?;
    Ok(Json(row))
}

async fn delete_schedule(
    State(state): State<AppState>,
    AuthUser(claims): AuthUser,
    Path(id): Path<i64>,
) -> ApiResult<impl IntoResponse> {
    require_role(&claims, &["admin"])?;
    let sched_id: Option<String> = sqlx::query_scalar(
        "SELECT temporal_schedule_id FROM schedules WHERE id = $1 AND org_id = $2",
    )
    .bind(id)
    .bind(claims.org)
    .fetch_optional(&state.db)
    .await?
    .flatten();
    if let Some(sid) = sched_id {
        state.temporal.delete_schedule(&sid).await.map_err(|e| ApiError::External(e.to_string()))?;
    }
    let affected = sqlx::query("DELETE FROM schedules WHERE id = $1 AND org_id = $2")
        .bind(id)
        .bind(claims.org)
        .execute(&state.db)
        .await?
        .rows_affected();
    if affected == 0 { return Err(ApiError::NotFound); }
    record_audit(&state.db, claims.org, &claims.sub, "schedule", Some(id.to_string()), "delete", None, None).await?;
    Ok(Json(json!({"deleted": true})))
}

async fn pause_schedule(State(s): State<AppState>, AuthUser(c): AuthUser, Path(id): Path<i64>) -> ApiResult<impl IntoResponse> {
    require_role(&c, &["admin", "editor", "operator"])?;
    let sid = temporal_id(&s.db, id, c.org).await?;
    s.temporal.pause_schedule(&sid).await.map_err(|e| ApiError::External(e.to_string()))?;
    sqlx::query("UPDATE schedules SET enabled = false, updated_at = now() WHERE id = $1 AND org_id = $2")
        .bind(id).bind(c.org).execute(&s.db).await?;
    record_audit(&s.db, c.org, &c.sub, "schedule", Some(id.to_string()), "pause", None, None).await?;
    Ok(Json(json!({"id": id, "enabled": false})))
}

async fn resume_schedule(State(s): State<AppState>, AuthUser(c): AuthUser, Path(id): Path<i64>) -> ApiResult<impl IntoResponse> {
    require_role(&c, &["admin", "editor", "operator"])?;
    let sid = temporal_id(&s.db, id, c.org).await?;
    s.temporal.unpause_schedule(&sid).await.map_err(|e| ApiError::External(e.to_string()))?;
    sqlx::query("UPDATE schedules SET enabled = true, updated_at = now() WHERE id = $1 AND org_id = $2")
        .bind(id).bind(c.org).execute(&s.db).await?;
    record_audit(&s.db, c.org, &c.sub, "schedule", Some(id.to_string()), "resume", None, None).await?;
    Ok(Json(json!({"id": id, "enabled": true})))
}

async fn trigger_schedule(State(s): State<AppState>, AuthUser(c): AuthUser, Path(id): Path<i64>) -> ApiResult<impl IntoResponse> {
    require_role(&c, &["admin", "editor", "operator"])?;
    let sid = temporal_id(&s.db, id, c.org).await?;
    s.temporal.trigger_schedule(&sid).await.map_err(|e| ApiError::External(e.to_string()))?;
    record_audit(&s.db, c.org, &c.sub, "schedule", Some(id.to_string()), "trigger_now", None, None).await?;
    Ok(Json(json!({"id": id, "triggered": true})))
}

async fn list_runs(
    State(state): State<AppState>,
    AuthUser(claims): AuthUser,
    Path(id): Path<i64>,
) -> ApiResult<Json<Value>> {
    // Ensure ownership.
    let owned: bool = sqlx::query_scalar("SELECT EXISTS(SELECT 1 FROM schedules WHERE id = $1 AND org_id = $2)")
        .bind(id).bind(claims.org).fetch_one(&state.db).await?;
    if !owned { return Err(ApiError::NotFound); }
    let rows: Vec<(i64, Option<i64>, chrono::DateTime<chrono::Utc>, Option<chrono::DateTime<chrono::Utc>>, String)> = sqlx::query_as(
        "SELECT id, job_id, scheduled_time, actual_start_time, status::text AS status
         FROM schedule_runs WHERE schedule_id = $1 ORDER BY scheduled_time DESC LIMIT 100",
    )
    .bind(id)
    .fetch_all(&state.db)
    .await?;
    Ok(Json(json!({
        "items": rows.iter().map(|(id, job_id, sched, actual, status)| json!({
            "id": id, "job_id": job_id, "scheduled_time": sched, "actual_start_time": actual, "status": status,
        })).collect::<Vec<_>>()
    })))
}

async fn temporal_id(db: &sqlx::PgPool, id: i64, org: i64) -> ApiResult<String> {
    let sid: Option<String> = sqlx::query_scalar(
        "SELECT temporal_schedule_id FROM schedules WHERE id = $1 AND org_id = $2",
    )
    .bind(id)
    .bind(org)
    .fetch_optional(db)
    .await?
    .flatten();
    sid.ok_or(ApiError::NotFound)
}
