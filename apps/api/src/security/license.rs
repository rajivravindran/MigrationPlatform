//! Signed license file enforcement for self-hosted deployments.
//!
//! A license is a JSON file: `{ "license": {...doc...}, "signature": "b64" }`.
//! The signature is RSA PKCS#1 v1.5 / SHA-256 over the canonical (sorted-key)
//! serialization of the `license` value, produced by `migration-admin
//! license-sign` (or the phone-home license service) with the vendor's private
//! key. The public verification key is embedded in the binary at build time
//! (override with `LICENSE_PUBLIC_KEY_PATH` only for key rotation).
//!
//! Startup policy:
//! - Valid `LICENSE_FILE` or durable store → load and verify.
//! - `LICENSE_ENFORCE=true` with no valid local license and `LICENSE_TRIAL_EMAIL`
//!   set → phone home to `LICENSE_SERVER_URL` to start/reuse a 10-day trial
//!   keyed by install fingerprint; persist to `LICENSE_STORE_PATH`.
//! - Without enforce and no license → development mode (loud warning).
//! - Under enforce, missing/expired license does **not** abort boot; mutating
//!   APIs are blocked at runtime via [`require_licensed`].
//!
//! Heartbeat policy (P5.1):
//! - Licenses with `requires_heartbeat: true` (all server-issued trials, and
//!   commercial licenses signed with `--require-heartbeat`) must be
//!   re-attested by the license server every [`HEARTBEAT_INTERVAL`].
//! - A successful heartbeat returns the same license re-signed with a fresh
//!   `issued_at`. Offline grace is [`HEARTBEAT_GRACE`] measured from that
//!   **signed** `issued_at`, so the grace baseline cannot be edited locally.
//! - `403 revoked` from the server denies work immediately; network failures
//!   never deny inside the grace window.
//! - Licenses without the flag (legacy / air-gapped commercial) are unaffected.

use std::fs;
use std::path::{Path, PathBuf};
use std::sync::{OnceLock, RwLock};
use std::time::SystemTime;

use anyhow::{bail, Context, Result};
use base64::{engine::general_purpose::STANDARD, Engine};
use chrono::{DateTime, Duration, Utc};
use rsa::pkcs1v15::{Signature, SigningKey, VerifyingKey};
use rsa::pkcs8::{DecodePrivateKey, DecodePublicKey};
use rsa::sha2::Sha256;
use rsa::signature::{SignatureEncoding, Signer, Verifier};
use rsa::{RsaPrivateKey, RsaPublicKey};
use serde::{Deserialize, Serialize};
use serde_json::Value;

use super::fingerprint::{install_fingerprint, short_fingerprint};

/// Vendor verification key baked into the binary. Replace this file before
/// cutting customer builds; the paired private key must live offline.
const EMBEDDED_PUBLIC_KEY_PEM: &str =
    include_str!("../../../../infra/license/license_public_key.pem");

const DEFAULT_STORE_PATH: &str = "/var/lib/migration/license.json";
const PRODUCT_NAME: &str = "migration-platform";
const PRODUCT_VERSION: &str = env!("CARGO_PKG_VERSION");

/// How often a heartbeat-required license is re-attested with the server.
pub const HEARTBEAT_INTERVAL: Duration = Duration::hours(24);
/// How long a heartbeat-required license stays valid without a successful
/// heartbeat, measured from the signed `issued_at`.
pub const HEARTBEAT_GRACE: Duration = Duration::hours(72);
/// Retry cadence after a failed heartbeat attempt.
pub const HEARTBEAT_RETRY: Duration = Duration::hours(1);
/// Background tick: store-file reload check + heartbeat due check.
pub const HEARTBEAT_TICK_SECS: u64 = 60;

#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize, Default)]
#[serde(rename_all = "snake_case")]
pub enum LicenseKind {
    Trial,
    #[default]
    Commercial,
}

#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
pub struct LicenseDoc {
    pub licensee: String,
    pub expires_at: DateTime<Utc>,
    #[serde(default)]
    pub features: Vec<String>,
    #[serde(default)]
    pub max_seats: Option<u32>,
    #[serde(default)]
    pub issued_at: Option<DateTime<Utc>>,
    #[serde(default)]
    pub kind: LicenseKind,
    /// Install fingerprint this trial/commercial seat is bound to (hashed).
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub fingerprint: Option<String>,
    /// When true the license must be re-attested by the license server every
    /// [`HEARTBEAT_INTERVAL`]; validity lapses [`HEARTBEAT_GRACE`] after the
    /// signed `issued_at` without one. Absent on legacy/air-gapped licenses.
    #[serde(default, skip_serializing_if = "std::ops::Not::not")]
    pub requires_heartbeat: bool,
}

#[derive(Debug, Serialize, Deserialize)]
pub struct LicenseFile {
    pub license: Value,
    pub signature: String,
}

/// Why a loaded license is not currently usable.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize)]
#[serde(rename_all = "snake_case")]
pub enum LicenseProblem {
    Expired,
    FingerprintMismatch,
    /// `requires_heartbeat` and no successful heartbeat within the grace window.
    HeartbeatGraceExpired,
    /// `requires_heartbeat` but the signed doc carries no `issued_at`.
    MissingIssuedAt,
    /// The license server explicitly revoked or refused this license.
    Revoked,
}

impl LicenseProblem {
    pub fn as_str(self) -> &'static str {
        match self {
            LicenseProblem::Expired => "expired",
            LicenseProblem::FingerprintMismatch => "fingerprint_mismatch",
            LicenseProblem::HeartbeatGraceExpired => "heartbeat_grace_expired",
            LicenseProblem::MissingIssuedAt => "missing_issued_at",
            LicenseProblem::Revoked => "revoked",
        }
    }
}

/// Snapshot of the process-wide license state, safe to read from handlers.
#[derive(Debug, Clone, Default)]
pub struct LicenseState {
    /// Verified license, if any was loaded/activated.
    pub doc: Option<LicenseDoc>,
    /// Raw signed file for `doc`; sent back to the server on heartbeat.
    pub raw: Option<String>,
    /// Set when the server answered a heartbeat with an authoritative denial.
    pub revoked_reason: Option<String>,
    pub last_heartbeat_attempt: Option<DateTime<Utc>>,
    pub last_heartbeat_error: Option<String>,
    /// Store-file identity (mtime, len) observed at the last load; used to
    /// notice when another replica or an operator refreshed the file.
    store_stamp: Option<(SystemTime, u64)>,
}

static STATE: RwLock<LicenseState> = RwLock::new(LicenseState {
    doc: None,
    raw: None,
    revoked_reason: None,
    last_heartbeat_attempt: None,
    last_heartbeat_error: None,
    store_stamp: None,
});
static ENFORCE: OnceLock<bool> = OnceLock::new();
static FINGERPRINT: OnceLock<String> = OnceLock::new();

/// Whether `LICENSE_ENFORCE=true` for this process.
pub fn enforce_enabled() -> bool {
    *ENFORCE.get().unwrap_or(&false)
}

/// Cloned snapshot of the current license state.
pub fn snapshot() -> LicenseState {
    STATE.read().unwrap_or_else(|p| p.into_inner()).clone()
}

fn with_state<R>(f: impl FnOnce(&mut LicenseState) -> R) -> R {
    let mut guard = STATE.write().unwrap_or_else(|p| p.into_inner());
    f(&mut guard)
}

/// The validated license for this process, if one was loaded.
pub fn active_license() -> Option<LicenseDoc> {
    snapshot().doc
}

/// Full install fingerprint (hex). Computed once at startup.
pub fn process_fingerprint() -> &'static str {
    FINGERPRINT
        .get()
        .map(|s| s.as_str())
        .unwrap_or("unavailable")
}

/// Current usability of the active license.
pub fn current_problem() -> Option<LicenseProblem> {
    let state = snapshot();
    problem_for(&state, process_fingerprint(), Utc::now())
}

/// True when a non-expired license is active (fingerprint matches when set,
/// heartbeat grace not exhausted, not revoked).
pub fn is_licensed() -> bool {
    let state = snapshot();
    state.doc.is_some() && problem_for(&state, process_fingerprint(), Utc::now()).is_none()
}

fn problem_for(state: &LicenseState, local_fp: &str, now: DateTime<Utc>) -> Option<LicenseProblem> {
    if state.revoked_reason.is_some() {
        return Some(LicenseProblem::Revoked);
    }
    state.doc.as_ref().and_then(|doc| validity(doc, local_fp, now).err())
}

/// Pure validity check for a verified document at instant `now`.
/// `local_fp` empty ⇒ fingerprint binding is not checked (unavailable).
pub fn validity(doc: &LicenseDoc, local_fp: &str, now: DateTime<Utc>) -> Result<(), LicenseProblem> {
    if doc.expires_at <= now {
        return Err(LicenseProblem::Expired);
    }
    if let Some(bound) = doc.fingerprint.as_deref() {
        if !local_fp.is_empty() && bound != local_fp {
            return Err(LicenseProblem::FingerprintMismatch);
        }
    }
    if doc.requires_heartbeat {
        let Some(issued) = doc.issued_at else {
            return Err(LicenseProblem::MissingIssuedAt);
        };
        if now - issued > HEARTBEAT_GRACE {
            return Err(LicenseProblem::HeartbeatGraceExpired);
        }
    }
    Ok(())
}

/// Instant at which offline grace lapses (only for heartbeat licenses).
pub fn grace_until(doc: &LicenseDoc) -> Option<DateTime<Utc>> {
    if !doc.requires_heartbeat {
        return None;
    }
    doc.issued_at.map(|t| t + HEARTBEAT_GRACE)
}

/// Whether a heartbeat should be attempted now.
pub fn heartbeat_due(state: &LicenseState, now: DateTime<Utc>) -> bool {
    let Some(doc) = state.doc.as_ref() else {
        return false;
    };
    if !doc.requires_heartbeat || state.revoked_reason.is_some() {
        return false;
    }
    let issued = match doc.issued_at {
        Some(t) => t,
        None => return true,
    };
    let interval = heartbeat_interval();
    if now - issued >= interval {
        // Back off retries after a failure so a dead server is not hammered.
        return match state.last_heartbeat_attempt {
            Some(last) => now - last >= HEARTBEAT_RETRY,
            None => true,
        };
    }
    false
}

fn heartbeat_interval() -> Duration {
    std::env::var("LICENSE_HEARTBEAT_INTERVAL_SECS")
        .ok()
        .and_then(|v| v.parse::<i64>().ok())
        .filter(|s| *s > 0)
        .map(Duration::seconds)
        .unwrap_or(HEARTBEAT_INTERVAL)
}

/// Days remaining until expiry (0 if expired / unlicensed). Ceiling of whole days.
pub fn days_remaining(doc: &LicenseDoc) -> i64 {
    let secs = (doc.expires_at - Utc::now()).num_seconds();
    if secs <= 0 {
        0
    } else {
        (secs + 86_399) / 86_400
    }
}

/// Minimal syntactic e-mail check shared by the API and the license server.
/// Deliberately conservative: it gates lead capture, not deliverability.
pub fn validate_trial_email(raw: &str) -> Result<String> {
    let email = raw.trim().to_ascii_lowercase();
    if email.len() < 6 || email.len() > 254 {
        bail!("email must be between 6 and 254 characters");
    }
    if email.chars().any(|c| c.is_whitespace() || c.is_control()) {
        bail!("email must not contain whitespace");
    }
    let Some((local, domain)) = email.rsplit_once('@') else {
        bail!("email must contain '@'");
    };
    if local.is_empty() || domain.is_empty() || local.contains('@') {
        bail!("email has an empty local part or domain");
    }
    let labels: Vec<&str> = domain.split('.').collect();
    if labels.len() < 2
        || labels
            .iter()
            .any(|l| l.is_empty() || !l.chars().all(|c| c.is_ascii_alphanumeric() || c == '-'))
    {
        bail!("email domain must be a dotted hostname");
    }
    Ok(email)
}

/// Validate / activate the deployment license. Call once from `main` before serving.
pub async fn enforce_at_startup() -> Result<()> {
    let enforce = std::env::var("LICENSE_ENFORCE").ok().as_deref() == Some("true");
    let _ = ENFORCE.set(enforce);

    let fp = install_fingerprint().context(
        "loading durable installation ID; mount LICENSE_INSTALLATION_ID_PATH on shared durable storage",
    )?;
    tracing::info!(fingerprint = %short_fingerprint(&fp), "install fingerprint ready");
    let _ = FINGERPRINT.set(fp.clone());

    if let Some(loaded) = try_load_local(&fp)? {
        activate(loaded);
        return Ok(());
    }

    if enforce {
        match std::env::var("LICENSE_TRIAL_EMAIL")
            .ok()
            .filter(|e| !e.trim().is_empty())
        {
            Some(email) => match activate_trial_phone_home(&fp, &email).await {
                Ok(loaded) => {
                    activate(loaded);
                    return Ok(());
                }
                Err(err) => {
                    tracing::error!(
                        error = %err,
                        "LICENSE_ENFORCE=true but no valid license and trial activation failed; \
                         mutating APIs will be blocked until a license is available"
                    );
                }
            },
            None => {
                tracing::warn!(
                    "LICENSE_ENFORCE=true with no local license and no LICENSE_TRIAL_EMAIL; \
                     start a trial from Settings → License (admin) or mount a commercial LICENSE_FILE. \
                     Mutating APIs are blocked until then."
                );
            }
        }
        return Ok(());
    }

    tracing::warn!(
        "no LICENSE_FILE configured: running in UNLICENSED development mode; \
         set LICENSE_FILE (and LICENSE_ENFORCE=true) for production deployments"
    );
    Ok(())
}

/// A verified license together with its raw signed file and store identity.
#[derive(Debug, Clone)]
pub struct LoadedLicense {
    pub doc: LicenseDoc,
    pub raw: String,
    store_stamp: Option<(SystemTime, u64)>,
}

fn activate(loaded: LoadedLicense) {
    metrics::counter!("license_activation_total", "kind" => format!("{:?}", loaded.doc.kind).to_lowercase())
        .increment(1);
    tracing::info!(
        licensee = %loaded.doc.licensee,
        kind = ?loaded.doc.kind,
        expires_at = %loaded.doc.expires_at,
        features = ?loaded.doc.features,
        fingerprint = loaded.doc.fingerprint.as_deref().map(short_fingerprint),
        requires_heartbeat = loaded.doc.requires_heartbeat,
        "license validated"
    );
    with_state(|s| {
        s.doc = Some(loaded.doc);
        s.raw = Some(loaded.raw);
        s.revoked_reason = None;
        s.last_heartbeat_error = None;
        s.store_stamp = loaded.store_stamp;
    });
    publish_gauges();
}

fn publish_gauges() {
    let state = snapshot();
    let licensed = state.doc.is_some() && problem_for(&state, process_fingerprint(), Utc::now()).is_none();
    metrics::gauge!("license_valid").set(if licensed { 1.0 } else { 0.0 });
    let grace_secs = state
        .doc
        .as_ref()
        .and_then(grace_until)
        .map(|t| (t - Utc::now()).num_seconds().max(0) as f64)
        .unwrap_or(0.0);
    metrics::gauge!("license_heartbeat_grace_seconds_remaining").set(grace_secs);
}

fn try_load_local(fp: &str) -> Result<Option<LoadedLicense>> {
    let candidates = [
        std::env::var("LICENSE_FILE").ok().filter(|p| !p.is_empty()),
        Some(store_path().display().to_string()).filter(|_| store_path().is_file()),
    ];

    let key = load_public_key()?;
    for path in candidates.into_iter().flatten() {
        match load_and_verify_path(&path, &key, fp) {
            Ok(loaded) => return Ok(Some(loaded)),
            Err(err) => {
                tracing::warn!(path = %path, error = %err, "license candidate rejected");
            }
        }
    }
    Ok(None)
}

fn load_and_verify_path(path: &str, key: &RsaPublicKey, fp: &str) -> Result<LoadedLicense> {
    let raw = fs::read_to_string(path).with_context(|| format!("reading license file {path}"))?;
    let doc = verify_license(&raw, key)?;
    if let Some(bound) = doc.fingerprint.as_deref() {
        if bound != fp {
            bail!(
                "license fingerprint mismatch (license bound to {}, this install is {})",
                short_fingerprint(bound),
                short_fingerprint(fp)
            );
        }
    }
    let store_stamp = if Path::new(path) == store_path() {
        file_stamp(&store_path())
    } else {
        None
    };
    Ok(LoadedLicense {
        doc,
        raw,
        store_stamp,
    })
}

fn file_stamp(path: &Path) -> Option<(SystemTime, u64)> {
    let meta = fs::metadata(path).ok()?;
    Some((meta.modified().ok()?, meta.len()))
}

fn store_path() -> PathBuf {
    std::env::var("LICENSE_STORE_PATH")
        .ok()
        .filter(|p| !p.is_empty())
        .map(PathBuf::from)
        .unwrap_or_else(|| PathBuf::from(DEFAULT_STORE_PATH))
}

/// Connection details for the vendor license service.
#[derive(Debug, Clone)]
pub struct LicenseServer {
    pub base_url: String,
    pub token: String,
}

impl LicenseServer {
    /// Read `LICENSE_SERVER_URL` / `LICENSE_SERVER_TOKEN`; `None` when not configured.
    pub fn from_env() -> Option<Self> {
        let base_url = std::env::var("LICENSE_SERVER_URL")
            .ok()
            .filter(|u| !u.trim().is_empty())?;
        let token = std::env::var("LICENSE_SERVER_TOKEN")
            .ok()
            .filter(|t| !t.trim().is_empty())?;
        Some(Self {
            base_url: base_url.trim().trim_end_matches('/').to_string(),
            token,
        })
    }

    fn http() -> Result<reqwest::Client> {
        reqwest::Client::builder()
            .timeout(std::time::Duration::from_secs(15))
            .build()
            .context("building license HTTP client")
    }
}

/// Start (or resume) a trial for this install and make it the active license.
/// Used at boot (`LICENSE_TRIAL_EMAIL`) and by the admin `POST /license/trial`.
pub async fn start_trial(email: &str) -> Result<LicenseDoc> {
    let fp = process_fingerprint();
    if fp == "unavailable" {
        bail!("install fingerprint unavailable; license subsystem not initialised");
    }
    let loaded = activate_trial_phone_home(fp, email).await?;
    let doc = loaded.doc.clone();
    activate(loaded);
    Ok(doc)
}

async fn activate_trial_phone_home(fp: &str, email: &str) -> Result<LoadedLicense> {
    let email = validate_trial_email(email)?;
    let server = LicenseServer::from_env().context(
        "LICENSE_SERVER_URL and LICENSE_SERVER_TOKEN are required to start a trial",
    )?;

    let url = format!("{}/v1/trial/start", server.base_url);
    let body = serde_json::json!({
        "fingerprint": fp,
        "email": email,
        "product": PRODUCT_NAME,
        "version": PRODUCT_VERSION,
    });

    tracing::info!(%url, fingerprint = %short_fingerprint(fp), "requesting trial license");

    let resp = LicenseServer::http()?
        .post(&url)
        .bearer_auth(&server.token)
        .json(&body)
        .send()
        .await
        .with_context(|| format!("calling license server {url}"))?;

    let status = resp.status();
    let raw = resp
        .text()
        .await
        .context("reading license server response")?;
    if !status.is_success() {
        bail!("license server returned {status}: {raw}");
    }

    let raw_file = unwrap_license_response(&raw)?;
    let key = load_public_key()?;
    let doc = verify_license(&raw_file, &key)?;
    if doc.kind != LicenseKind::Trial {
        bail!("license server returned a non-trial license");
    }
    if doc.fingerprint.as_deref() != Some(fp) {
        bail!("trial license fingerprint does not match this install");
    }

    let store_stamp = persist_license(&raw_file)?;
    Ok(LoadedLicense {
        doc,
        raw: raw_file,
        store_stamp,
    })
}

/// Result of one heartbeat exchange with the license server.
#[derive(Debug)]
pub enum HeartbeatOutcome {
    /// Server re-attested the license; carries the refreshed signed document.
    Refreshed(LoadedLicense),
    /// Server authoritatively refused (revoked / expired). Deny immediately.
    Denied { status: u16, reason: String },
    /// Transport / server-side failure. Keep the current license inside grace.
    Unavailable(String),
}

/// Perform a single heartbeat for `raw_license` bound to `fp`. Pure with
/// respect to process state so it can be exercised against a mock server.
pub async fn heartbeat_once(
    server: &LicenseServer,
    fp: &str,
    raw_license: &str,
    key: &RsaPublicKey,
) -> HeartbeatOutcome {
    let url = format!("{}/v1/heartbeat", server.base_url);
    let license_file: Value = match serde_json::from_str(raw_license) {
        Ok(v) => v,
        Err(e) => return HeartbeatOutcome::Unavailable(format!("stored license unreadable: {e}")),
    };
    let body = serde_json::json!({
        "fingerprint": fp,
        "license_file": license_file,
        "product": PRODUCT_NAME,
        "version": PRODUCT_VERSION,
    });

    let client = match LicenseServer::http() {
        Ok(c) => c,
        Err(e) => return HeartbeatOutcome::Unavailable(e.to_string()),
    };
    let resp = match client
        .post(&url)
        .bearer_auth(&server.token)
        .json(&body)
        .send()
        .await
    {
        Ok(r) => r,
        Err(e) => return HeartbeatOutcome::Unavailable(format!("calling {url}: {e}")),
    };
    let status = resp.status();
    let text = match resp.text().await {
        Ok(t) => t,
        Err(e) => return HeartbeatOutcome::Unavailable(format!("reading response: {e}")),
    };

    match status.as_u16() {
        200 => {
            let raw_file = match unwrap_license_response(&text) {
                Ok(r) => r,
                Err(e) => return HeartbeatOutcome::Unavailable(e.to_string()),
            };
            match verify_license(&raw_file, key) {
                Ok(doc) if doc.fingerprint.as_deref() == Some(fp) => {
                    HeartbeatOutcome::Refreshed(LoadedLicense {
                        doc,
                        raw: raw_file,
                        store_stamp: None,
                    })
                }
                Ok(_) => HeartbeatOutcome::Unavailable(
                    "heartbeat response is bound to a different install".into(),
                ),
                Err(e) => HeartbeatOutcome::Unavailable(format!("heartbeat response rejected: {e}")),
            }
        }
        // Only explicit business refusals are authoritative. 401/5xx and
        // transport errors are treated as "unavailable" so a misconfigured
        // token or a vendor outage cannot revoke a customer inside grace.
        402 | 403 => HeartbeatOutcome::Denied {
            status: status.as_u16(),
            reason: extract_error_message(&text),
        },
        _ => HeartbeatOutcome::Unavailable(format!("license server returned {status}: {text}")),
    }
}

fn extract_error_message(text: &str) -> String {
    serde_json::from_str::<Value>(text)
        .ok()
        .and_then(|v| {
            v.get("error")
                .and_then(|e| e.get("message").or(Some(e)))
                .and_then(|m| m.as_str().map(str::to_string))
        })
        .unwrap_or_else(|| text.trim().chars().take(200).collect())
}

/// One background tick: adopt a refreshed store file if another replica or an
/// operator replaced it, then heartbeat if due. Never panics; never denies on
/// transport failure.
pub async fn heartbeat_tick() {
    reload_store_if_changed();

    let state = snapshot();
    let now = Utc::now();
    if !heartbeat_due(&state, now) {
        publish_gauges();
        return;
    }
    let (Some(raw), Some(doc)) = (state.raw.clone(), state.doc.clone()) else {
        return;
    };
    with_state(|s| s.last_heartbeat_attempt = Some(now));

    let Some(server) = LicenseServer::from_env() else {
        let msg = "license requires heartbeat but LICENSE_SERVER_URL/LICENSE_SERVER_TOKEN are unset";
        tracing::warn!(msg);
        with_state(|s| s.last_heartbeat_error = Some(msg.into()));
        metrics::counter!("license_heartbeat_total", "result" => "unconfigured").increment(1);
        publish_gauges();
        return;
    };
    let key = match load_public_key() {
        Ok(k) => k,
        Err(e) => {
            tracing::error!(error = %e, "license public key unavailable for heartbeat");
            return;
        }
    };
    let fp = process_fingerprint();

    match heartbeat_once(&server, fp, &raw, &key).await {
        HeartbeatOutcome::Refreshed(loaded) => {
            let stamp = match persist_license(&loaded.raw) {
                Ok(s) => s,
                Err(e) => {
                    tracing::warn!(error = %e, "heartbeat succeeded but persisting refreshed license failed");
                    None
                }
            };
            tracing::info!(
                licensee = %loaded.doc.licensee,
                issued_at = ?loaded.doc.issued_at,
                expires_at = %loaded.doc.expires_at,
                "license heartbeat ok"
            );
            metrics::counter!("license_heartbeat_total", "result" => "ok").increment(1);
            with_state(|s| {
                s.doc = Some(loaded.doc);
                s.raw = Some(loaded.raw);
                s.revoked_reason = None;
                s.last_heartbeat_error = None;
                s.store_stamp = stamp.or(s.store_stamp);
            });
        }
        HeartbeatOutcome::Denied { status, reason } => {
            tracing::error!(status, reason = %reason, licensee = %doc.licensee, "license server denied heartbeat; work-producing APIs are now blocked");
            metrics::counter!("license_heartbeat_total", "result" => "denied").increment(1);
            with_state(|s| {
                s.revoked_reason = Some(format!("{status}: {reason}"));
                s.last_heartbeat_error = Some(reason);
            });
        }
        HeartbeatOutcome::Unavailable(err) => {
            let grace = grace_until(&doc);
            tracing::warn!(error = %err, grace_until = ?grace, "license heartbeat failed; continuing inside offline grace");
            metrics::counter!("license_heartbeat_total", "result" => "unavailable").increment(1);
            with_state(|s| s.last_heartbeat_error = Some(err));
        }
    }
    publish_gauges();
}

/// If the durable store file changed since we last loaded it (another replica
/// heartbeated, or an operator dropped in a commercial license), verify and
/// adopt it. A rejected file is logged and ignored.
fn reload_store_if_changed() {
    let path = store_path();
    let Some(stamp) = file_stamp(&path) else {
        return;
    };
    let current = snapshot();
    if current.store_stamp == Some(stamp) {
        return;
    }
    let fp = process_fingerprint();
    let key = match load_public_key() {
        Ok(k) => k,
        Err(_) => return,
    };
    match load_and_verify_path(&path.display().to_string(), &key, fp) {
        Ok(loaded) => {
            if should_adopt(&current, &loaded.doc, fp, Utc::now()) {
                tracing::info!(path = %path.display(), licensee = %loaded.doc.licensee, kind = ?loaded.doc.kind, "adopted refreshed license from store");
                activate(loaded);
            } else {
                with_state(|s| s.store_stamp = Some(stamp));
            }
        }
        Err(err) => {
            tracing::warn!(path = %path.display(), error = %err, "changed license store rejected; keeping current license");
            with_state(|s| s.store_stamp = Some(stamp));
        }
    }
}

/// Precedence rule for adopting a verified store document over the active one.
/// Never downgrade: a commercial license is not replaced by a trial, and a
/// document is only replaced by a strictly newer attestation of the same kind.
/// A currently unusable license (expired, revoked, out of grace) is always
/// replaced by a usable candidate.
fn should_adopt(
    current: &LicenseState,
    candidate: &LicenseDoc,
    local_fp: &str,
    now: DateTime<Utc>,
) -> bool {
    let Some(cur) = current.doc.as_ref() else {
        return true;
    };
    if cur == candidate {
        return false;
    }
    let cur_ok = problem_for(current, local_fp, now).is_none();
    let cand_ok = validity(candidate, local_fp, now).is_ok();
    if !cand_ok {
        return false;
    }
    if !cur_ok {
        return true;
    }
    match (cur.kind, candidate.kind) {
        (LicenseKind::Trial, LicenseKind::Commercial) => true,
        (LicenseKind::Commercial, LicenseKind::Trial) => false,
        _ => {
            let epoch = DateTime::<Utc>::UNIX_EPOCH;
            candidate.issued_at.unwrap_or(epoch) > cur.issued_at.unwrap_or(epoch)
        }
    }
}

fn unwrap_license_response(raw: &str) -> Result<String> {
    if let Ok(v) = serde_json::from_str::<Value>(raw) {
        if v.get("license").is_some() && v.get("signature").is_some() {
            return Ok(raw.to_string());
        }
        if let Some(inner) = v.get("license_file") {
            return Ok(serde_json::to_string(inner)?);
        }
        if let Some(s) = v.get("license_json").and_then(|x| x.as_str()) {
            return Ok(s.to_string());
        }
    }
    // Treat body as the license file itself.
    Ok(raw.to_string())
}

fn persist_license(raw: &str) -> Result<Option<(SystemTime, u64)>> {
    let path = store_path();
    if let Some(parent) = path.parent() {
        fs::create_dir_all(parent)
            .with_context(|| format!("creating license store dir {}", parent.display()))?;
    }
    // Atomic-ish write: temp + rename when on same filesystem.
    let tmp = path.with_extension(format!("json.{}.tmp", std::process::id()));
    fs::write(&tmp, raw).with_context(|| format!("writing {}", tmp.display()))?;
    fs::rename(&tmp, &path).with_context(|| format!("persisting license to {}", path.display()))?;
    tracing::info!(path = %path.display(), "persisted license");
    Ok(file_stamp(&path))
}

pub fn load_public_key() -> Result<RsaPublicKey> {
    let pem = match std::env::var("LICENSE_PUBLIC_KEY_PATH")
        .ok()
        .filter(|p| !p.is_empty())
    {
        Some(p) => fs::read_to_string(&p).with_context(|| format!("reading {p}"))?,
        None => EMBEDDED_PUBLIC_KEY_PEM.to_string(),
    };
    RsaPublicKey::from_public_key_pem(&pem).context("parsing license public key")
}

/// Canonical bytes that get signed: serde_json's default `Map` is a BTreeMap,
/// so a parse -> serialize round-trip yields sorted keys deterministically.
fn canonical_bytes(license: &Value) -> Result<Vec<u8>> {
    serde_json::to_vec(license).context("serializing license payload")
}

pub fn verify_license(raw: &str, key: &RsaPublicKey) -> Result<LicenseDoc> {
    let file: LicenseFile = serde_json::from_str(raw).context("license file is not valid JSON")?;
    let sig_bytes = STANDARD
        .decode(&file.signature)
        .context("license signature is not valid base64")?;
    let signature = Signature::try_from(sig_bytes.as_slice()).context("malformed signature")?;
    let verifying_key = VerifyingKey::<Sha256>::new(key.clone());
    verifying_key
        .verify(&canonical_bytes(&file.license)?, &signature)
        .map_err(|_| {
            anyhow::anyhow!("license signature verification FAILED (tampered or wrong key)")
        })?;

    let doc: LicenseDoc =
        serde_json::from_value(file.license).context("license payload has an invalid shape")?;
    if doc.expires_at <= Utc::now() {
        bail!(
            "license for {:?} expired at {}",
            doc.licensee,
            doc.expires_at
        );
    }
    Ok(doc)
}

/// Sign a license document with the vendor private key (admin CLI / license server).
pub fn sign_license(doc: &LicenseDoc, private_key_pem: &str) -> Result<String> {
    let key = RsaPrivateKey::from_pkcs8_pem(private_key_pem).context("parsing private key")?;
    let license = serde_json::to_value(doc)?;
    let signing_key = SigningKey::<Sha256>::new(key);
    let signature = signing_key.sign(&canonical_bytes(&license)?);
    let file = LicenseFile {
        license,
        signature: STANDARD.encode(signature.to_bytes()),
    };
    Ok(serde_json::to_string_pretty(&file)?)
}

/// Runtime gate for mutating APIs when enforcement is on.
pub fn require_licensed() -> Result<(), crate::error::ApiError> {
    if !enforce_enabled() {
        return Ok(());
    }
    let state = snapshot();
    let problem = problem_for(&state, process_fingerprint(), Utc::now());
    if state.doc.is_some() && problem.is_none() {
        return Ok(());
    }
    let reason = match problem {
        Some(p) => p.as_str(),
        None => "missing",
    };
    metrics::counter!("license_denied_total", "reason" => reason).increment(1);
    if matches!(problem, Some(LicenseProblem::Expired)) {
        metrics::counter!("license_expired_denied_total").increment(1);
    }
    tracing::warn!(reason, "work-producing operation denied by runtime license gate");
    let hint = match problem {
        Some(LicenseProblem::HeartbeatGraceExpired) => {
            "The license could not be re-attested with the license server for more than 72 hours. \
             Restore connectivity to LICENSE_SERVER_URL or mount a commercial LICENSE_FILE."
        }
        Some(LicenseProblem::Revoked) => {
            "The license server revoked this license. Contact the vendor or mount a new LICENSE_FILE."
        }
        Some(LicenseProblem::Expired) => {
            "The license has expired. Mount a renewed commercial LICENSE_FILE."
        }
        _ => {
            "A valid license is required. Start a trial from Settings → License or mount a commercial \
             LICENSE_FILE."
        }
    };
    Err(crate::error::ApiError::LicenseRequired(format!(
        "{hint} (reason: {reason})"
    )))
}

/// Path helpers used by tests / install docs.
pub fn default_store_path() -> &'static Path {
    Path::new(DEFAULT_STORE_PATH)
}

#[cfg(test)]
mod tests {
    use super::*;

    fn keypair() -> (String, RsaPublicKey) {
        let mut rng = rand::thread_rng();
        let priv_key = RsaPrivateKey::new(&mut rng, 2048).unwrap();
        let pub_key = RsaPublicKey::from(&priv_key);
        let pem = rsa::pkcs8::EncodePrivateKey::to_pkcs8_pem(&priv_key, rsa::pkcs8::LineEnding::LF)
            .unwrap()
            .to_string();
        (pem, pub_key)
    }

    fn doc(expires_in: Duration) -> LicenseDoc {
        LicenseDoc {
            licensee: "Acme Corp".into(),
            expires_at: Utc::now() + expires_in,
            features: vec!["llm".into()],
            max_seats: Some(25),
            issued_at: Some(Utc::now()),
            kind: LicenseKind::Commercial,
            fingerprint: None,
            requires_heartbeat: false,
        }
    }

    #[test]
    fn sign_verify_roundtrip() {
        let (priv_pem, pub_key) = keypair();
        let raw = sign_license(&doc(Duration::days(365)), &priv_pem).unwrap();
        let verified = verify_license(&raw, &pub_key).unwrap();
        assert_eq!(verified.licensee, "Acme Corp");
        assert_eq!(verified.max_seats, Some(25));
        assert_eq!(verified.kind, LicenseKind::Commercial);
        assert!(!verified.requires_heartbeat);
    }

    #[test]
    fn trial_with_fingerprint_roundtrip() {
        let (priv_pem, pub_key) = keypair();
        let mut d = doc(Duration::days(10));
        d.kind = LicenseKind::Trial;
        d.fingerprint = Some("abc123".into());
        d.licensee = "trial".into();
        d.requires_heartbeat = true;
        let raw = sign_license(&d, &priv_pem).unwrap();
        assert!(raw.contains("requires_heartbeat"));
        let verified = verify_license(&raw, &pub_key).unwrap();
        assert_eq!(verified.kind, LicenseKind::Trial);
        assert_eq!(verified.fingerprint.as_deref(), Some("abc123"));
        assert!(verified.requires_heartbeat);
    }

    #[test]
    fn heartbeat_flag_omitted_when_false() {
        let (priv_pem, _) = keypair();
        let raw = sign_license(&doc(Duration::days(1)), &priv_pem).unwrap();
        assert!(!raw.contains("requires_heartbeat"));
    }

    #[test]
    fn legacy_license_defaults_commercial() {
        let (priv_pem, pub_key) = keypair();
        // Sign via Value without kind field to simulate older licenses.
        let license = serde_json::json!({
            "licensee": "Legacy",
            "expires_at": (Utc::now() + Duration::days(30)).to_rfc3339(),
        });
        let key = RsaPrivateKey::from_pkcs8_pem(&priv_pem).unwrap();
        let signing_key = SigningKey::<rsa::sha2::Sha256>::new(key);
        let signature = signing_key.sign(&serde_json::to_vec(&license).unwrap());
        let file = LicenseFile {
            license,
            signature: STANDARD.encode(signature.to_bytes()),
        };
        let raw = serde_json::to_string(&file).unwrap();
        let verified = verify_license(&raw, &pub_key).unwrap();
        assert_eq!(verified.kind, LicenseKind::Commercial);
        assert!(!verified.requires_heartbeat);
        assert_eq!(validity(&verified, "anything", Utc::now()), Ok(()));
    }

    #[test]
    fn rejects_tampered_payload() {
        let (priv_pem, pub_key) = keypair();
        let raw = sign_license(&doc(Duration::days(365)), &priv_pem).unwrap();
        let tampered = raw.replace("Acme Corp", "Evil Corp");
        assert!(verify_license(&tampered, &pub_key).is_err());
    }

    #[test]
    fn rejects_expired_license() {
        let (priv_pem, pub_key) = keypair();
        let raw = sign_license(&doc(Duration::days(-1)), &priv_pem).unwrap();
        let err = verify_license(&raw, &pub_key).unwrap_err().to_string();
        assert!(err.contains("expired"), "{err}");
    }

    #[test]
    fn rejects_wrong_key() {
        let (priv_pem, _) = keypair();
        let (_, other_pub) = keypair();
        let raw = sign_license(&doc(Duration::days(30)), &priv_pem).unwrap();
        assert!(verify_license(&raw, &other_pub).is_err());
    }

    #[test]
    fn embedded_key_parses() {
        assert!(RsaPublicKey::from_public_key_pem(EMBEDDED_PUBLIC_KEY_PEM).is_ok());
    }

    #[test]
    fn days_remaining_positive() {
        let d = doc(Duration::hours(36));
        assert!(days_remaining(&d) >= 1);
    }

    #[test]
    fn validity_fingerprint_binding() {
        let mut d = doc(Duration::days(30));
        d.fingerprint = Some("aa".repeat(32));
        assert_eq!(validity(&d, &"aa".repeat(32), Utc::now()), Ok(()));
        assert_eq!(
            validity(&d, &"bb".repeat(32), Utc::now()),
            Err(LicenseProblem::FingerprintMismatch)
        );
        // Unknown local fingerprint ⇒ binding not enforced.
        assert_eq!(validity(&d, "", Utc::now()), Ok(()));
    }

    #[test]
    fn validity_heartbeat_grace() {
        let now = Utc::now();
        let mut d = doc(Duration::days(30));
        d.requires_heartbeat = true;

        d.issued_at = Some(now - Duration::hours(71));
        assert_eq!(validity(&d, "", now), Ok(()));

        d.issued_at = Some(now - Duration::hours(73));
        assert_eq!(
            validity(&d, "", now),
            Err(LicenseProblem::HeartbeatGraceExpired)
        );
        assert_eq!(grace_until(&d), Some(now - Duration::hours(73) + HEARTBEAT_GRACE));

        d.issued_at = None;
        assert_eq!(validity(&d, "", now), Err(LicenseProblem::MissingIssuedAt));

        // Expiry wins over grace.
        d.expires_at = now - Duration::seconds(1);
        assert_eq!(validity(&d, "", now), Err(LicenseProblem::Expired));
    }

    #[test]
    fn heartbeat_not_required_for_plain_licenses() {
        let now = Utc::now();
        let mut d = doc(Duration::days(30));
        d.issued_at = Some(now - Duration::days(400));
        assert_eq!(validity(&d, "", now), Ok(()));
        assert_eq!(grace_until(&d), None);
        let state = LicenseState {
            doc: Some(d),
            ..Default::default()
        };
        assert!(!heartbeat_due(&state, now));
    }

    #[test]
    fn heartbeat_due_schedule_and_retry_backoff() {
        let now = Utc::now();
        let mut d = doc(Duration::days(30));
        d.requires_heartbeat = true;

        d.issued_at = Some(now - Duration::hours(1));
        let mut state = LicenseState {
            doc: Some(d.clone()),
            ..Default::default()
        };
        assert!(!heartbeat_due(&state, now), "fresh attestation: not due");

        d.issued_at = Some(now - Duration::hours(25));
        state.doc = Some(d.clone());
        assert!(heartbeat_due(&state, now), "older than interval: due");

        state.last_heartbeat_attempt = Some(now - Duration::minutes(10));
        assert!(!heartbeat_due(&state, now), "recent failed attempt: back off");

        state.last_heartbeat_attempt = Some(now - Duration::hours(2));
        assert!(heartbeat_due(&state, now), "retry window elapsed: due again");

        state.revoked_reason = Some("revoked".into());
        assert!(!heartbeat_due(&state, now), "revoked: stop heartbeating");
    }

    #[test]
    fn revocation_overrides_valid_doc() {
        let state = LicenseState {
            doc: Some(doc(Duration::days(30))),
            revoked_reason: Some("403: revoked".into()),
            ..Default::default()
        };
        assert_eq!(
            problem_for(&state, "", Utc::now()),
            Some(LicenseProblem::Revoked)
        );
    }

    #[test]
    fn store_adoption_never_downgrades() {
        let now = Utc::now();
        let commercial = doc(Duration::days(365));
        let mut trial = doc(Duration::days(9));
        trial.kind = LicenseKind::Trial;
        trial.licensee = "trial@example.com".into();

        let none = LicenseState::default();
        assert!(should_adopt(&none, &trial, "", now), "nothing active: adopt");

        let cur_commercial = LicenseState {
            doc: Some(commercial.clone()),
            ..Default::default()
        };
        assert!(
            !should_adopt(&cur_commercial, &trial, "", now),
            "stale trial in store must not replace a mounted commercial license"
        );
        assert!(
            !should_adopt(&cur_commercial, &commercial, "", now),
            "identical document: nothing to do"
        );

        let cur_trial = LicenseState {
            doc: Some(trial.clone()),
            ..Default::default()
        };
        assert!(
            should_adopt(&cur_trial, &commercial, "", now),
            "operator dropped a commercial file: upgrade"
        );

        let mut newer_trial = trial.clone();
        newer_trial.issued_at = Some(trial.issued_at.unwrap() + Duration::hours(1));
        assert!(
            should_adopt(&cur_trial, &newer_trial, "", now),
            "another replica heartbeated: adopt the fresher attestation"
        );
        let mut older_trial = trial.clone();
        older_trial.issued_at = Some(trial.issued_at.unwrap() - Duration::hours(1));
        assert!(
            !should_adopt(&cur_trial, &older_trial, "", now),
            "older attestation never wins"
        );

        let mut expired_cand = commercial.clone();
        expired_cand.expires_at = now - Duration::seconds(1);
        assert!(!should_adopt(&cur_trial, &expired_cand, "", now), "unusable candidate ignored");

        let revoked_current = LicenseState {
            doc: Some(commercial.clone()),
            revoked_reason: Some("403: revoked".into()),
            ..Default::default()
        };
        assert!(
            should_adopt(&revoked_current, &trial, "", now),
            "a usable candidate replaces a revoked license"
        );
    }

    #[test]
    fn email_validation() {
        assert_eq!(
            validate_trial_email("  Ops@Example.COM ").unwrap(),
            "ops@example.com"
        );
        assert!(validate_trial_email("ops@example.com").is_ok());
        assert!(validate_trial_email("first.last+tag@sub.example.co").is_ok());
        for bad in [
            "",
            "a@b",
            "no-at-sign.example.com",
            "@example.com",
            "ops@",
            "ops@example",
            "ops @example.com",
            "ops@exa mple.com",
            "ops@example..com",
            "a@@example.com",
        ] {
            assert!(validate_trial_email(bad).is_err(), "{bad:?} should be rejected");
        }
    }

    #[test]
    fn unwrap_license_response_shapes() {
        let inner = serde_json::json!({"license": {"a": 1}, "signature": "xx"});
        let bare = serde_json::to_string(&inner).unwrap();
        assert_eq!(unwrap_license_response(&bare).unwrap(), bare);
        let wrapped = serde_json::json!({"license_file": inner, "expires_at": "x"}).to_string();
        let out = unwrap_license_response(&wrapped).unwrap();
        assert!(out.contains("\"signature\""));
    }

    #[test]
    fn extract_error_message_shapes() {
        assert_eq!(
            extract_error_message(r#"{"error":{"code":"revoked","message":"license revoked"}}"#),
            "license revoked"
        );
        assert_eq!(extract_error_message(r#"{"error":"revoked"}"#), "revoked");
        assert_eq!(extract_error_message("plain text"), "plain text");
    }

    #[tokio::test]
    async fn heartbeat_once_refreshes_on_200() {
        use wiremock::matchers::{header, method, path};
        use wiremock::{Mock, MockServer, ResponseTemplate};

        let (priv_pem, pub_key) = keypair();
        let fp = "ab".repeat(32);
        let mut d = doc(Duration::days(10));
        d.kind = LicenseKind::Trial;
        d.fingerprint = Some(fp.clone());
        d.requires_heartbeat = true;
        d.issued_at = Some(Utc::now() - Duration::hours(30));
        let stale_raw = sign_license(&d, &priv_pem).unwrap();

        let mut fresh = d.clone();
        fresh.issued_at = Some(Utc::now());
        let fresh_raw = sign_license(&fresh, &priv_pem).unwrap();
        let fresh_val: Value = serde_json::from_str(&fresh_raw).unwrap();

        let server = MockServer::start().await;
        Mock::given(method("POST"))
            .and(path("/v1/heartbeat"))
            .and(header("authorization", "Bearer tok"))
            .respond_with(ResponseTemplate::new(200).set_body_json(serde_json::json!({
                "license_file": fresh_val,
                "expires_at": fresh.expires_at,
            })))
            .expect(1)
            .mount(&server)
            .await;

        let ls = LicenseServer {
            base_url: server.uri(),
            token: "tok".into(),
        };
        match heartbeat_once(&ls, &fp, &stale_raw, &pub_key).await {
            HeartbeatOutcome::Refreshed(loaded) => {
                assert!(loaded.doc.issued_at.unwrap() > d.issued_at.unwrap());
                assert_eq!(loaded.doc.licensee, d.licensee);
            }
            other => panic!("expected Refreshed, got {other:?}"),
        }
    }

    #[tokio::test]
    async fn heartbeat_once_denied_and_unavailable() {
        use wiremock::matchers::{method, path};
        use wiremock::{Mock, MockServer, ResponseTemplate};

        let (priv_pem, pub_key) = keypair();
        let (_, other_pub) = keypair();
        let fp = "cd".repeat(32);
        let mut d = doc(Duration::days(10));
        d.fingerprint = Some(fp.clone());
        d.requires_heartbeat = true;
        let raw = sign_license(&d, &priv_pem).unwrap();

        // 403 → Denied
        let server = MockServer::start().await;
        Mock::given(method("POST"))
            .and(path("/v1/heartbeat"))
            .respond_with(ResponseTemplate::new(403).set_body_json(
                serde_json::json!({"error": {"code": "revoked", "message": "license revoked by vendor"}}),
            ))
            .mount(&server)
            .await;
        let ls = LicenseServer {
            base_url: server.uri(),
            token: "tok".into(),
        };
        match heartbeat_once(&ls, &fp, &raw, &pub_key).await {
            HeartbeatOutcome::Denied { status, reason } => {
                assert_eq!(status, 403);
                assert_eq!(reason, "license revoked by vendor");
            }
            other => panic!("expected Denied, got {other:?}"),
        }

        // 401 (misconfigured token) → Unavailable, never Denied.
        let server = MockServer::start().await;
        Mock::given(method("POST"))
            .respond_with(ResponseTemplate::new(401).set_body_string("unauthorized"))
            .mount(&server)
            .await;
        let ls = LicenseServer {
            base_url: server.uri(),
            token: "tok".into(),
        };
        assert!(matches!(
            heartbeat_once(&ls, &fp, &raw, &pub_key).await,
            HeartbeatOutcome::Unavailable(_)
        ));

        // 200 with a license signed by the wrong key → Unavailable (kept current).
        let server = MockServer::start().await;
        let val: Value = serde_json::from_str(&raw).unwrap();
        Mock::given(method("POST"))
            .respond_with(ResponseTemplate::new(200).set_body_json(serde_json::json!({"license_file": val})))
            .mount(&server)
            .await;
        let ls = LicenseServer {
            base_url: server.uri(),
            token: "tok".into(),
        };
        assert!(matches!(
            heartbeat_once(&ls, &fp, &raw, &other_pub).await,
            HeartbeatOutcome::Unavailable(_)
        ));

        // Connection refused → Unavailable.
        let ls = LicenseServer {
            base_url: "http://127.0.0.1:9".into(),
            token: "tok".into(),
        };
        assert!(matches!(
            heartbeat_once(&ls, &fp, &raw, &pub_key).await,
            HeartbeatOutcome::Unavailable(_)
        ));
    }
}
