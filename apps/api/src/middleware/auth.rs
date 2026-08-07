//! JWT extraction + RBAC enforcement.

use axum::extract::FromRequestParts;
use axum::http::request::Parts;
use axum::http::StatusCode;
use axum::response::{IntoResponse, Response};

use crate::error::ApiError;
use crate::security::Claims;
use crate::state::AppState;

pub struct AuthUser(pub Claims);

#[async_trait::async_trait]
impl FromRequestParts<AppState> for AuthUser {
    type Rejection = Response;

    async fn from_request_parts(parts: &mut Parts, state: &AppState) -> Result<Self, Self::Rejection> {
        let Some(auth) = parts.headers.get("authorization").and_then(|v| v.to_str().ok()) else {
            return Err((StatusCode::UNAUTHORIZED, "missing bearer token").into_response());
        };
        let Some(token) = auth.strip_prefix("Bearer ").map(str::trim) else {
            return Err((StatusCode::UNAUTHORIZED, "invalid bearer scheme").into_response());
        };
        match state.jwt.verify(token) {
            Ok(claims) => Ok(AuthUser(claims)),
            Err(_) => Err(ApiError::Unauthorized.into_response()),
        }
    }
}

pub fn require_role(claims: &Claims, required: &[&str]) -> Result<(), ApiError> {
    if required.contains(&claims.role.as_str()) {
        Ok(())
    } else {
        Err(ApiError::Forbidden)
    }
}
