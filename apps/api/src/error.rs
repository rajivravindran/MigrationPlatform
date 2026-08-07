//! Error type used across every handler. Maps cleanly to HTTP and redacts
//! sensitive fields before serializing.

use axum::http::StatusCode;
use axum::response::{IntoResponse, Response};
use axum::Json;
use serde_json::json;
use thiserror::Error;

pub type ApiResult<T> = std::result::Result<T, ApiError>;

#[derive(Debug, Error)]
pub enum ApiError {
    #[error("not found")]
    NotFound,
    #[error("forbidden")]
    Forbidden,
    #[error("license required: {0}")]
    LicenseRequired(String),
    #[error("unauthorized")]
    Unauthorized,
    #[error("bad request: {0}")]
    BadRequest(String),
    #[error("conflict: {0}")]
    Conflict(String),
    #[error("validation failed: {0}")]
    ValidationFailed(String),
    #[error("database error")]
    Database(#[from] sqlx::Error),
    #[error("json error")]
    Json(#[from] serde_json::Error),
    #[error("io error")]
    Io(#[from] std::io::Error),
    #[error("external service error: {0}")]
    External(String),
    #[error("internal error")]
    Internal(#[from] anyhow::Error),
}

impl ApiError {
    fn status_and_code(&self) -> (StatusCode, &'static str) {
        match self {
            ApiError::NotFound => (StatusCode::NOT_FOUND, "not_found"),
            ApiError::Forbidden => (StatusCode::FORBIDDEN, "forbidden"),
            ApiError::LicenseRequired(_) => (StatusCode::PAYMENT_REQUIRED, "license_required"),
            ApiError::Unauthorized => (StatusCode::UNAUTHORIZED, "unauthorized"),
            ApiError::BadRequest(_) => (StatusCode::BAD_REQUEST, "bad_request"),
            ApiError::ValidationFailed(_) => (StatusCode::UNPROCESSABLE_ENTITY, "validation_failed"),
            ApiError::Conflict(_) => (StatusCode::CONFLICT, "conflict"),
            ApiError::Database(_) | ApiError::Json(_) | ApiError::Io(_) | ApiError::Internal(_) => {
                (StatusCode::INTERNAL_SERVER_ERROR, "internal_error")
            }
            ApiError::External(_) => (StatusCode::BAD_GATEWAY, "external_error"),
        }
    }
}

impl IntoResponse for ApiError {
    fn into_response(self) -> Response {
        let (status, code) = self.status_and_code();

        if status.is_server_error() {
            tracing::error!(error = %self, "api error");
        } else {
            tracing::info!(error = %self, "api client error");
        }

        let message = match &self {
            ApiError::Database(_) | ApiError::Json(_) | ApiError::Io(_) | ApiError::Internal(_) => {
                "An unexpected error occurred".to_string()
            }
            other => other.to_string(),
        };
        (status, Json(json!({ "error": { "code": code, "message": message } }))).into_response()
    }
}
