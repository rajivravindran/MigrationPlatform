//! License status (read-only) and admin trial activation.

use axum::extract::State;
use axum::routing::get;
use axum::{Json, Router};
use chrono::Utc;
use serde::Deserialize;
use serde_json::{json, Value};

use crate::error::{ApiError, ApiResult};
use crate::middleware::auth::{require_role, AuthUser};
use crate::security::audit::record_audit;
use crate::security::fingerprint::short_fingerprint;
use crate::security::license::{
    self, days_remaining, enforce_enabled, grace_until, heartbeat_due, process_fingerprint,
    snapshot, validate_trial_email, LicenseKind, LicenseProblem, LicenseServer, LicenseState,
};
use crate::state::AppState;

pub fn router() -> Router<AppState> {
    Router::new()
        .route("/license", get(license_status))
        .route("/license/trial", axum::routing::post(start_trial))
}

#[derive(Debug, Deserialize)]
struct StartTrialRequest {
    email: String,
}

/// Admin-only: start (or resume) the phone-home trial for this install without
/// restarting the API. The license server decides reuse/expiry; we only
/// verify the returned signature and persist it.
async fn start_trial(
    State(state): State<AppState>,
    AuthUser(claims): AuthUser,
    Json(req): Json<StartTrialRequest>,
) -> ApiResult<Json<Value>> {
    require_role(&claims, &["admin"])?;
    let email =
        validate_trial_email(&req.email).map_err(|e| ApiError::BadRequest(e.to_string()))?;
    if LicenseServer::from_env().is_none() {
        return Err(ApiError::BadRequest(
            "LICENSE_SERVER_URL and LICENSE_SERVER_TOKEN must be configured on the API to start a trial"
                .into(),
        ));
    }
    if let Some(doc) = snapshot().doc.as_ref() {
        if doc.kind == LicenseKind::Commercial && license::is_licensed() {
            return Err(ApiError::Conflict(
                "a valid commercial license is already active; a trial would not replace it".into(),
            ));
        }
    }

    let doc = license::start_trial(&email)
        .await
        .map_err(|e| ApiError::External(format!("trial activation failed: {e}")))?;

    record_audit(
        &state.db,
        claims.org,
        &claims.sub,
        "license",
        Some(short_fingerprint(process_fingerprint())),
        "trial_start",
        None,
        Some(&json!({
            "licensee": doc.licensee,
            "expires_at": doc.expires_at,
            "kind": "trial",
        })),
    )
    .await?;

    let mut body = status_json(&snapshot());
    body["install_id_full"] = json!(process_fingerprint());
    Ok(Json(body))
}

async fn license_status(AuthUser(claims): AuthUser) -> ApiResult<Json<Value>> {
    let mut body = status_json(&snapshot());
    // Vendors bind commercial licenses to the full install ID. It is a hash of
    // a random UUID (no PII) and grants nothing on its own, but keep it to
    // admins so the short form stays the default in support tickets.
    if claims.role == "admin" {
        body["install_id_full"] = json!(process_fingerprint());
    }
    Ok(Json(body))
}

fn kind_str(kind: LicenseKind) -> &'static str {
    match kind {
        LicenseKind::Trial => "trial",
        LicenseKind::Commercial => "commercial",
    }
}

/// Serialize the process license state for the UI / support diagnostics.
/// Only the short fingerprint leaves the process.
pub fn status_json(state: &LicenseState) -> Value {
    let fp = process_fingerprint();
    let short_fp = short_fingerprint(fp);
    let enforce = enforce_enabled();
    let now = Utc::now();
    let server_configured = LicenseServer::from_env().is_some();
    let problem = state
        .doc
        .as_ref()
        .and_then(|doc| {
            if state.revoked_reason.is_some() {
                Some(LicenseProblem::Revoked)
            } else {
                license::validity(doc, fp, now).err()
            }
        });

    let heartbeat = state.doc.as_ref().map(|doc| {
        let status = if !doc.requires_heartbeat {
            "not_required"
        } else if state.revoked_reason.is_some() {
            "revoked"
        } else if matches!(problem, Some(LicenseProblem::HeartbeatGraceExpired)) {
            "grace_expired"
        } else if state.last_heartbeat_error.is_some() {
            "degraded"
        } else {
            "ok"
        };
        json!({
            "required": doc.requires_heartbeat,
            "status": status,
            "last_attested_at": doc.issued_at,
            "grace_until": grace_until(doc),
            "due": heartbeat_due(state, now),
            "last_attempt_at": state.last_heartbeat_attempt,
            "last_error": state.last_heartbeat_error,
            "server_configured": server_configured,
        })
    });

    match state.doc.as_ref() {
        Some(doc) if problem.is_none() => json!({
            "licensed": true,
            "enforce": enforce,
            "kind": kind_str(doc.kind),
            "licensee": doc.licensee,
            "expires_at": doc.expires_at,
            "days_remaining": days_remaining(doc),
            "features": doc.features,
            "max_seats": doc.max_seats,
            "fingerprint": short_fp,
            "heartbeat": heartbeat,
            "trial_available": server_configured && doc.kind != LicenseKind::Commercial,
            "activation_hint": activation_hint(enforce, Some(doc.kind), None, server_configured),
        }),
        Some(doc) => {
            let mode = match problem {
                Some(LicenseProblem::Revoked) => "revoked",
                Some(LicenseProblem::HeartbeatGraceExpired) => "grace_expired",
                Some(LicenseProblem::FingerprintMismatch) => "fingerprint_mismatch",
                _ => {
                    if enforce {
                        "expired"
                    } else {
                        "development"
                    }
                }
            };
            json!({
                "licensed": false,
                "enforce": enforce,
                "kind": kind_str(doc.kind),
                "licensee": doc.licensee,
                "expires_at": doc.expires_at,
                "days_remaining": 0,
                "mode": mode,
                "problem": problem.map(LicenseProblem::as_str),
                "revoked_reason": state.revoked_reason,
                "fingerprint": short_fp,
                "heartbeat": heartbeat,
                "trial_available": server_configured,
                "activation_hint": activation_hint(enforce, None, problem, server_configured),
            })
        }
        None => json!({
            "licensed": false,
            "enforce": enforce,
            "mode": if enforce { "unlicensed" } else { "development" },
            "days_remaining": 0,
            "fingerprint": short_fp,
            "trial_available": server_configured,
            "activation_hint": activation_hint(enforce, None, None, server_configured),
        }),
    }
}

/// `licensed_kind` is `Some` when a valid license is active.
fn activation_hint(
    enforce: bool,
    licensed_kind: Option<LicenseKind>,
    problem: Option<LicenseProblem>,
    server_configured: bool,
) -> String {
    match licensed_kind {
        Some(LicenseKind::Commercial) => {
            return "Commercial license active. To renew or re-issue, mount the new vendor-signed \
                    file via LICENSE_FILE (or copy it to LICENSE_STORE_PATH); the API adopts a \
                    changed store file within a minute."
                .into();
        }
        Some(LicenseKind::Trial) => {
            return "To replace a trial with a commercial license, mount a vendor-signed file via \
                    LICENSE_FILE (or copy it to LICENSE_STORE_PATH); the API adopts a changed store \
                    file within a minute."
                .into();
        }
        None => {}
    }
    match problem {
        Some(LicenseProblem::HeartbeatGraceExpired) => {
            return "The license server has been unreachable for more than 72 hours. Restore \
                    connectivity to LICENSE_SERVER_URL (the next heartbeat re-attests \
                    automatically) or mount a commercial LICENSE_FILE."
                .into()
        }
        Some(LicenseProblem::Revoked) => {
            return "The vendor revoked this license. Contact support, then mount a new \
                    LICENSE_FILE."
                .into()
        }
        Some(LicenseProblem::FingerprintMismatch) => {
            return "This license is bound to a different install ID. Request a license for the \
                    install ID shown above."
                .into()
        }
        _ => {}
    }
    if enforce {
        if server_configured {
            "No valid license. An admin can start a 10-day trial below with a contact email, \
             or mount a commercial LICENSE_FILE signed with migration-admin license-sign."
                .into()
        } else {
            "No valid license. Configure LICENSE_SERVER_URL + LICENSE_SERVER_TOKEN to start a \
             trial, or mount a commercial LICENSE_FILE signed with migration-admin license-sign."
                .into()
        }
    } else {
        "Development mode (LICENSE_ENFORCE unset). For production set LICENSE_ENFORCE=true and \
         either LICENSE_FILE or LICENSE_SERVER_URL for a 10-day trial."
            .into()
    }
}
