//! Connector CRUD. Secrets are AES-GCM encrypted via the configured
//! `MASTER_KEY` and stored alongside the connector row.

use axum::extract::{Path, State};
use axum::response::IntoResponse;
use axum::routing::{get, post};
use axum::{Json, Router};
use serde::{Deserialize, Serialize};
use serde_json::{json, Value};

use crate::db::ConnectorRow;
use crate::error::{ApiError, ApiResult};
use crate::middleware::auth::{require_role, AuthUser};
use crate::security::audit::record_audit;
use crate::state::AppState;

pub fn router() -> Router<AppState> {
    Router::new()
        .route("/connectors", get(list_connectors).post(create_connector))
        .route(
            "/connectors/:id",
            get(get_connector).delete(delete_connector),
        )
        .route("/connectors/:id/test", post(test_connector))
        .route(
            "/connectors/salesforce/oauth/callback",
            get(sf_oauth_callback),
        )
}

#[derive(Deserialize)]
pub struct CreateConnector {
    pub name: String,
    pub connector_kind: String,
    pub config_json: Value,
    pub secret: Option<String>,
}

#[derive(Serialize)]
pub struct ListResponse {
    pub items: Vec<ConnectorRow>,
}

async fn list_connectors(
    State(state): State<AppState>,
    AuthUser(claims): AuthUser,
) -> ApiResult<Json<ListResponse>> {
    let items: Vec<ConnectorRow> = sqlx::query_as(
        "SELECT id, org_id, name, connector_kind::text AS connector_kind, config_json, secret_id, created_by, created_at, updated_at
         FROM connectors WHERE org_id = $1 ORDER BY name",
    )
    .bind(claims.org)
    .fetch_all(&state.db)
    .await?;
    Ok(Json(ListResponse { items }))
}

async fn create_connector(
    State(state): State<AppState>,
    AuthUser(claims): AuthUser,
    Json(req): Json<CreateConnector>,
) -> ApiResult<Json<ConnectorRow>> {
    require_role(&claims, &["admin", "editor"])?;
    let connector_kind = match req.connector_kind.as_str() {
        "sftp" => "watched_sftp",
        other => other,
    };

    let mut tx = state.db.begin().await?;
    let secret_id: Option<i64> = if let Some(secret) = req.secret {
        let (ct, nonce) = state.master_key.encrypt(secret.as_bytes())?;
        let id: i64 = sqlx::query_scalar(
            "INSERT INTO secrets (org_id, name, ciphertext, nonce)
             VALUES ($1, $2, $3, $4) RETURNING id",
        )
        .bind(claims.org)
        .bind(format!("{}-secret", req.name))
        .bind(&ct)
        .bind(&nonce)
        .fetch_one(&mut *tx)
        .await?;
        Some(id)
    } else {
        None
    };

    let created_by: i64 = claims.sub.parse().map_err(|_| ApiError::Unauthorized)?;
    let row: ConnectorRow = sqlx::query_as(
        "INSERT INTO connectors (org_id, name, connector_kind, config_json, secret_id, created_by)
         VALUES ($1, $2, $3::connector_kind, $4, $5, $6)
         RETURNING id, org_id, name, connector_kind::text AS connector_kind, config_json, secret_id, created_by, created_at, updated_at",
    )
    .bind(claims.org)
    .bind(&req.name)
    .bind(connector_kind)
    .bind(&req.config_json)
    .bind(secret_id)
    .bind(created_by)
    .fetch_one(&mut *tx)
    .await
    .map_err(|e| match &e {
        sqlx::Error::Database(db) if db.is_unique_violation() => {
            ApiError::Conflict(format!("connector {} already exists", req.name))
        }
        _ => ApiError::Database(e),
    })?;

    tx.commit().await?;

    record_audit(
        &state.db,
        claims.org,
        &claims.sub,
        "connector",
        Some(row.id.to_string()),
        "create",
        None,
        Some(&serde_json::to_value(&row)?),
    )
    .await?;

    Ok(Json(row))
}

async fn get_connector(
    State(state): State<AppState>,
    AuthUser(claims): AuthUser,
    Path(id): Path<i64>,
) -> ApiResult<Json<ConnectorRow>> {
    let row: Option<ConnectorRow> = sqlx::query_as(
        "SELECT id, org_id, name, connector_kind::text AS connector_kind, config_json, secret_id, created_by, created_at, updated_at
         FROM connectors WHERE id = $1 AND org_id = $2",
    )
    .bind(id)
    .bind(claims.org)
    .fetch_optional(&state.db)
    .await?;
    row.map(Json).ok_or(ApiError::NotFound)
}

async fn delete_connector(
    State(state): State<AppState>,
    AuthUser(claims): AuthUser,
    Path(id): Path<i64>,
) -> ApiResult<impl IntoResponse> {
    require_role(&claims, &["admin"])?;
    let affected = sqlx::query("DELETE FROM connectors WHERE id = $1 AND org_id = $2")
        .bind(id)
        .bind(claims.org)
        .execute(&state.db)
        .await?
        .rows_affected();
    if affected == 0 {
        return Err(ApiError::NotFound);
    }
    record_audit(
        &state.db,
        claims.org,
        &claims.sub,
        "connector",
        Some(id.to_string()),
        "delete",
        None,
        None,
    )
    .await?;
    Ok(Json(json!({"deleted": true})))
}

async fn test_connector(
    State(state): State<AppState>,
    AuthUser(claims): AuthUser,
    Path(id): Path<i64>,
) -> ApiResult<impl IntoResponse> {
    require_role(&claims, &["admin", "editor", "operator"])?;
    let kind: Option<String> = sqlx::query_scalar(
        "SELECT connector_kind::text FROM connectors WHERE id = $1 AND org_id = $2",
    )
    .bind(id)
    .bind(claims.org)
    .fetch_optional(&state.db)
    .await?;
    let kind = kind.ok_or(ApiError::NotFound)?;
    if kind != "watched_sftp" {
        return Err(ApiError::BadRequest(format!(
            "real connector test is not implemented for kind {kind}"
        )));
    }
    state
        .temporal
        .test_sftp_connector(claims.org, id)
        .await
        .map_err(|e| ApiError::External(format!("connector validation: {e}")))?;
    Ok(Json(
        json!({"id": id, "ok": true, "message": "SFTP handshake and bounded listing succeeded"}),
    ))
}

#[derive(Deserialize)]
pub struct OauthCallback {
    pub code: String,
    pub state: String,
}

async fn sf_oauth_callback(
    State(_state): State<AppState>,
    axum::extract::Query(q): axum::extract::Query<OauthCallback>,
) -> impl IntoResponse {
    tracing::info!(
        code_len = q.code.len(),
        state = q.state,
        "salesforce oauth callback"
    );
    Json(
        json!({"ok": true, "instructions":"exchange the code server-side then call POST /connectors"}),
    )
}
