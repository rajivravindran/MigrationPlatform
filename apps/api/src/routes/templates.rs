//! Rule-template CRUD with JSON Schema validation, version history, audit
//! logging, and draft/publish semantics.

use axum::extract::{Path, Query, State};
use axum::response::IntoResponse;
use axum::routing::{get, post};
use axum::{Json, Router};
use std::sync::LazyLock;

use jsonschema::JSONSchema;
use serde::{Deserialize, Serialize};
use serde_json::Value;

use crate::db::RuleTemplateRow;
use crate::error::{ApiError, ApiResult};
use crate::middleware::auth::{require_role, AuthUser};
use crate::rule_template_types::RuleTemplate;
use crate::security::audit::record_audit;
use crate::state::AppState;

pub fn router() -> Router<AppState> {
    Router::new()
        .route("/rule-templates", get(list_templates).post(create_template))
        .route(
            "/rule-templates/:id",
            get(get_template).delete(delete_template),
        )
        .route(
            "/rule-templates/:id/versions",
            get(list_versions).post(create_version),
        )
        .route("/rule-templates/:id/publish", post(publish_template))
}

const RULE_SCHEMA: &str =
    include_str!("../../../../packages/rule-schema/schema/rule-template.schema.json");

static COMPILED_SCHEMA: LazyLock<JSONSchema> = LazyLock::new(|| {
    let schema_value: Value = serde_json::from_str(RULE_SCHEMA).expect("schema parse");
    JSONSchema::options()
        .with_draft(jsonschema::Draft::Draft202012)
        .compile(&schema_value)
        .expect("schema compile")
});

pub(crate) fn validate_template(body: &Value) -> ApiResult<RuleTemplate> {
    if let Err(errors) = COMPILED_SCHEMA.validate(body) {
        let errs: Vec<String> = errors
            .map(|e| format!("{}: {}", e.instance_path, e))
            .collect();
        return Err(ApiError::ValidationFailed(errs.join("; ")));
    }
    let tpl: RuleTemplate = serde_json::from_value(body.clone())
        .map_err(|e| ApiError::ValidationFailed(format!("shape: {e}")))?;
    validate_step_references(&tpl)?;
    Ok(tpl)
}

/// Cross-step rules the JSON Schema cannot express: step names must be
/// unique, and every `$fromResponse` must reference a *strictly earlier* step.
fn validate_step_references(tpl: &RuleTemplate) -> ApiResult<()> {
    let steps = tpl.execution_steps();
    let mut seen: Vec<&str> = Vec::with_capacity(steps.len());
    for step in &steps {
        if seen.contains(&step.name.as_str()) {
            return Err(ApiError::ValidationFailed(format!(
                "duplicate step name {:?}",
                step.name
            )));
        }
        let mut refs = Vec::new();
        if let Some(mapping) = &step.mapping {
            collect_response_refs(&Value::Object(mapping.payload.clone()), &mut refs);
        }
        let dest = serde_json::to_value(&step.destination)?;
        collect_response_refs(&dest, &mut refs);
        for r in refs {
            if !seen.contains(&r.as_str()) {
                return Err(ApiError::ValidationFailed(format!(
                    "step {:?} references response of step {:?}, which is not an earlier step",
                    step.name, r
                )));
            }
        }
        seen.push(step.name.as_str());
    }
    Ok(())
}

fn collect_response_refs(value: &Value, out: &mut Vec<String>) {
    match value {
        Value::Object(map) => {
            if let Some(Value::String(step)) = map.get("$fromResponse") {
                out.push(step.clone());
            }
            for v in map.values() {
                collect_response_refs(v, out);
            }
        }
        Value::Array(items) => {
            for v in items {
                collect_response_refs(v, out);
            }
        }
        _ => {}
    }
}

#[derive(Deserialize)]
pub struct ListQuery {
    pub template_key: Option<String>,
    pub published: Option<bool>,
    pub limit: Option<i64>,
}

#[derive(Serialize)]
pub struct ListResponse {
    pub items: Vec<RuleTemplateRow>,
}

async fn list_templates(
    State(state): State<AppState>,
    AuthUser(claims): AuthUser,
    Query(q): Query<ListQuery>,
) -> ApiResult<Json<ListResponse>> {
    let limit = q.limit.unwrap_or(100).min(500);
    let rows: Vec<RuleTemplateRow> = if let Some(key) = q.template_key {
        sqlx::query_as(
            "SELECT id, org_id, template_key, version, name, schema_json, published, created_by, created_at
             FROM rule_templates
             WHERE org_id = $1 AND template_key = $2 AND ($3::bool IS NULL OR published = $3)
             ORDER BY version DESC LIMIT $4",
        )
        .bind(claims.org)
        .bind(key)
        .bind(q.published)
        .bind(limit)
        .fetch_all(&state.db)
        .await?
    } else {
        // One row per template_key: the latest version matching the optional
        // published filter. With published=true this returns the newest
        // *published* version even when a more recent draft exists, which is
        // what job creation needs.
        sqlx::query_as(
            "SELECT DISTINCT ON (template_key) id, org_id, template_key, version, name, schema_json, published, created_by, created_at
             FROM rule_templates
             WHERE org_id = $1 AND ($2::bool IS NULL OR published = $2)
             ORDER BY template_key, version DESC LIMIT $3",
        )
        .bind(claims.org)
        .bind(q.published)
        .bind(limit)
        .fetch_all(&state.db)
        .await?
    };
    Ok(Json(ListResponse { items: rows }))
}

#[derive(Deserialize)]
pub struct CreateRequest {
    pub template_key: Option<String>,
    pub name: String,
    pub schema_json: Value,
}

async fn create_template(
    State(state): State<AppState>,
    AuthUser(claims): AuthUser,
    Json(req): Json<CreateRequest>,
) -> ApiResult<Json<RuleTemplateRow>> {
    require_role(&claims, &["admin", "editor"])?;
    let _tpl = validate_template(&req.schema_json)?;
    let key = req
        .template_key
        .unwrap_or_else(|| format!("rt_{}", uuid::Uuid::new_v4().simple()));
    let created_by: i64 = claims.sub.parse().map_err(|_| ApiError::Unauthorized)?;
    let row: RuleTemplateRow = sqlx::query_as(
        "INSERT INTO rule_templates (org_id, template_key, version, name, schema_json, published, created_by)
         VALUES ($1, $2, 1, $3, $4, false, $5)
         RETURNING id, org_id, template_key, version, name, schema_json, published, created_by, created_at",
    )
    .bind(claims.org)
    .bind(&key)
    .bind(&req.name)
    .bind(&req.schema_json)
    .bind(created_by)
    .fetch_one(&state.db)
    .await
    .map_err(|e| match &e {
        sqlx::Error::Database(db) if db.is_unique_violation() => {
            ApiError::Conflict(format!("template_key {key} already exists"))
        }
        _ => ApiError::Database(e),
    })?;

    record_audit(
        &state.db,
        claims.org,
        &claims.sub,
        "rule_template",
        Some(row.id.to_string()),
        "create",
        None,
        Some(&serde_json::to_value(&row)?),
    )
    .await?;
    Ok(Json(row))
}

async fn get_template(
    State(state): State<AppState>,
    AuthUser(claims): AuthUser,
    Path(id): Path<i64>,
) -> ApiResult<Json<RuleTemplateRow>> {
    let row: Option<RuleTemplateRow> = sqlx::query_as(
        "SELECT id, org_id, template_key, version, name, schema_json, published, created_by, created_at
         FROM rule_templates WHERE id = $1 AND org_id = $2",
    )
    .bind(id)
    .bind(claims.org)
    .fetch_optional(&state.db)
    .await?;
    row.map(Json).ok_or(ApiError::NotFound)
}

async fn list_versions(
    State(state): State<AppState>,
    AuthUser(claims): AuthUser,
    Path(id): Path<i64>,
) -> ApiResult<Json<ListResponse>> {
    let key: Option<String> =
        sqlx::query_scalar("SELECT template_key FROM rule_templates WHERE id = $1 AND org_id = $2")
            .bind(id)
            .bind(claims.org)
            .fetch_optional(&state.db)
            .await?;
    let Some(key) = key else {
        return Err(ApiError::NotFound);
    };
    let rows: Vec<RuleTemplateRow> = sqlx::query_as(
        "SELECT id, org_id, template_key, version, name, schema_json, published, created_by, created_at
         FROM rule_templates WHERE org_id = $1 AND template_key = $2 ORDER BY version DESC",
    )
    .bind(claims.org)
    .bind(key)
    .fetch_all(&state.db)
    .await?;
    Ok(Json(ListResponse { items: rows }))
}

async fn create_version(
    State(state): State<AppState>,
    AuthUser(claims): AuthUser,
    Path(id): Path<i64>,
    Json(req): Json<CreateRequest>,
) -> ApiResult<Json<RuleTemplateRow>> {
    require_role(&claims, &["admin", "editor"])?;
    let _tpl = validate_template(&req.schema_json)?;
    let key: Option<String> =
        sqlx::query_scalar("SELECT template_key FROM rule_templates WHERE id = $1 AND org_id = $2")
            .bind(id)
            .bind(claims.org)
            .fetch_optional(&state.db)
            .await?;
    let Some(key) = key else {
        return Err(ApiError::NotFound);
    };
    let created_by: i64 = claims.sub.parse().map_err(|_| ApiError::Unauthorized)?;
    let row: RuleTemplateRow = sqlx::query_as(
        "INSERT INTO rule_templates (org_id, template_key, version, name, schema_json, published, created_by)
         SELECT $1, $2, COALESCE(MAX(version),0)+1, $3, $4, false, $5 FROM rule_templates
         WHERE org_id = $1 AND template_key = $2
         RETURNING id, org_id, template_key, version, name, schema_json, published, created_by, created_at",
    )
    .bind(claims.org)
    .bind(&key)
    .bind(&req.name)
    .bind(&req.schema_json)
    .bind(created_by)
    .fetch_one(&state.db)
    .await?;
    record_audit(
        &state.db,
        claims.org,
        &claims.sub,
        "rule_template",
        Some(row.id.to_string()),
        "create_version",
        None,
        Some(&serde_json::to_value(&row)?),
    )
    .await?;
    Ok(Json(row))
}

async fn publish_template(
    State(state): State<AppState>,
    AuthUser(claims): AuthUser,
    Path(id): Path<i64>,
) -> ApiResult<impl IntoResponse> {
    require_role(&claims, &["admin", "editor"])?;
    let row: Option<RuleTemplateRow> = sqlx::query_as(
        "UPDATE rule_templates SET published = true
         WHERE id = $1 AND org_id = $2
         RETURNING id, org_id, template_key, version, name, schema_json, published, created_by, created_at",
    )
    .bind(id)
    .bind(claims.org)
    .fetch_optional(&state.db)
    .await?;
    let row = row.ok_or(ApiError::NotFound)?;
    record_audit(
        &state.db,
        claims.org,
        &claims.sub,
        "rule_template",
        Some(row.id.to_string()),
        "publish",
        None,
        Some(&serde_json::to_value(&row)?),
    )
    .await?;
    Ok(Json(row))
}

async fn delete_template(
    State(state): State<AppState>,
    AuthUser(claims): AuthUser,
    Path(id): Path<i64>,
) -> ApiResult<impl IntoResponse> {
    require_role(&claims, &["admin"])?;
    let deleted = sqlx::query(
        "DELETE FROM rule_templates WHERE id = $1 AND org_id = $2 AND published = false",
    )
    .bind(id)
    .bind(claims.org)
    .execute(&state.db)
    .await?
    .rows_affected();
    if deleted == 0 {
        return Err(ApiError::Conflict(
            "template missing or already published; cannot delete".to_string(),
        ));
    }
    record_audit(
        &state.db,
        claims.org,
        &claims.sub,
        "rule_template",
        Some(id.to_string()),
        "delete",
        None,
        None,
    )
    .await?;
    Ok(Json(serde_json::json!({"deleted": true})))
}
