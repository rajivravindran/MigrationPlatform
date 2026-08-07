use axum::extract::State;
use axum::response::IntoResponse;
use axum::routing::{get, post};
use axum::{Json, Router};
use serde::{Deserialize, Serialize};

use crate::db::User;
use crate::error::{ApiError, ApiResult};
use crate::middleware::auth::AuthUser;
use crate::security::passwords::verify_password;
use crate::state::AppState;

pub fn router() -> Router<AppState> {
    Router::new()
        .route("/auth/login", post(login))
        .route("/auth/me", get(me))
}

#[derive(Deserialize)]
pub struct LoginRequest {
    pub email: String,
    pub password: String,
}

#[derive(Serialize)]
pub struct LoginResponse {
    pub token: String,
    pub role: String,
    pub org_id: i64,
    pub user_id: i64,
}

async fn login(
    State(state): State<AppState>,
    Json(req): Json<LoginRequest>,
) -> ApiResult<Json<LoginResponse>> {
    let user: Option<User> = sqlx::query_as(
        "SELECT id, org_id, email::text AS email, role::text AS role, password_hash, disabled, created_at, updated_at
         FROM users WHERE email = $1",
    )
    .bind(&req.email)
    .fetch_optional(&state.db)
    .await?;

    let Some(user) = user else {
        return Err(ApiError::Unauthorized);
    };
    if user.disabled {
        return Err(ApiError::Forbidden);
    }
    if !verify_password(&req.password, &user.password_hash)? {
        return Err(ApiError::Unauthorized);
    }

    let token = state.jwt.issue(user.id, user.org_id, &user.role)?;
    Ok(Json(LoginResponse {
        token,
        role: user.role,
        org_id: user.org_id,
        user_id: user.id,
    }))
}

#[derive(Serialize)]
pub struct MeResponse {
    pub id: i64,
    pub email: String,
    pub org_id: i64,
    pub role: String,
}

async fn me(
    State(state): State<AppState>,
    AuthUser(claims): AuthUser,
) -> ApiResult<Json<MeResponse>> {
    let id: i64 = claims.sub.parse().map_err(|_| ApiError::Unauthorized)?;
    let email: Option<(String,)> =
        sqlx::query_as("SELECT email::text FROM users WHERE id = $1 AND org_id = $2")
            .bind(id)
            .bind(claims.org)
            .fetch_optional(&state.db)
            .await?;
    let Some((email,)) = email else {
        // Token is valid but the user has been deleted since it was issued.
        return Err(ApiError::Unauthorized);
    };
    Ok(Json(MeResponse {
        id,
        email,
        org_id: claims.org,
        role: claims.role,
    }))
}
