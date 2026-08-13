use axum::extract::State;
use axum::http::{HeaderMap, StatusCode};
use axum::response::IntoResponse;
use axum::routing::get;
use axum::{Json, Router};
use serde_json::json;

use crate::state::AppState;

pub fn router() -> Router<AppState> {
    Router::new()
        .route("/healthz", get(health))
        .route("/readyz", get(ready))
        .route("/internal/license/authorize", get(authorize_work))
}

async fn authorize_work(State(state): State<AppState>, headers: HeaderMap) -> impl IntoResponse {
    let expected = state.cfg.bridge_token.as_deref().unwrap_or("");
    let supplied = headers
        .get(axum::http::header::AUTHORIZATION)
        .and_then(|v| v.to_str().ok())
        .and_then(|v| v.strip_prefix("Bearer "))
        .unwrap_or("");
    if expected.is_empty() || supplied != expected {
        return (
            StatusCode::UNAUTHORIZED,
            Json(json!({"authorized": false, "reason": "unauthorized"})),
        );
    }
    match crate::security::license::require_licensed() {
        Ok(()) => (StatusCode::OK, Json(json!({"authorized": true}))),
        Err(_) => (
            StatusCode::PAYMENT_REQUIRED,
            Json(json!({"authorized": false, "reason": "license_required"})),
        ),
    }
}

async fn health() -> impl IntoResponse {
    Json(json!({"status":"ok"}))
}

async fn ready(State(state): State<AppState>) -> impl IntoResponse {
    let db_ok = sqlx::query_scalar::<_, i32>("SELECT 1")
        .fetch_one(&state.db)
        .await
        .is_ok();
    let status = if db_ok { "ok" } else { "degraded" };
    (
        if db_ok {
            axum::http::StatusCode::OK
        } else {
            axum::http::StatusCode::SERVICE_UNAVAILABLE
        },
        Json(json!({"status": status, "db": db_ok})),
    )
}
