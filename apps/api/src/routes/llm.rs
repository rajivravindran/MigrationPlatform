//! LLM-assisted mapping suggestions.
//!
//! `PUT /llm-config` stores an org-scoped provider config (OpenAI-compatible
//! chat-completions endpoint; API key AES-encrypted in `secrets`).
//! `POST /mapping-suggestions` sends sampled source columns/rows plus an
//! optional OpenAPI operation to the configured LLM and returns a rule
//! template draft. The LLM output is strictly validated against the
//! rule-template JSON Schema before it ever reaches the caller — generation
//! and execution stay separated, and hallucinated shapes are rejected here.

use axum::extract::State;
use axum::routing::{get, post};
use axum::{Json, Router};
use serde::Deserialize;
use serde_json::{json, Value};

use crate::error::{ApiError, ApiResult};
use crate::middleware::auth::{require_role, AuthUser};
use crate::security::audit::record_audit;
use crate::state::AppState;

pub fn router() -> Router<AppState> {
    Router::new()
        .route("/llm-config", get(get_config).put(put_config))
        .route("/mapping-suggestions", post(suggest_mapping))
}

// ---------------- org LLM config ----------------

#[derive(Deserialize)]
pub struct PutConfig {
    /// Currently only "openai-compatible" is supported.
    pub provider: Option<String>,
    /// Chat-completions base URL, e.g. https://api.openai.com/v1
    pub base_url: String,
    pub model: String,
    /// Omit to keep the existing key.
    pub api_key: Option<String>,
    pub redact_pii: Option<bool>,
    pub enabled: Option<bool>,
}

async fn get_config(
    State(state): State<AppState>,
    AuthUser(claims): AuthUser,
) -> ApiResult<Json<Value>> {
    let row: Option<(String, String, String, Option<i64>, bool, bool)> = sqlx::query_as(
        "SELECT provider, base_url, model, secret_id, redact_pii, enabled
         FROM org_llm_configs WHERE org_id = $1",
    )
    .bind(claims.org)
    .fetch_optional(&state.db)
    .await?;
    let Some((provider, base_url, model, secret_id, redact_pii, enabled)) = row else {
        return Ok(Json(json!({"configured": false})));
    };
    Ok(Json(json!({
        "configured": true,
        "provider": provider,
        "base_url": base_url,
        "model": model,
        "has_api_key": secret_id.is_some(),
        "redact_pii": redact_pii,
        "enabled": enabled,
    })))
}

async fn put_config(
    State(state): State<AppState>,
    AuthUser(claims): AuthUser,
    Json(req): Json<PutConfig>,
) -> ApiResult<Json<Value>> {
    require_role(&claims, &["admin"])?;
    let provider = req.provider.unwrap_or_else(|| "openai-compatible".to_string());
    if provider != "openai-compatible" {
        return Err(ApiError::BadRequest(format!("unsupported provider {provider:?}")));
    }
    let updated_by: i64 = claims.sub.parse().map_err(|_| ApiError::Unauthorized)?;

    let mut tx = state.db.begin().await?;
    let secret_id: Option<i64> = if let Some(api_key) = req.api_key {
        let (ct, nonce) = state.master_key.encrypt(api_key.as_bytes())?;
        let id: i64 = sqlx::query_scalar(
            "INSERT INTO secrets (org_id, name, ciphertext, nonce)
             VALUES ($1, 'llm-api-key', $2, $3)
             ON CONFLICT (org_id, name) DO UPDATE SET
                ciphertext = EXCLUDED.ciphertext,
                nonce = EXCLUDED.nonce,
                updated_at = now()
             RETURNING id",
        )
        .bind(claims.org)
        .bind(&ct)
        .bind(&nonce)
        .fetch_one(&mut *tx)
        .await?;
        Some(id)
    } else {
        sqlx::query_scalar("SELECT secret_id FROM org_llm_configs WHERE org_id = $1")
            .bind(claims.org)
            .fetch_optional(&mut *tx)
            .await?
            .flatten()
    };

    sqlx::query(
        "INSERT INTO org_llm_configs (org_id, provider, base_url, model, secret_id, redact_pii, enabled, updated_by)
         VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
         ON CONFLICT (org_id) DO UPDATE SET
            provider = EXCLUDED.provider,
            base_url = EXCLUDED.base_url,
            model = EXCLUDED.model,
            secret_id = EXCLUDED.secret_id,
            redact_pii = EXCLUDED.redact_pii,
            enabled = EXCLUDED.enabled,
            updated_by = EXCLUDED.updated_by,
            updated_at = now()",
    )
    .bind(claims.org)
    .bind(&provider)
    .bind(req.base_url.trim_end_matches('/'))
    .bind(&req.model)
    .bind(secret_id)
    .bind(req.redact_pii.unwrap_or(true))
    .bind(req.enabled.unwrap_or(true))
    .bind(updated_by)
    .execute(&mut *tx)
    .await?;
    tx.commit().await?;

    record_audit(&state.db, claims.org, &claims.sub, "llm_config", None, "update", None, None).await?;
    Ok(Json(json!({"ok": true})))
}

// ---------------- mapping suggestions ----------------

#[derive(Deserialize)]
pub struct SuggestRequest {
    /// Column names (with optional types) from the sampled source file.
    pub columns: Vec<Value>,
    /// First N sample rows (the caller should send 10-20).
    pub sample_rows: Vec<Value>,
    /// Optional imported OpenAPI spec + operation to target.
    pub spec_id: Option<i64>,
    pub operation_id: Option<String>,
    /// Free-form user guidance, e.g. "create the contact then attach address".
    pub instructions: Option<String>,
    /// Source kind for the template's `source.type` (defaults to csv).
    pub source_type: Option<String>,
}

const MAX_SAMPLE_ROWS: usize = 20;
const RULE_SCHEMA_TEXT: &str =
    include_str!("../../../../packages/rule-schema/schema/rule-template.schema.json");

async fn suggest_mapping(
    State(state): State<AppState>,
    AuthUser(claims): AuthUser,
    Json(req): Json<SuggestRequest>,
) -> ApiResult<Json<Value>> {
    require_role(&claims, &["admin", "editor"])?;
    if req.columns.is_empty() {
        return Err(ApiError::BadRequest("columns must not be empty".into()));
    }

    // Load + decrypt the org's LLM configuration.
    let cfg: Option<(String, String, Option<i64>, bool, bool)> = sqlx::query_as(
        "SELECT base_url, model, secret_id, redact_pii, enabled
         FROM org_llm_configs WHERE org_id = $1",
    )
    .bind(claims.org)
    .fetch_optional(&state.db)
    .await?;
    let Some((base_url, model, secret_id, redact_pii, enabled)) = cfg else {
        return Err(ApiError::BadRequest(
            "LLM is not configured for this organization; PUT /llm-config first".into(),
        ));
    };
    if !enabled {
        return Err(ApiError::BadRequest("LLM suggestions are disabled for this organization".into()));
    }
    let api_key = match secret_id {
        Some(sid) => {
            let row: Option<(Vec<u8>, Vec<u8>)> =
                sqlx::query_as("SELECT ciphertext, nonce FROM secrets WHERE id = $1 AND org_id = $2")
                    .bind(sid)
                    .bind(claims.org)
                    .fetch_optional(&state.db)
                    .await?;
            let (ct, nonce) = row.ok_or_else(|| ApiError::BadRequest("LLM API key secret missing".into()))?;
            let plain = state.master_key.decrypt(&ct, &nonce)?;
            String::from_utf8(plain).map_err(|_| ApiError::Internal(anyhow::anyhow!("api key not utf8")))?
        }
        None => return Err(ApiError::BadRequest("LLM API key not set; PUT /llm-config with api_key".into())),
    };

    // Optional OpenAPI operation context.
    let operation: Option<Value> = match (req.spec_id, &req.operation_id) {
        (Some(spec_id), Some(op_id)) => {
            let spec: Option<(Value, Option<String>)> =
                sqlx::query_as("SELECT spec_json, source_url FROM openapi_specs WHERE id = $1 AND org_id = $2")
                    .bind(spec_id)
                    .bind(claims.org)
                    .fetch_optional(&state.db)
                    .await?;
            let Some((spec, source_url)) = spec else { return Err(ApiError::NotFound) };
            let op = super::specs::parse_operations(&spec, source_url.as_deref())
                .into_iter()
                .find(|o| o.get("operation_id").and_then(|v| v.as_str()) == Some(op_id.as_str()));
            if op.is_none() {
                return Err(ApiError::BadRequest(format!("operation {op_id:?} not found in spec {spec_id}")));
            }
            op
        }
        _ => None,
    };

    // Redact PII before anything leaves our infrastructure.
    let mut rows: Vec<Value> = req.sample_rows.into_iter().take(MAX_SAMPLE_ROWS).collect();
    if redact_pii {
        for row in &mut rows {
            redact_value(row);
        }
    }

    let prompt = build_prompt(&req.columns, &rows, operation.as_ref(), req.instructions.as_deref(), req.source_type.as_deref());

    // One retry with the validation error appended: schema-invalid output is
    // the dominant failure mode and models usually self-correct given the error.
    let mut messages = vec![
        json!({"role": "system", "content": SYSTEM_PROMPT}),
        json!({"role": "user", "content": prompt}),
    ];
    let mut last_err = String::new();
    for _attempt in 0..2 {
        let content = call_llm(&base_url, &api_key, &model, &messages).await?;
        let candidate = extract_json(&content)
            .ok_or_else(|| ApiError::External("LLM returned no parseable JSON".into()))?;
        match super::templates::validate_template(&candidate) {
            Ok(_) => {
                record_audit(
                    &state.db, claims.org, &claims.sub, "mapping_suggestion", None, "generate",
                    None,
                    Some(&json!({"model": model, "redacted": redact_pii, "operation": req.operation_id})),
                )
                .await?;
                return Ok(Json(json!({"template": candidate, "model": model, "redacted": redact_pii})));
            }
            Err(e) => {
                last_err = e.to_string();
                messages.push(json!({"role": "assistant", "content": content}));
                messages.push(json!({
                    "role": "user",
                    "content": format!(
                        "That template failed schema validation with: {last_err}\n\
                         Return the corrected template as pure JSON only."
                    ),
                }));
            }
        }
    }
    Err(ApiError::External(format!(
        "LLM could not produce a schema-valid template: {last_err}"
    )))
}

const SYSTEM_PROMPT: &str = r#"You are a data-migration mapping assistant. You produce RuleTemplate JSON documents that map rows from a source file onto HTTP API calls.

Rules:
- Output ONLY a single JSON object, no prose and no markdown fences.
- The document must validate against the RuleTemplate JSON Schema provided by the user.
- Reference source columns with {"$from": "columnName"} — only columns that actually exist.
- Fixed strings use {"$literal": "value"}.
- For multi-step chains use "steps": [{name, mapping, destination}, ...] and reference earlier responses with {"$fromResponse": "stepName", "path": "$.jsonpath.to.field"}. Step names must be [A-Za-z_][A-Za-z0-9_]* and unique; $fromResponse may only reference strictly earlier steps. Do NOT include top-level "mapping"/"destination" when "steps" is used.
- For a single call use top-level "mapping" + "destination" and no "steps".
- destination.url may contain {placeholders} bound via destination.pathParams.
- Use "preprocess" entries (fn like builtin.uppercase, builtin.lowercase, builtin.trim, builtin.date_format) only when sample values clearly need normalization.
- Set source.schema to the provided columns. Set id to a short slug, version to 1, and pick a descriptive name."#;

fn build_prompt(
    columns: &[Value],
    rows: &[Value],
    operation: Option<&Value>,
    instructions: Option<&str>,
    source_type: Option<&str>,
) -> String {
    let mut p = String::new();
    p.push_str("RuleTemplate JSON Schema:\n");
    p.push_str(RULE_SCHEMA_TEXT);
    p.push_str("\n\nSource type: ");
    p.push_str(source_type.unwrap_or("csv"));
    p.push_str("\n\nSource columns:\n");
    p.push_str(&serde_json::to_string_pretty(columns).unwrap_or_default());
    p.push_str("\n\nSample rows (values may be redacted):\n");
    p.push_str(&serde_json::to_string_pretty(rows).unwrap_or_default());
    if let Some(op) = operation {
        p.push_str("\n\nTarget API operation (from the OpenAPI spec). Map source columns onto its request schema; required fields first:\n");
        p.push_str(&serde_json::to_string_pretty(op).unwrap_or_default());
    }
    if let Some(instr) = instructions {
        p.push_str("\n\nUser instructions:\n");
        p.push_str(instr);
    }
    p.push_str("\n\nProduce the RuleTemplate JSON now.");
    p
}

async fn call_llm(base_url: &str, api_key: &str, model: &str, messages: &[Value]) -> ApiResult<String> {
    let client = reqwest::Client::builder()
        .timeout(std::time::Duration::from_secs(60))
        .build()
        .map_err(|e| ApiError::External(e.to_string()))?;
    let resp = client
        .post(format!("{base_url}/chat/completions"))
        .bearer_auth(api_key)
        .json(&json!({
            "model": model,
            "messages": messages,
            "temperature": 0.1,
            "response_format": {"type": "json_object"},
        }))
        .send()
        .await
        .map_err(|e| ApiError::External(format!("llm request: {e}")))?;
    let status = resp.status();
    let body: Value = resp
        .json()
        .await
        .map_err(|e| ApiError::External(format!("llm response: {e}")))?;
    if !status.is_success() {
        let msg = body
            .pointer("/error/message")
            .and_then(|v| v.as_str())
            .unwrap_or("unknown provider error");
        return Err(ApiError::External(format!("llm provider {status}: {msg}")));
    }
    body.pointer("/choices/0/message/content")
        .and_then(|v| v.as_str())
        .map(str::to_string)
        .ok_or_else(|| ApiError::External("llm response missing content".into()))
}

/// Pull the first JSON object out of the model output, tolerating markdown
/// fences despite the instructions.
fn extract_json(content: &str) -> Option<Value> {
    let trimmed = content.trim();
    if let Ok(v) = serde_json::from_str::<Value>(trimmed) {
        return Some(v);
    }
    let start = trimmed.find('{')?;
    let end = trimmed.rfind('}')?;
    serde_json::from_str(&trimmed[start..=end]).ok()
}

// ---------------- PII redaction ----------------

static EMAIL_RE: std::sync::LazyLock<regex::Regex> = std::sync::LazyLock::new(|| {
    regex::Regex::new(r"(?i)\b[a-z0-9._%+-]+@[a-z0-9.-]+\.[a-z]{2,}\b").expect("email re")
});
static PHONE_RE: std::sync::LazyLock<regex::Regex> = std::sync::LazyLock::new(|| {
    regex::Regex::new(r"\+?\d[\d\s().-]{7,}\d").expect("phone re")
});
static LONG_DIGITS_RE: std::sync::LazyLock<regex::Regex> = std::sync::LazyLock::new(|| {
    // Card/SSN/account-like runs of 9+ digits.
    regex::Regex::new(r"\b\d{9,}\b").expect("digits re")
});

/// Mask obviously sensitive values in-place while keeping enough shape for
/// the LLM to infer semantics (an email stays email-shaped, etc.).
fn redact_value(v: &mut Value) {
    match v {
        Value::String(s) => {
            let mut out = EMAIL_RE.replace_all(s, "user@example.com").into_owned();
            out = LONG_DIGITS_RE.replace_all(&out, "000000000").into_owned();
            out = PHONE_RE.replace_all(&out, "+10000000000").into_owned();
            *s = out;
        }
        Value::Object(map) => {
            for (_k, vv) in map.iter_mut() {
                redact_value(vv);
            }
        }
        Value::Array(items) => {
            for vv in items {
                redact_value(vv);
            }
        }
        _ => {}
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn redacts_emails_phones_and_card_numbers() {
        let mut v = json!({
            "email": "jane.doe@corp.io",
            "phone": "+91 98765 43210",
            "card": "4111111111111111",
            "name": "Jane",
            "nested": [{"contact": "bob@x.co"}]
        });
        redact_value(&mut v);
        assert_eq!(v["email"], "user@example.com");
        assert_eq!(v["name"], "Jane");
        assert_eq!(v["nested"][0]["contact"], "user@example.com");
        let phone = v["phone"].as_str().unwrap();
        assert!(!phone.contains("98765"), "phone digits must be masked: {phone}");
        let card = v["card"].as_str().unwrap();
        assert!(!card.contains("4111111111111111"), "card digits must be masked: {card}");
    }

    #[test]
    fn extract_json_tolerates_fences() {
        let fenced = "```json\n{\"a\": 1}\n```";
        assert_eq!(extract_json(fenced).unwrap()["a"], 1);
        assert_eq!(extract_json("{\"b\":2}").unwrap()["b"], 2);
        assert!(extract_json("no json here").is_none());
    }
}
