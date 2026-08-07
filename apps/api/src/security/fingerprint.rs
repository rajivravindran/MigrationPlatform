//! Stable install fingerprint for trial binding.
//!
//! Hashes machine-id (preferred) or hostname — never stores cleartext PII.
//! The full hex digest is what phones home; UIs show only a short prefix.

use sha2::{Digest, Sha256};
use std::fs;
use std::path::Path;

const MACHINE_ID_PATHS: &[&str] = &["/etc/machine-id", "/var/lib/dbus/machine-id"];

/// Full SHA-256 hex digest used as the trial key.
pub fn install_fingerprint() -> String {
    let material = fingerprint_material();
    let digest = Sha256::digest(material.as_bytes());
    hex::encode(digest)
}

/// First 12 hex chars for Settings / support tickets.
pub fn short_fingerprint(full: &str) -> String {
    full.chars().take(12).collect()
}

fn fingerprint_material() -> String {
    for path in MACHINE_ID_PATHS {
        if let Ok(raw) = fs::read_to_string(Path::new(path)) {
            let id = raw.trim();
            if !id.is_empty() {
                return format!("machine-id:{id}");
            }
        }
    }
    let host = hostname().unwrap_or_else(|| "unknown-host".into());
    format!("hostname:{host}")
}

fn hostname() -> Option<String> {
    if let Ok(h) = fs::read_to_string("/etc/hostname") {
        let h = h.trim();
        if !h.is_empty() {
            return Some(h.to_string());
        }
    }
    std::env::var("HOSTNAME")
        .ok()
        .filter(|h| !h.is_empty())
        .or_else(|| std::env::var("COMPUTERNAME").ok().filter(|h| !h.is_empty()))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn fingerprint_is_stable_hex() {
        let a = install_fingerprint();
        let b = install_fingerprint();
        assert_eq!(a, b);
        assert_eq!(a.len(), 64);
        assert!(a.chars().all(|c| c.is_ascii_hexdigit()));
    }

    #[test]
    fn short_is_prefix() {
        let full = install_fingerprint();
        assert_eq!(short_fingerprint(&full), &full[..12]);
    }
}
