//! Read-only license status for the UI / support diagnostics.

use axum::routing::get;
use axum::{Json, Router};
use serde_json::{json, Value};

use crate::error::ApiResult;
use crate::middleware::auth::AuthUser;
use crate::security::fingerprint::short_fingerprint;
use crate::security::license::{
    active_license, days_remaining, enforce_enabled, is_licensed, process_fingerprint, LicenseKind,
};
use crate::state::AppState;

pub fn router() -> Router<AppState> {
    Router::new().route("/license", get(license_status))
}

async fn license_status(AuthUser(_claims): AuthUser) -> ApiResult<Json<Value>> {
    let fp = process_fingerprint();
    let short_fp = short_fingerprint(fp);
    let enforce = enforce_enabled();

    match active_license() {
        Some(doc) if is_licensed() => {
            let kind = match doc.kind {
                LicenseKind::Trial => "trial",
                LicenseKind::Commercial => "commercial",
            };
            Ok(Json(json!({
                "licensed": true,
                "enforce": enforce,
                "kind": kind,
                "licensee": doc.licensee,
                "expires_at": doc.expires_at,
                "days_remaining": days_remaining(doc),
                "features": doc.features,
                "max_seats": doc.max_seats,
                "fingerprint": short_fp,
                "activation_hint": activation_hint(enforce, true),
            })))
        }
        Some(doc) => {
            // Loaded but expired / fingerprint mismatch under enforce.
            Ok(Json(json!({
                "licensed": false,
                "enforce": enforce,
                "kind": match doc.kind {
                    LicenseKind::Trial => "trial",
                    LicenseKind::Commercial => "commercial",
                },
                "licensee": doc.licensee,
                "expires_at": doc.expires_at,
                "days_remaining": 0,
                "mode": if enforce { "expired" } else { "development" },
                "fingerprint": short_fp,
                "activation_hint": activation_hint(enforce, false),
            })))
        }
        None => Ok(Json(json!({
            "licensed": false,
            "enforce": enforce,
            "mode": if enforce { "unlicensed" } else { "development" },
            "days_remaining": 0,
            "fingerprint": short_fp,
            "activation_hint": activation_hint(enforce, false),
        }))),
    }
}

fn activation_hint(enforce: bool, licensed: bool) -> String {
    if licensed {
        "To replace a trial with a commercial license, mount a vendor-signed file via \
         LICENSE_FILE (or copy it to LICENSE_STORE_PATH) and restart the API."
            .into()
    } else if enforce {
        "No valid license. Ensure LICENSE_SERVER_URL can issue a trial, or mount a commercial \
         LICENSE_FILE signed with migration-admin license-sign, then restart."
            .into()
    } else {
        "Development mode (LICENSE_ENFORCE unset). For production set LICENSE_ENFORCE=true and \
         either LICENSE_FILE or LICENSE_SERVER_URL for a 10-day trial."
            .into()
    }
}
