//! Dry-run preview endpoint. Accepts a rule template schema and a small set of
//! sample rows and returns the rendered payload per step for each, so the
//! Designer can show a live preview without starting a job.
//!
//! `$fromResponse` bindings cannot be resolved without firing real requests,
//! so they preview as a marker string carrying the step + JSONPath.

use axum::extract::State;
use axum::response::IntoResponse;
use axum::routing::post;
use axum::{Json, Router};
use serde::Deserialize;
use serde_json::{json, Value};

use crate::error::ApiResult;
use crate::middleware::auth::AuthUser;
use crate::state::AppState;

pub fn router() -> Router<AppState> {
    Router::new().route("/dry-run", post(dry_run))
}

#[derive(Deserialize)]
pub struct DryRunRequest {
    pub template: Value,
    pub rows: Vec<Value>,
}

async fn dry_run(
    State(_state): State<AppState>,
    AuthUser(_claims): AuthUser,
    Json(req): Json<DryRunRequest>,
) -> ApiResult<impl IntoResponse> {
    let steps = extract_steps(&req.template);
    let mut previews = Vec::with_capacity(req.rows.len());
    for row in req.rows.iter().take(20) {
        let mut per_step = Vec::with_capacity(steps.len());
        for (name, mapping) in &steps {
            per_step.push(json!({
                "step": name,
                "payload": render(mapping, row),
            }));
        }
        // Single-step templates keep the historical flat shape for backwards
        // compatibility with the existing Designer preview.
        if per_step.len() == 1 {
            previews.push(per_step[0]["payload"].clone());
        } else {
            previews.push(json!({"steps": per_step}));
        }
    }
    Ok(Json(json!({"previews": previews})))
}

/// Normalize either template shape into (stepName, payloadMapping) pairs.
fn extract_steps(template: &Value) -> Vec<(String, serde_json::Map<String, Value>)> {
    if let Some(steps) = template.get("steps").and_then(|s| s.as_array()) {
        return steps
            .iter()
            .map(|s| {
                let name = s
                    .get("name")
                    .and_then(|n| n.as_str())
                    .unwrap_or("step")
                    .to_string();
                let mapping = s
                    .get("mapping")
                    .and_then(|m| m.get("payload"))
                    .and_then(|p| p.as_object())
                    .cloned()
                    .unwrap_or_default();
                (name, mapping)
            })
            .collect();
    }
    let mapping = template
        .get("mapping")
        .and_then(|m| m.get("payload"))
        .and_then(|p| p.as_object())
        .cloned()
        .unwrap_or_default();
    vec![("main".to_string(), mapping)]
}

fn render(mapping: &serde_json::Map<String, Value>, row: &Value) -> Value {
    let mut out = serde_json::Map::new();
    for (k, v) in mapping {
        out.insert(k.clone(), resolve(v, row));
    }
    Value::Object(out)
}

fn resolve(template: &Value, row: &Value) -> Value {
    if let Some(obj) = template.as_object() {
        if let Some(field) = obj.get("$from").and_then(|v| v.as_str()) {
            return row.get(field).cloned().unwrap_or(Value::Null);
        }
        if let Some(expr) = obj.get("$py").and_then(|v| v.as_str()) {
            return Value::String(format!("<py: {expr}>"));
        }
        if let Some(step) = obj.get("$fromResponse").and_then(|v| v.as_str()) {
            let path = obj.get("path").and_then(|v| v.as_str()).unwrap_or("$");
            return Value::String(format!("<response of {step}: {path}>"));
        }
        if let Some(lit) = obj.get("$literal") {
            return lit.clone();
        }
        let mut out = serde_json::Map::new();
        for (k, v) in obj {
            out.insert(k.clone(), resolve(v, row));
        }
        return Value::Object(out);
    }
    if let Some(arr) = template.as_array() {
        return Value::Array(arr.iter().map(|v| resolve(v, row)).collect());
    }
    template.clone()
}
