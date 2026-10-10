//! Destination secrets (ADR 0003 §5): credentials that rule templates may
//! reference by name through `destination.auth.secretRef`. Values are
//! write-only: they are encrypted at rest, never returned, and never
//! written to the audit log.

use axum::extract::{Path, State};
use axum::routing::get;
use axum::{Json, Router};
use chrono::{DateTime, Utc};
use serde::{Deserialize, Serialize};
use serde_json::json;

use crate::error::{ApiError, ApiResult};
use crate::middleware::auth::{require_role, AuthUser};
use crate::security::audit::record_audit;
use crate::state::AppState;

const MAX_SECRET_BYTES: usize = 8 * 1024;

pub fn router() -> Router<AppState> {
    Router::new().route("/secrets", get(list_secrets)).route(
        "/secrets/:name",
        get(get_secret).put(put_secret).delete(revoke_secret),
    )
}

#[derive(Serialize, sqlx::FromRow)]
pub struct SecretMeta {
    pub name: String,
    pub created_at: DateTime<Utc>,
    pub updated_at: DateTime<Utc>,
    pub revoked_at: Option<DateTime<Utc>>,
}

#[derive(Deserialize)]
pub struct PutSecret {
    pub value: String,
}

/// Same pattern as the template schema's `auth.secretRef`.
fn validate_name(name: &str) -> ApiResult<()> {
    let ok = (1..=128).contains(&name.len())
        && name
            .bytes()
            .all(|b| b.is_ascii_alphanumeric() || matches!(b, b'_' | b'.' | b'-'));
    if ok {
        Ok(())
    } else {
        Err(ApiError::BadRequest(
            "secret name must be 1-128 characters of A-Z, a-z, 0-9, '_', '.', '-'".into(),
        ))
    }
}

async fn list_secrets(
    State(state): State<AppState>,
    AuthUser(claims): AuthUser,
) -> ApiResult<Json<serde_json::Value>> {
    require_role(&claims, &["admin", "editor"])?;
    let items: Vec<SecretMeta> = sqlx::query_as(
        "SELECT name, created_at, updated_at, revoked_at FROM secrets
         WHERE org_id = $1 AND kind = 'destination' ORDER BY name",
    )
    .bind(claims.org)
    .fetch_all(&state.db)
    .await?;
    Ok(Json(json!({ "items": items })))
}

async fn get_secret(
    State(state): State<AppState>,
    AuthUser(claims): AuthUser,
    Path(name): Path<String>,
) -> ApiResult<Json<SecretMeta>> {
    require_role(&claims, &["admin", "editor"])?;
    validate_name(&name)?;
    let meta: Option<SecretMeta> = sqlx::query_as(
        "SELECT name, created_at, updated_at, revoked_at FROM secrets
         WHERE org_id = $1 AND name = $2 AND kind = 'destination'",
    )
    .bind(claims.org)
    .bind(&name)
    .fetch_optional(&state.db)
    .await?;
    meta.map(Json).ok_or(ApiError::NotFound)
}

/// Create or rotate. A revoked secret becomes active again with the new
/// value. A name already used by an internal (connector or LLM) secret is
/// refused, so a template can never be pointed at one.
async fn put_secret(
    State(state): State<AppState>,
    AuthUser(claims): AuthUser,
    Path(name): Path<String>,
    Json(req): Json<PutSecret>,
) -> ApiResult<Json<SecretMeta>> {
    require_role(&claims, &["admin"])?;
    validate_name(&name)?;
    if req.value.is_empty() || req.value.len() > MAX_SECRET_BYTES {
        return Err(ApiError::BadRequest(format!(
            "secret value must be 1-{MAX_SECRET_BYTES} bytes"
        )));
    }
    let (ct, nonce) = state.master_key.encrypt(req.value.as_bytes())?;
    let mut tx = state.db.begin().await?;
    let previous: Option<(String, Option<DateTime<Utc>>)> = sqlx::query_as(
        "SELECT kind, revoked_at FROM secrets WHERE org_id = $1 AND name = $2 FOR UPDATE",
    )
    .bind(claims.org)
    .bind(&name)
    .fetch_optional(&mut *tx)
    .await?;
    if matches!(&previous, Some((kind, _)) if kind != "destination") {
        return Err(ApiError::Conflict(format!(
            "secret name {name:?} is used by a connector or LLM credential; choose another name"
        )));
    }
    let meta: SecretMeta = sqlx::query_as(
        "INSERT INTO secrets (org_id, name, ciphertext, nonce, kind)
         VALUES ($1, $2, $3, $4, 'destination')
         ON CONFLICT (org_id, name) DO UPDATE SET
             ciphertext = EXCLUDED.ciphertext, nonce = EXCLUDED.nonce,
             revoked_at = NULL, updated_at = now()
         RETURNING name, created_at, updated_at, revoked_at",
    )
    .bind(claims.org)
    .bind(&name)
    .bind(&ct)
    .bind(&nonce)
    .fetch_one(&mut *tx)
    .await?;
    tx.commit().await?;

    let action = match previous {
        None => "create",
        Some((_, Some(_))) => "reactivate",
        Some(_) => "rotate",
    };
    record_audit(
        &state.db,
        claims.org,
        &claims.sub,
        "destination_secret",
        Some(name.clone()),
        action,
        None,
        None,
    )
    .await?;
    tracing::info!(org = claims.org, secret = %name, action, "destination secret updated");
    Ok(Json(meta))
}

/// Revoke: calls that reference the secret fail without sending, and the
/// ciphertext is erased.
async fn revoke_secret(
    State(state): State<AppState>,
    AuthUser(claims): AuthUser,
    Path(name): Path<String>,
) -> ApiResult<Json<SecretMeta>> {
    require_role(&claims, &["admin"])?;
    validate_name(&name)?;
    let meta: Option<SecretMeta> = sqlx::query_as(
        "UPDATE secrets SET revoked_at = now(), updated_at = now(),
             ciphertext = ''::bytea, nonce = ''::bytea
         WHERE org_id = $1 AND name = $2 AND kind = 'destination' AND revoked_at IS NULL
         RETURNING name, created_at, updated_at, revoked_at",
    )
    .bind(claims.org)
    .bind(&name)
    .fetch_optional(&state.db)
    .await?;
    let Some(meta) = meta else {
        return Err(ApiError::NotFound);
    };
    record_audit(
        &state.db,
        claims.org,
        &claims.sub,
        "destination_secret",
        Some(name.clone()),
        "revoke",
        None,
        None,
    )
    .await?;
    tracing::info!(org = claims.org, secret = %name, "destination secret revoked");
    Ok(Json(meta))
}

#[cfg(test)]
mod tests {
    use super::validate_name;

    #[test]
    fn secret_names_are_restricted() {
        for ok in ["crm_token", "crm.token-2", "A"] {
            assert!(validate_name(ok).is_ok(), "{ok}");
        }
        let long = "x".repeat(129);
        for bad in ["", "../etc", "a b", "tok/en", "名前", long.as_str()] {
            assert!(validate_name(bad).is_err(), "{bad}");
        }
    }
}
