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
//! - `LICENSE_ENFORCE=true` with no valid local license → phone home to
//!   `LICENSE_SERVER_URL` to start/reuse a 10-day trial keyed by install
//!   fingerprint; persist to `LICENSE_STORE_PATH`.
//! - Without enforce and no license → development mode (loud warning).
//! - Under enforce, missing/expired license does **not** abort boot; mutating
//!   APIs are blocked at runtime via [`require_licensed`].

use std::fs;
use std::path::{Path, PathBuf};
use std::sync::OnceLock;

use anyhow::{bail, Context, Result};
use base64::{engine::general_purpose::STANDARD, Engine};
use chrono::{DateTime, Utc};
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

#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize, Default)]
#[serde(rename_all = "snake_case")]
pub enum LicenseKind {
    Trial,
    #[default]
    Commercial,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
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
}

#[derive(Debug, Serialize, Deserialize)]
pub struct LicenseFile {
    pub license: Value,
    pub signature: String,
}

static ACTIVE: OnceLock<Option<LicenseDoc>> = OnceLock::new();
static ENFORCE: OnceLock<bool> = OnceLock::new();
static FINGERPRINT: OnceLock<String> = OnceLock::new();

/// Whether `LICENSE_ENFORCE=true` for this process.
pub fn enforce_enabled() -> bool {
    *ENFORCE.get().unwrap_or(&false)
}

/// The validated license for this process, if one was loaded.
pub fn active_license() -> Option<&'static LicenseDoc> {
    ACTIVE.get().and_then(|o| o.as_ref())
}

/// Full install fingerprint (hex). Computed once at startup.
pub fn process_fingerprint() -> &'static str {
    FINGERPRINT
        .get()
        .map(|s| s.as_str())
        .unwrap_or("unavailable")
}

/// True when a non-expired license is active (and fingerprint matches when set).
pub fn is_licensed() -> bool {
    match active_license() {
        Some(doc) => license_still_valid(doc),
        None => false,
    }
}

fn license_still_valid(doc: &LicenseDoc) -> bool {
    if doc.expires_at <= Utc::now() {
        return false;
    }
    if let Some(bound) = doc.fingerprint.as_deref() {
        let local = FINGERPRINT.get().map(|s| s.as_str()).unwrap_or("");
        if !local.is_empty() && bound != local {
            return false;
        }
    }
    true
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

/// Validate / activate the deployment license. Call once from `main` before serving.
pub async fn enforce_at_startup() -> Result<()> {
    let enforce = std::env::var("LICENSE_ENFORCE").ok().as_deref() == Some("true");
    let _ = ENFORCE.set(enforce);

    let fp = install_fingerprint().context(
        "loading durable installation ID; mount LICENSE_INSTALLATION_ID_PATH on shared durable storage",
    )?;
    tracing::info!(fingerprint = %short_fingerprint(&fp), "install fingerprint ready");
    let _ = FINGERPRINT.set(fp.clone());

    if let Some(doc) = try_load_local(&fp)? {
        activate(doc);
        return Ok(());
    }

    if enforce {
        match activate_trial_phone_home(&fp).await {
            Ok(doc) => {
                activate(doc);
                return Ok(());
            }
            Err(err) => {
                tracing::error!(
                    error = %err,
                    "LICENSE_ENFORCE=true but no valid license and trial activation failed; \
                     mutating APIs will be blocked until a license is available"
                );
                let _ = ACTIVE.set(None);
                return Ok(());
            }
        }
    }

    tracing::warn!(
        "no LICENSE_FILE configured: running in UNLICENSED development mode; \
         set LICENSE_FILE (and LICENSE_ENFORCE=true) for production deployments"
    );
    let _ = ACTIVE.set(None);
    Ok(())
}

fn activate(doc: LicenseDoc) {
    metrics::counter!("license_activation_total", "kind" => format!("{:?}", doc.kind).to_lowercase())
        .increment(1);
    tracing::info!(
        licensee = %doc.licensee,
        kind = ?doc.kind,
        expires_at = %doc.expires_at,
        features = ?doc.features,
        fingerprint = doc.fingerprint.as_deref().map(short_fingerprint),
        "license validated"
    );
    let _ = ACTIVE.set(Some(doc));
}

fn try_load_local(fp: &str) -> Result<Option<LicenseDoc>> {
    let candidates = [
        std::env::var("LICENSE_FILE").ok().filter(|p| !p.is_empty()),
        Some(store_path().display().to_string()).filter(|_| store_path().is_file()),
    ];

    let key = load_public_key()?;
    for path in candidates.into_iter().flatten() {
        match load_and_verify_path(&path, &key, fp) {
            Ok(doc) => return Ok(Some(doc)),
            Err(err) => {
                tracing::warn!(path = %path, error = %err, "license candidate rejected");
            }
        }
    }
    Ok(None)
}

fn load_and_verify_path(path: &str, key: &RsaPublicKey, fp: &str) -> Result<LicenseDoc> {
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
    Ok(doc)
}

fn store_path() -> PathBuf {
    std::env::var("LICENSE_STORE_PATH")
        .ok()
        .filter(|p| !p.is_empty())
        .map(PathBuf::from)
        .unwrap_or_else(|| PathBuf::from(DEFAULT_STORE_PATH))
}

async fn activate_trial_phone_home(fp: &str) -> Result<LicenseDoc> {
    let server = std::env::var("LICENSE_SERVER_URL")
        .ok()
        .filter(|u| !u.is_empty())
        .context("LICENSE_SERVER_URL is required when LICENSE_ENFORCE=true and no local license")?;

    let url = format!("{}/v1/trial/start", server.trim_end_matches('/'));
    let body = serde_json::json!({
        "fingerprint": fp,
        "product": PRODUCT_NAME,
        "version": PRODUCT_VERSION,
    });

    tracing::info!(%url, fingerprint = %short_fingerprint(fp), "requesting trial license");

    let client = reqwest::Client::builder()
        .timeout(std::time::Duration::from_secs(15))
        .build()
        .context("building license HTTP client")?;

    let resp = client
        .post(&url)
        .bearer_auth(
            std::env::var("LICENSE_SERVER_TOKEN")
                .context("LICENSE_SERVER_TOKEN is required for trial activation")?,
        )
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

    // Accept either a bare LicenseFile or `{ "license_file": {...} }` / `{ "license": "<json string>" }`.
    let raw_file = unwrap_license_response(&raw)?;
    let key = load_public_key()?;
    let doc = verify_license(&raw_file, &key)?;
    if doc.kind != LicenseKind::Trial {
        bail!("license server returned a non-trial license");
    }
    if let Some(bound) = doc.fingerprint.as_deref() {
        if bound != fp {
            bail!("trial license fingerprint does not match this install");
        }
    }

    persist_license(&raw_file)?;
    Ok(doc)
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

fn persist_license(raw: &str) -> Result<()> {
    let path = store_path();
    if let Some(parent) = path.parent() {
        fs::create_dir_all(parent)
            .with_context(|| format!("creating license store dir {}", parent.display()))?;
    }
    // Atomic-ish write: temp + rename when on same filesystem.
    let tmp = path.with_extension("json.tmp");
    fs::write(&tmp, raw).with_context(|| format!("writing {}", tmp.display()))?;
    fs::rename(&tmp, &path).with_context(|| format!("persisting license to {}", path.display()))?;
    tracing::info!(path = %path.display(), "persisted trial license");
    Ok(())
}

fn load_public_key() -> Result<RsaPublicKey> {
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
    if is_licensed() {
        return Ok(());
    }
    if active_license().is_some_and(|doc| doc.expires_at <= Utc::now()) {
        metrics::counter!("license_expired_denied_total").increment(1);
    }
    metrics::counter!("license_denied_total").increment(1);
    tracing::warn!("work-producing operation denied by runtime license gate");
    Err(crate::error::ApiError::LicenseRequired(
        "A valid license is required. Start a trial (LICENSE_SERVER_URL) or mount a commercial \
         LICENSE_FILE. See Settings → License."
            .into(),
    ))
}

/// Path helpers used by tests / install docs.
pub fn default_store_path() -> &'static Path {
    Path::new(DEFAULT_STORE_PATH)
}

#[cfg(test)]
mod tests {
    use super::*;
    use chrono::Duration;

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
    }

    #[test]
    fn trial_with_fingerprint_roundtrip() {
        let (priv_pem, pub_key) = keypair();
        let mut d = doc(Duration::days(10));
        d.kind = LicenseKind::Trial;
        d.fingerprint = Some("abc123".into());
        d.licensee = "trial".into();
        let raw = sign_license(&d, &priv_pem).unwrap();
        let verified = verify_license(&raw, &pub_key).unwrap();
        assert_eq!(verified.kind, LicenseKind::Trial);
        assert_eq!(verified.fingerprint.as_deref(), Some("abc123"));
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
}
