//! Migration plans: a coordinator's dependency tree of steps, designed before
//! files exist. Files arrive later and become plan runs; a run waits until
//! every step it depends on has a run that met that step's completion rule
//! (orchestrator `PlanRunWorkflow`). Archives become plan intakes that are
//! unpacked and fanned out into runs (`PlanArchiveWorkflow`).

use std::collections::{HashMap, HashSet};

use axum::extract::{Path, State};
use axum::routing::{get, post};
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

pub fn router() -> Router<AppState> {
    Router::new()
        .route("/plans", get(list_plans).post(create_plan))
        .route("/plans/:id", get(get_plan).put(update_plan))
        .route("/plans/:id/publish", post(publish_plan))
        .route("/plans/:id/revise", post(revise_plan))
        .route("/plans/:id/runs", post(create_run))
        .route("/plan-runs/:id/cancel", post(cancel_run))
}

/* ------------------------------------------------------------------ types */

#[derive(Debug, Clone, Deserialize, Serialize, FromRow)]
pub struct StepInput {
    pub step_key: String,
    #[serde(default)]
    pub label: Option<String>,
    #[serde(default)]
    pub depends_on: Vec<String>,
    #[serde(default)]
    pub default_template_key: Option<String>,
    #[serde(default)]
    pub file_pattern: Option<String>,
    #[serde(default = "default_rule")]
    pub completion_rule: String,
    #[serde(default)]
    pub max_failed_pct: Option<f64>,
    #[serde(default)]
    pub position: Option<Value>,
}

fn default_rule() -> String {
    "strict".into()
}

#[derive(Debug, Deserialize)]
pub struct PlanInput {
    pub name: String,
    #[serde(default)]
    pub description: Option<String>,
    #[serde(default)]
    pub steps: Vec<StepInput>,
}

#[derive(Debug, Serialize, FromRow)]
pub struct PlanRow {
    pub id: i64,
    pub org_id: i64,
    pub plan_key: String,
    pub version: i32,
    pub name: String,
    pub description: Option<String>,
    pub status: String,
    pub created_at: DateTime<Utc>,
    pub published_at: Option<DateTime<Utc>>,
    pub updated_at: DateTime<Utc>,
}

const PLAN_COLS: &str =
    "id, org_id, plan_key, version, name, description, status, created_at, published_at, updated_at";

const STEP_COLS: &str = "step_key, label, depends_on, default_template_key, file_pattern, completion_rule, max_failed_pct::float8 AS max_failed_pct, position";

/// Live state of one step across every run of the plan (all versions).
#[derive(Debug, Serialize, FromRow)]
pub struct StepStatus {
    pub step_key: String,
    pub completed: bool,
    pub running: bool,
    pub waiting: bool,
    pub blocked: bool,
    pub latest_status: Option<String>,
    pub latest_reason: Option<String>,
    pub latest_job_id: Option<i64>,
    pub runs: i64,
}

#[derive(Debug, Serialize, FromRow)]
pub struct RunRow {
    pub id: i64,
    pub plan_id: i64,
    pub plan_version: i32,
    pub step_key: Option<String>,
    pub intake_id: Option<i64>,
    pub filename: Option<String>,
    pub rule_template_id: Option<i64>,
    pub template_name: Option<String>,
    pub status: String,
    pub blocked: bool,
    pub rule_met: Option<bool>,
    pub failed_pct: Option<f64>,
    pub reason: Option<String>,
    pub job_id: Option<i64>,
    pub job_status: Option<String>,
    pub job_totals: Option<Value>,
    pub schedule_id: Option<i64>,
    pub created_at: DateTime<Utc>,
    pub started_at: Option<DateTime<Utc>>,
    pub finished_at: Option<DateTime<Utc>>,
}

const RUN_SELECT: &str =
    "SELECT r.id, r.plan_id, p.version AS plan_version, r.step_key, r.intake_id, r.filename,
       r.rule_template_id, t.name AS template_name, r.status, r.blocked, r.rule_met,
       r.failed_pct::float8 AS failed_pct, r.reason, r.job_id, j.status::text AS job_status,
       j.totals_json AS job_totals, r.schedule_id, r.created_at, r.started_at, r.finished_at
FROM plan_runs r
JOIN plans p ON p.id = r.plan_id
LEFT JOIN rule_templates t ON t.id = r.rule_template_id
LEFT JOIN jobs j ON j.id = r.job_id";

#[derive(Debug, Serialize, FromRow)]
pub struct IntakeRow {
    pub id: i64,
    pub plan_id: i64,
    pub filename: Option<String>,
    pub status: String,
    pub error_message: Option<String>,
    pub runs: i64,
    pub created_at: DateTime<Utc>,
}

/* ------------------------------------------------------------- validation */

fn safe_key(s: &str) -> bool {
    let b = s.as_bytes();
    !b.is_empty()
        && b.len() <= 128
        && b[0].is_ascii_alphanumeric()
        && b.iter()
            .all(|c| c.is_ascii_alphanumeric() || matches!(c, b'.' | b'_' | b'-'))
}

/// Checks a plan's steps: names, dependencies (known, acyclic), completion
/// rules and file patterns. Template keys are checked against the database
/// separately.
pub fn validate_steps(steps: &[StepInput]) -> Result<(), String> {
    if steps.is_empty() {
        return Err("a plan needs at least one step".into());
    }
    let mut keys = HashSet::new();
    for s in steps {
        if !safe_key(&s.step_key) {
            return Err(format!(
                "step name {:?}: use letters, digits, '.', '_' or '-', starting with a letter or digit",
                s.step_key
            ));
        }
        if !keys.insert(s.step_key.as_str()) {
            return Err(format!("two steps are called {:?}", s.step_key));
        }
        match s.completion_rule.as_str() {
            "strict" => {}
            "max_failed_pct" => match s.max_failed_pct {
                Some(p) if (0.0..=100.0).contains(&p) => {}
                _ => {
                    return Err(format!(
                        "step {:?}: the allowed failure percentage must be between 0 and 100",
                        s.step_key
                    ))
                }
            },
            other => {
                return Err(format!(
                    "step {:?}: unknown completion rule {other:?}",
                    s.step_key
                ))
            }
        }
        if let Some(p) = s
            .file_pattern
            .as_deref()
            .map(str::trim)
            .filter(|p| !p.is_empty())
        {
            if p.contains('/') || p.contains('\\') || p.len() > 200 {
                return Err(format!(
                    "step {:?}: a file pattern matches a file name, e.g. accounts_*.csv (no folders)",
                    s.step_key
                ));
            }
            if p.matches('[').count() != p.matches(']').count() {
                return Err(format!(
                    "step {:?}: unbalanced [ ] in file pattern",
                    s.step_key
                ));
            }
        }
    }
    for s in steps {
        for d in &s.depends_on {
            if d == &s.step_key {
                return Err(format!("step {:?} depends on itself", s.step_key));
            }
            if !keys.contains(d.as_str()) {
                return Err(format!(
                    "step {:?} depends on unknown step {d:?}",
                    s.step_key
                ));
            }
        }
    }
    // Kahn's algorithm: every step must be reachable without a loop.
    let mut indeg: HashMap<&str, usize> = steps
        .iter()
        .map(|s| (s.step_key.as_str(), s.depends_on.len()))
        .collect();
    let mut ready: Vec<&str> = indeg
        .iter()
        .filter(|(_, n)| **n == 0)
        .map(|(k, _)| *k)
        .collect();
    let mut seen = 0;
    while let Some(k) = ready.pop() {
        seen += 1;
        for s in steps.iter().filter(|s| s.depends_on.iter().any(|d| d == k)) {
            let n = indeg.get_mut(s.step_key.as_str()).expect("known step");
            *n -= 1;
            if *n == 0 {
                ready.push(&s.step_key);
            }
        }
    }
    if seen < steps.len() {
        return Err("the dependencies form a loop".into());
    }
    Ok(())
}

/// Explains why a file of `file_format` cannot feed a template whose source
/// type is `template_type`; `None` when it can. Mirrors the orchestrator's
/// `activities.FormatMismatch`.
pub fn format_mismatch(template_type: &str, file_format: &str, filename: &str) -> Option<String> {
    if !matches!(template_type, "csv" | "json" | "xml") {
        return None;
    }
    if file_format == "archive" {
        return Some(format!(
            "{filename} is an archive; a single step needs a {} file",
            template_type.to_uppercase()
        ));
    }
    (file_format != template_type).then(|| {
        format!(
            "{filename} is {} but the template reads {}",
            file_format.to_uppercase(),
            template_type.to_uppercase()
        )
    })
}

async fn check_template_keys(db: &sqlx::PgPool, org: i64, steps: &[StepInput]) -> ApiResult<()> {
    for s in steps {
        let Some(key) = s.default_template_key.as_deref().filter(|k| !k.is_empty()) else {
            continue;
        };
        let found: Option<i64> = sqlx::query_scalar(
            "SELECT id FROM rule_templates WHERE org_id = $1 AND template_key = $2 AND published LIMIT 1",
        )
        .bind(org)
        .bind(key)
        .fetch_optional(db)
        .await?;
        if found.is_none() {
            return Err(ApiError::BadRequest(format!(
                "step {:?}: template {key:?} has no published version",
                s.step_key
            )));
        }
    }
    Ok(())
}

/* --------------------------------------------------------------- handlers */

async fn load_plan(db: &sqlx::PgPool, org: i64, id: i64) -> ApiResult<PlanRow> {
    sqlx::query_as(&format!(
        "SELECT {PLAN_COLS} FROM plans WHERE id = $1 AND org_id = $2"
    ))
    .bind(id)
    .bind(org)
    .fetch_optional(db)
    .await?
    .ok_or(ApiError::NotFound)
}

async fn load_steps(db: &sqlx::PgPool, plan_id: i64) -> ApiResult<Vec<StepInput>> {
    Ok(sqlx::query_as(&format!(
        "SELECT {STEP_COLS} FROM plan_steps WHERE plan_id = $1 ORDER BY ordinal, id"
    ))
    .bind(plan_id)
    .fetch_all(db)
    .await?)
}

async fn write_steps(
    tx: &mut sqlx::Transaction<'_, sqlx::Postgres>,
    plan_id: i64,
    steps: &[StepInput],
) -> ApiResult<()> {
    sqlx::query("DELETE FROM plan_steps WHERE plan_id = $1")
        .bind(plan_id)
        .execute(&mut **tx)
        .await?;
    for (i, s) in steps.iter().enumerate() {
        let max_pct = (s.completion_rule == "max_failed_pct")
            .then_some(s.max_failed_pct)
            .flatten();
        sqlx::query(
            "INSERT INTO plan_steps (plan_id, step_key, label, ordinal, depends_on, default_template_key,
                file_pattern, completion_rule, max_failed_pct, position)
             VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9::float8::numeric, $10)",
        )
        .bind(plan_id)
        .bind(&s.step_key)
        .bind(s.label.as_deref().filter(|l| !l.trim().is_empty()))
        .bind(i as i32)
        .bind(&s.depends_on)
        .bind(s.default_template_key.as_deref().filter(|k| !k.is_empty()))
        .bind(s.file_pattern.as_deref().map(str::trim).filter(|p| !p.is_empty()))
        .bind(&s.completion_rule)
        .bind(max_pct)
        .bind(&s.position)
        .execute(&mut **tx)
        .await?;
    }
    Ok(())
}

#[derive(Serialize, FromRow)]
struct PlanSummary {
    #[sqlx(flatten)]
    #[serde(flatten)]
    plan: PlanRow,
    steps: i64,
    steps_completed: i64,
    runs_waiting: i64,
    runs_running: i64,
    runs_failed: i64,
}

async fn list_plans(
    State(state): State<AppState>,
    AuthUser(claims): AuthUser,
) -> ApiResult<Json<Value>> {
    // Latest version per plan, with progress across all versions.
    let items: Vec<PlanSummary> = sqlx::query_as(&format!(
        "WITH latest AS (
           SELECT DISTINCT ON (plan_key) {PLAN_COLS} FROM plans
           WHERE org_id = $1 ORDER BY plan_key, version DESC
         )
         SELECT l.*,
           (SELECT count(*) FROM plan_steps s WHERE s.plan_id = l.id) AS steps,
           (SELECT count(DISTINCT r.step_key) FROM plan_runs r JOIN plans p ON p.id = r.plan_id
              JOIN plan_steps s ON s.plan_id = l.id AND s.step_key = r.step_key
              WHERE p.org_id = $1 AND p.plan_key = l.plan_key AND r.status = 'completed') AS steps_completed,
           (SELECT count(*) FROM plan_runs r JOIN plans p ON p.id = r.plan_id
              WHERE p.org_id = $1 AND p.plan_key = l.plan_key AND r.status = 'waiting') AS runs_waiting,
           (SELECT count(*) FROM plan_runs r JOIN plans p ON p.id = r.plan_id
              WHERE p.org_id = $1 AND p.plan_key = l.plan_key AND r.status = 'running') AS runs_running,
           (SELECT count(*) FROM plan_runs r JOIN plans p ON p.id = r.plan_id
              WHERE p.org_id = $1 AND p.plan_key = l.plan_key AND r.status = 'failed') AS runs_failed
         FROM latest l ORDER BY l.updated_at DESC"
    ))
    .bind(claims.org)
    .fetch_all(&state.db)
    .await?;
    Ok(Json(json!({ "items": items })))
}

async fn create_plan(
    State(state): State<AppState>,
    AuthUser(claims): AuthUser,
    Json(req): Json<PlanInput>,
) -> ApiResult<Json<Value>> {
    require_role(&claims, &["admin", "editor", "operator"])?;
    let name = req.name.trim();
    if name.is_empty() {
        return Err(ApiError::BadRequest("name the plan".into()));
    }
    // A new plan may start empty; steps are validated whenever present.
    if !req.steps.is_empty() {
        validate_steps(&req.steps).map_err(ApiError::BadRequest)?;
        check_template_keys(&state.db, claims.org, &req.steps).await?;
    }
    let created_by: i64 = claims.sub.parse().map_err(|_| ApiError::Unauthorized)?;
    let plan_key = format!("plan_{}", uuid::Uuid::new_v4().simple());
    let mut tx = state.db.begin().await?;
    let plan: PlanRow = sqlx::query_as(&format!(
        "INSERT INTO plans (org_id, plan_key, version, name, description, created_by)
         VALUES ($1, $2, 1, $3, $4, $5) RETURNING {PLAN_COLS}"
    ))
    .bind(claims.org)
    .bind(&plan_key)
    .bind(name)
    .bind(req.description.as_deref())
    .bind(created_by)
    .fetch_one(&mut *tx)
    .await?;
    write_steps(&mut tx, plan.id, &req.steps).await?;
    tx.commit().await?;
    record_audit(
        &state.db,
        claims.org,
        &claims.sub,
        "plan",
        Some(plan.id.to_string()),
        "create",
        None,
        Some(&serde_json::to_value(&plan)?),
    )
    .await?;
    plan_detail(&state, claims.org, plan.id).await
}

async fn update_plan(
    State(state): State<AppState>,
    AuthUser(claims): AuthUser,
    Path(id): Path<i64>,
    Json(req): Json<PlanInput>,
) -> ApiResult<Json<Value>> {
    require_role(&claims, &["admin", "editor", "operator"])?;
    let plan = load_plan(&state.db, claims.org, id).await?;
    if plan.status != "draft" {
        return Err(ApiError::Conflict(
            "a published plan cannot change; revise it to create the next version".into(),
        ));
    }
    if req.name.trim().is_empty() {
        return Err(ApiError::BadRequest("name the plan".into()));
    }
    if !req.steps.is_empty() {
        validate_steps(&req.steps).map_err(ApiError::BadRequest)?;
        check_template_keys(&state.db, claims.org, &req.steps).await?;
    }
    let mut tx = state.db.begin().await?;
    sqlx::query("UPDATE plans SET name = $2, description = $3, updated_at = now() WHERE id = $1")
        .bind(id)
        .bind(req.name.trim())
        .bind(req.description.as_deref())
        .execute(&mut *tx)
        .await?;
    write_steps(&mut tx, id, &req.steps).await?;
    tx.commit().await?;
    plan_detail(&state, claims.org, id).await
}

async fn publish_plan(
    State(state): State<AppState>,
    AuthUser(claims): AuthUser,
    Path(id): Path<i64>,
) -> ApiResult<Json<Value>> {
    require_role(&claims, &["admin", "editor", "operator"])?;
    let plan = load_plan(&state.db, claims.org, id).await?;
    if plan.status == "published" {
        return plan_detail(&state, claims.org, id).await;
    }
    let steps = load_steps(&state.db, id).await?;
    validate_steps(&steps).map_err(ApiError::BadRequest)?;
    check_template_keys(&state.db, claims.org, &steps).await?;
    sqlx::query("UPDATE plans SET status = 'published', published_at = now(), updated_at = now() WHERE id = $1 AND status = 'draft'")
        .bind(id)
        .execute(&state.db)
        .await?;
    record_audit(
        &state.db,
        claims.org,
        &claims.sub,
        "plan",
        Some(id.to_string()),
        "publish",
        None,
        None,
    )
    .await?;
    plan_detail(&state, claims.org, id).await
}

/// Next draft version of a published plan (or the existing newer draft).
async fn revise_plan(
    State(state): State<AppState>,
    AuthUser(claims): AuthUser,
    Path(id): Path<i64>,
) -> ApiResult<Json<Value>> {
    require_role(&claims, &["admin", "editor", "operator"])?;
    let plan = load_plan(&state.db, claims.org, id).await?;
    let draft: Option<i64> = sqlx::query_scalar(
        "SELECT id FROM plans WHERE org_id = $1 AND plan_key = $2 AND status = 'draft' ORDER BY version DESC LIMIT 1",
    )
    .bind(claims.org)
    .bind(&plan.plan_key)
    .fetch_optional(&state.db)
    .await?;
    if let Some(draft) = draft {
        return plan_detail(&state, claims.org, draft).await;
    }
    let steps = load_steps(&state.db, id).await?;
    let created_by: i64 = claims.sub.parse().map_err(|_| ApiError::Unauthorized)?;
    let mut tx = state.db.begin().await?;
    let next: PlanRow = sqlx::query_as(&format!(
        "INSERT INTO plans (org_id, plan_key, version, name, description, created_by)
         SELECT org_id, plan_key, (SELECT max(version) + 1 FROM plans WHERE org_id = $1 AND plan_key = $2), name, description, $4
         FROM plans WHERE id = $3
         RETURNING {PLAN_COLS}"
    ))
    .bind(claims.org)
    .bind(&plan.plan_key)
    .bind(id)
    .bind(created_by)
    .fetch_one(&mut *tx)
    .await?;
    write_steps(&mut tx, next.id, &steps).await?;
    tx.commit().await?;
    plan_detail(&state, claims.org, next.id).await
}

async fn get_plan(
    State(state): State<AppState>,
    AuthUser(claims): AuthUser,
    Path(id): Path<i64>,
) -> ApiResult<Json<Value>> {
    plan_detail(&state, claims.org, id).await
}

async fn plan_detail(state: &AppState, org: i64, id: i64) -> ApiResult<Json<Value>> {
    let plan = load_plan(&state.db, org, id).await?;
    let steps = load_steps(&state.db, id).await?;
    let status: Vec<StepStatus> = sqlx::query_as(
        "SELECT r.step_key,
           bool_or(r.status = 'completed') AS completed,
           bool_or(r.status = 'running') AS running,
           bool_or(r.status = 'waiting' AND NOT r.blocked) AS waiting,
           bool_or(r.status = 'waiting' AND r.blocked) AS blocked,
           (array_agg(r.status ORDER BY r.created_at DESC, r.id DESC))[1] AS latest_status,
           (array_agg(r.reason ORDER BY r.created_at DESC, r.id DESC))[1] AS latest_reason,
           (array_agg(r.job_id ORDER BY r.created_at DESC, r.id DESC) FILTER (WHERE r.job_id IS NOT NULL))[1] AS latest_job_id,
           count(*) AS runs
         FROM plan_runs r JOIN plans p ON p.id = r.plan_id
         WHERE r.org_id = $1 AND p.plan_key = $2 AND r.step_key IS NOT NULL
         GROUP BY r.step_key",
    )
    .bind(org)
    .bind(&plan.plan_key)
    .fetch_all(&state.db)
    .await?;
    let runs: Vec<RunRow> = sqlx::query_as(&format!(
        "{RUN_SELECT} WHERE r.org_id = $1 AND p.plan_key = $2 ORDER BY r.created_at DESC, r.id DESC LIMIT 200"
    ))
    .bind(org)
    .bind(&plan.plan_key)
    .fetch_all(&state.db)
    .await?;
    let intakes: Vec<IntakeRow> = sqlx::query_as(
        "SELECT i.id, i.plan_id, i.filename, i.status, i.error_message,
           (SELECT count(*) FROM plan_runs r WHERE r.intake_id = i.id) AS runs, i.created_at
         FROM plan_intakes i JOIN plans p ON p.id = i.plan_id
         WHERE i.org_id = $1 AND p.plan_key = $2 ORDER BY i.created_at DESC LIMIT 50",
    )
    .bind(org)
    .bind(&plan.plan_key)
    .fetch_all(&state.db)
    .await?;
    let versions: Vec<(i64, i32, String)> = sqlx::query_as(
        "SELECT id, version, status FROM plans WHERE org_id = $1 AND plan_key = $2 ORDER BY version DESC",
    )
    .bind(org)
    .bind(&plan.plan_key)
    .fetch_all(&state.db)
    .await?;
    Ok(Json(json!({
        "plan": plan,
        "steps": steps,
        "step_status": status,
        "runs": runs,
        "intakes": intakes,
        "versions": versions.into_iter().map(|(id, version, status)| json!({"id": id, "version": version, "status": status})).collect::<Vec<_>>(),
    })))
}

#[derive(Debug, Deserialize, Serialize, Default)]
pub struct StepOverride {
    #[serde(
        default,
        rename = "stepKey",
        alias = "step_key",
        skip_serializing_if = "Option::is_none"
    )]
    pub step_key: Option<String>,
    #[serde(
        default,
        rename = "templateKey",
        alias = "template_key",
        skip_serializing_if = "Option::is_none"
    )]
    pub template_key: Option<String>,
}

#[derive(Debug, Deserialize)]
pub struct RunCreate {
    pub source_ref: Value,
    /// Single file: the step it feeds (required).
    pub step_key: Option<String>,
    /// Single file: the template to use; defaults to the step's template.
    pub rule_template_id: Option<i64>,
    /// Archive: per manifest stage id, the step and/or template to use.
    #[serde(default)]
    pub overrides: HashMap<String, StepOverride>,
}

fn start_never_reached_bridge(e: &anyhow::Error) -> bool {
    e.downcast_ref::<reqwest::Error>()
        .is_some_and(reqwest::Error::is_connect)
}

async fn create_run(
    State(state): State<AppState>,
    AuthUser(claims): AuthUser,
    Path(id): Path<i64>,
    Json(req): Json<RunCreate>,
) -> ApiResult<Json<Value>> {
    require_role(&claims, &["admin", "editor", "operator"])?;
    crate::security::license::require_licensed()?;
    let plan = load_plan(&state.db, claims.org, id).await?;
    if plan.status != "published" {
        return Err(ApiError::BadRequest(
            "publish the plan before running files against it".into(),
        ));
    }
    let created_by: i64 = claims.sub.parse().map_err(|_| ApiError::Unauthorized)?;
    let source_req = sources::parse_source_request(&req.source_ref)?;
    let source = sources::resolve_object(&state.db, claims.org, &source_req).await?;
    let filename = source.filename.clone().unwrap_or_else(|| {
        source
            .object_key
            .rsplit('/')
            .next()
            .unwrap_or("")
            .to_string()
    });
    let format = source
        .format
        .clone()
        .unwrap_or_else(|| sources::format_from_filename(&filename).to_string());

    if format == "archive" {
        let workflow_id = format!("plan-intake-up-{}", uuid::Uuid::new_v4().simple());
        let intake_id: i64 = sqlx::query_scalar(
            "INSERT INTO plan_intakes (org_id, plan_id, source_id, filename, temporal_workflow_id, created_by)
             VALUES ($1, $2, $3, $4, $5, $6) RETURNING id",
        )
        .bind(claims.org)
        .bind(id)
        .bind(source.id)
        .bind(&filename)
        .bind(&workflow_id)
        .bind(created_by)
        .fetch_one(&state.db)
        .await?;
        let start = state
            .temporal
            .start_workflow(
                &workflow_id,
                "PlanArchiveWorkflow",
                &json!({
                    "orgId": claims.org,
                    "intakeId": intake_id,
                    "bucket": source.bucket,
                    "key": source.object_key,
                    "filename": filename,
                    "overrides": req.overrides,
                }),
            )
            .await;
        if let Err(e) = start {
            if start_never_reached_bridge(&e) {
                sqlx::query(
                    "UPDATE plan_intakes SET status = 'failed', error_message = $2 WHERE id = $1",
                )
                .bind(intake_id)
                .bind(format!("workflow start failed: {e}"))
                .execute(&state.db)
                .await?;
            }
            return Err(ApiError::External(format!("temporal: {e}")));
        }
        record_audit(
            &state.db,
            claims.org,
            &claims.sub,
            "plan_intake",
            Some(intake_id.to_string()),
            "create",
            None,
            Some(&json!({"plan_id": id, "filename": filename})),
        )
        .await?;
        return Ok(Json(json!({ "intake_id": intake_id })));
    }

    let step_key = req
        .step_key
        .as_deref()
        .filter(|k| !k.is_empty())
        .ok_or_else(|| ApiError::BadRequest("choose the plan step this file feeds".into()))?;
    let default_template: Option<Option<String>> = sqlx::query_scalar(
        "SELECT default_template_key FROM plan_steps WHERE plan_id = $1 AND step_key = $2",
    )
    .bind(id)
    .bind(step_key)
    .fetch_optional(&state.db)
    .await?;
    let Some(default_template) = default_template else {
        return Err(ApiError::BadRequest(format!(
            "the plan has no step {step_key:?}"
        )));
    };
    let template_id = match (req.rule_template_id, default_template) {
        (Some(tid), _) => tid,
        (None, Some(key)) => sqlx::query_scalar(
            "SELECT id FROM rule_templates WHERE org_id = $1 AND template_key = $2 AND published ORDER BY version DESC LIMIT 1",
        )
        .bind(claims.org)
        .bind(&key)
        .fetch_optional(&state.db)
        .await?
        .ok_or_else(|| ApiError::BadRequest(format!("template {key:?} has no published version")))?,
        (None, None) => {
            return Err(ApiError::BadRequest(format!(
                "step {step_key:?} has no default template; choose one"
            )))
        }
    };
    let binding =
        crate::admission::admit_template(&state.db, claims.org, template_id, None).await?;
    let template_type: String = sqlx::query_scalar(
        "SELECT COALESCE(schema_json->'source'->>'type', '') FROM rule_templates WHERE id = $1",
    )
    .bind(binding.id)
    .fetch_one(&state.db)
    .await?;
    if let Some(msg) = format_mismatch(&template_type, &format, &filename) {
        return Err(ApiError::BadRequest(msg));
    }

    let mut tx = state.db.begin().await?;
    let run_id: i64 = sqlx::query_scalar(
        "INSERT INTO plan_runs (org_id, plan_id, step_key, source_id, source_ref, filename,
            rule_template_id, rule_template_version, status, created_by)
         VALUES ($1, $2, $3, $4, $5, $6, $7, $8, 'waiting', $9) RETURNING id",
    )
    .bind(claims.org)
    .bind(id)
    .bind(step_key)
    .bind(source.id)
    .bind(source.source_ref())
    .bind(&filename)
    .bind(binding.id)
    .bind(binding.version)
    .bind(created_by)
    .fetch_one(&mut *tx)
    .await?;
    let workflow_id = format!("plan-run-{run_id}");
    sqlx::query("UPDATE plan_runs SET temporal_workflow_id = $2 WHERE id = $1")
        .bind(run_id)
        .bind(&workflow_id)
        .execute(&mut *tx)
        .await?;
    tx.commit().await?;

    let start = state
        .temporal
        .start_workflow(
            &workflow_id,
            "PlanRunWorkflow",
            &json!({ "orgId": claims.org, "planRunId": run_id }),
        )
        .await;
    if let Err(e) = start {
        if start_never_reached_bridge(&e) {
            sqlx::query("UPDATE plan_runs SET status = 'failed', reason = $2, finished_at = now() WHERE id = $1 AND status = 'waiting'")
                .bind(run_id)
                .bind(format!("could not start: {e}"))
                .execute(&state.db)
                .await?;
        }
        return Err(ApiError::External(format!("temporal: {e}")));
    }
    record_audit(
        &state.db,
        claims.org,
        &claims.sub,
        "plan_run",
        Some(run_id.to_string()),
        "create",
        None,
        Some(&json!({"plan_id": id, "step_key": step_key, "filename": filename})),
    )
    .await?;
    let run: RunRow = sqlx::query_as(&format!("{RUN_SELECT} WHERE r.id = $1"))
        .bind(run_id)
        .fetch_one(&state.db)
        .await?;
    Ok(Json(json!({ "run": run })))
}

async fn cancel_run(
    State(state): State<AppState>,
    AuthUser(claims): AuthUser,
    Path(id): Path<i64>,
) -> ApiResult<Json<Value>> {
    require_role(&claims, &["admin", "editor", "operator"])?;
    let row: Option<(String, Option<String>)> = sqlx::query_as(
        "SELECT status, temporal_workflow_id FROM plan_runs WHERE id = $1 AND org_id = $2",
    )
    .bind(id)
    .bind(claims.org)
    .fetch_optional(&state.db)
    .await?;
    let Some((status, workflow_id)) = row else {
        return Err(ApiError::NotFound);
    };
    if !matches!(status.as_str(), "waiting" | "running") {
        return Err(ApiError::Conflict(format!("this run is already {status}")));
    }
    if let Some(wf) = workflow_id {
        if let Err(e) = state.temporal.cancel_workflow(&wf).await {
            tracing::warn!(plan_run = id, error = %e, "cancel plan run workflow");
        }
    }
    // A waiting run has no job yet; record it now rather than after the
    // workflow's next timer fires.
    sqlx::query(
        "UPDATE plan_runs SET status = 'cancelled', reason = 'cancelled by an operator', blocked = false,
           finished_at = now(), updated_at = now()
         WHERE id = $1 AND status = 'waiting'",
    )
    .bind(id)
    .execute(&state.db)
    .await?;
    record_audit(
        &state.db,
        claims.org,
        &claims.sub,
        "plan_run",
        Some(id.to_string()),
        "cancel",
        None,
        None,
    )
    .await?;
    let run: RunRow = sqlx::query_as(&format!("{RUN_SELECT} WHERE r.id = $1"))
        .bind(id)
        .fetch_one(&state.db)
        .await?;
    Ok(Json(json!({ "run": run })))
}

#[cfg(test)]
mod tests {
    use super::*;

    fn step(key: &str, deps: &[&str]) -> StepInput {
        StepInput {
            step_key: key.into(),
            label: None,
            depends_on: deps.iter().map(|d| d.to_string()).collect(),
            default_template_key: None,
            file_pattern: None,
            completion_rule: "strict".into(),
            max_failed_pct: None,
            position: None,
        }
    }

    #[test]
    fn accepts_a_diamond() {
        let steps = vec![
            step("accounts", &[]),
            step("contacts", &["accounts"]),
            step("orders", &["accounts"]),
            step("invoices", &["contacts", "orders"]),
        ];
        assert_eq!(validate_steps(&steps), Ok(()));
    }

    #[test]
    fn rejects_loops_unknown_steps_and_bad_names() {
        let looped = vec![step("a", &["b"]), step("b", &["a"])];
        assert!(validate_steps(&looped).unwrap_err().contains("loop"));
        assert!(validate_steps(&[step("a", &["ghost"])])
            .unwrap_err()
            .contains("unknown step"));
        assert!(validate_steps(&[step("a", &["a"])])
            .unwrap_err()
            .contains("itself"));
        assert!(validate_steps(&[step("../x", &[])])
            .unwrap_err()
            .contains("letters"));
        assert!(validate_steps(&[step("a", &[]), step("a", &[])])
            .unwrap_err()
            .contains("two steps"));
        assert!(validate_steps(&[]).is_err());
    }

    #[test]
    fn validates_completion_rules_and_patterns() {
        let mut s = step("a", &[]);
        s.completion_rule = "max_failed_pct".into();
        assert!(
            validate_steps(&[s.clone()]).is_err(),
            "a tolerance rule needs a percentage"
        );
        s.max_failed_pct = Some(5.0);
        assert_eq!(validate_steps(&[s.clone()]), Ok(()));
        s.max_failed_pct = Some(101.0);
        assert!(validate_steps(&[s.clone()]).is_err());
        let mut p = step("b", &[]);
        p.file_pattern = Some("in/accounts_*.csv".into());
        assert!(validate_steps(&[p.clone()])
            .unwrap_err()
            .contains("no folders"));
        p.file_pattern = Some("accounts_[0-9.csv".into());
        assert!(validate_steps(&[p]).unwrap_err().contains("unbalanced"));
    }

    #[test]
    fn format_mismatch_explains() {
        assert_eq!(format_mismatch("csv", "csv", "a.csv"), None);
        assert_eq!(
            format_mismatch("", "json", "a.json"),
            None,
            "untyped templates are not checked"
        );
        assert_eq!(
            format_mismatch("csv", "json", "orders.json").as_deref(),
            Some("orders.json is JSON but the template reads CSV")
        );
        assert!(format_mismatch("xml", "archive", "w.tar.gz")
            .unwrap()
            .contains("archive"));
    }
}
