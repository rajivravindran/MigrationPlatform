//! Lightweight OpenAPI description served at /openapi.json and a Swagger UI
//! mount at /docs. The full typed spec is generated at build time from utoipa
//! macros on individual routes - for brevity we ship a static JSON here.

use axum::response::{IntoResponse, Redirect};
use axum::routing::get;
use axum::{Json, Router};
use serde_json::json;

use crate::state::AppState;

pub fn router() -> Router<AppState> {
    Router::new()
        .route("/openapi.json", get(spec))
        .route("/docs", get(|| async { Redirect::temporary("/docs/") }))
}

async fn spec() -> impl IntoResponse {
    Json(json!({
        "openapi": "3.0.3",
        "info": {"title": "Migration Platform API", "version": env!("CARGO_PKG_VERSION")},
        "paths": {
            "/healthz": {"get": {"summary": "liveness", "responses": {"200": {"description": "ok"}}}},
            "/readyz": {"get": {"summary": "readiness", "responses": {"200": {"description": "ok"}}}},
            "/auth/login": {"post": {"summary": "issue JWT", "responses": {"200": {"description": "ok"}}}},
            "/auth/me": {"get": {"summary": "current user", "responses": {"200": {"description": "ok"}}}},
            "/rule-templates": {"get": {"summary": "list"}, "post": {"summary": "create"}},
            "/jobs": {"get": {"summary": "list"}, "post": {"summary": "create"}},
            "/jobs/{id}/stream": {"get": {"summary": "SSE progress"}},
            "/jobs/{id}/rows/{row_index}/retry": {"post": {"summary": "retry one row; body {from_start, confirm_not_applied}; 409 while the job or another retry runs, or for an unresolved row without confirm_not_applied"}},
            "/jobs/{id}/retry-failed": {"post": {"summary": "retry failed rows (not unresolved ones); 409 while the job or another retry runs"}},
            "/jobs/{id}/pause": {"post": {"summary": "pause a running job; 409 in any other state"}},
            "/jobs/{id}/resume": {"post": {"summary": "resume a paused job; 409 in any other state"}},
            "/jobs/{id}/cancel": {"post": {"summary": "cancel a pending, running or paused job; 409 once finished"}},
            "/jobs/{id}/export": {"post": {"summary": "re-run only the results export of a finished job (no destination calls); 202, or 409 while the job or another export runs"}},
            "/secrets": {"get": {"summary": "list destination secret names (never values)"}},
            "/secrets/{name}": {"get": {"summary": "secret metadata"}, "put": {"summary": "create or rotate (admin); body {value}"}, "delete": {"summary": "revoke (admin)"}},
            "/schedules": {"get": {"summary": "list"}, "post": {"summary": "create"}}
        }
    }))
}
