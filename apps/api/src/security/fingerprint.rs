//! Durable installation identity for trial binding.
//!
//! The installation UUID lives on operator-provisioned durable storage shared
//! by every API replica. Hostnames and container machine IDs are deliberately
//! not used because they change across restarts and replicas.

use anyhow::{bail, Context, Result};
use sha2::{Digest, Sha256};
use std::fs::{self, OpenOptions};
use std::io::Write;
use std::path::{Path, PathBuf};

const DEFAULT_INSTALLATION_ID_PATH: &str = "/var/lib/migration/installation-id";

/// Full SHA-256 hex digest used as the trial key.
pub fn install_fingerprint() -> Result<String> {
    let id = load_or_create_installation_id(&installation_id_path())?;
    let digest = Sha256::digest(format!("installation-id:{id}").as_bytes());
    Ok(hex::encode(digest))
}

/// First 12 hex chars for Settings / support tickets.
pub fn short_fingerprint(full: &str) -> String {
    full.chars().take(12).collect()
}

fn installation_id_path() -> PathBuf {
    std::env::var("LICENSE_INSTALLATION_ID_PATH")
        .ok()
        .filter(|v| !v.is_empty())
        .map(PathBuf::from)
        .unwrap_or_else(|| PathBuf::from(DEFAULT_INSTALLATION_ID_PATH))
}

fn load_or_create_installation_id(path: &Path) -> Result<String> {
    if let Ok(raw) = fs::read_to_string(path) {
        return validate_installation_id(&raw);
    }
    if let Some(parent) = path.parent() {
        fs::create_dir_all(parent)
            .with_context(|| format!("creating installation ID directory {}", parent.display()))?;
    }
    let id = uuid::Uuid::new_v4().to_string();
    match OpenOptions::new().write(true).create_new(true).open(path) {
        Ok(mut file) => {
            file.write_all(id.as_bytes())
                .with_context(|| format!("writing installation ID {}", path.display()))?;
            file.sync_all()
                .with_context(|| format!("syncing installation ID {}", path.display()))?;
            Ok(id)
        }
        Err(err) if err.kind() == std::io::ErrorKind::AlreadyExists => {
            let raw = fs::read_to_string(path).with_context(|| {
                format!(
                    "reading concurrently-created installation ID {}",
                    path.display()
                )
            })?;
            validate_installation_id(&raw)
        }
        Err(err) => {
            Err(err).with_context(|| format!("creating installation ID {}", path.display()))
        }
    }
}

fn validate_installation_id(raw: &str) -> Result<String> {
    let id = raw.trim();
    let parsed = uuid::Uuid::parse_str(id).context("installation ID is not a UUID")?;
    if parsed.is_nil() {
        bail!("installation ID must not be nil");
    }
    Ok(parsed.to_string())
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn fingerprint_is_stable_hex() {
        let dir = tempfile_dir();
        let path = dir.join("installation-id");
        let id_a = load_or_create_installation_id(&path).unwrap();
        let id_b = load_or_create_installation_id(&path).unwrap();
        let a = hex::encode(Sha256::digest(format!("installation-id:{id_a}").as_bytes()));
        let b = hex::encode(Sha256::digest(format!("installation-id:{id_b}").as_bytes()));
        assert_eq!(a, b);
        assert_eq!(a.len(), 64);
        assert!(a.chars().all(|c| c.is_ascii_hexdigit()));
    }

    #[test]
    fn short_is_prefix() {
        let full = "a".repeat(64);
        assert_eq!(short_fingerprint(&full), &full[..12]);
    }

    fn tempfile_dir() -> PathBuf {
        let p =
            std::env::temp_dir().join(format!("migration-fingerprint-{}", uuid::Uuid::new_v4()));
        fs::create_dir_all(&p).unwrap();
        p
    }
}
