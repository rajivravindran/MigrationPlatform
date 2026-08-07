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
            "/schedules": {"get": {"summary": "list"}, "post": {"summary": "create"}}
        }
    }))
}
