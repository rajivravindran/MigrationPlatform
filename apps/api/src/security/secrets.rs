//! AES-256-GCM secret encryption helper.
//!
//! The `MASTER_KEY` environment variable must be a base64-encoded 32-byte key.
//! Ciphertext is stored alongside the nonce in the `secrets` table.

use aes_gcm::aead::{Aead, KeyInit};
use aes_gcm::{Aes256Gcm, Key, Nonce};
use anyhow::{anyhow, Result};
use base64::{engine::general_purpose::STANDARD, Engine};
use rand::RngCore;

#[derive(Clone)]
pub struct MasterKey(Key<Aes256Gcm>);

impl MasterKey {
    pub fn from_env_value(value: &str) -> Result<Self> {
        let bytes = decode_master_key(value)?;
        Ok(Self(*Key::<Aes256Gcm>::from_slice(&bytes)))
    }

    pub fn encrypt(&self, plaintext: &[u8]) -> Result<(Vec<u8>, Vec<u8>)> {
        let cipher = Aes256Gcm::new(&self.0);
        let mut nonce_bytes = [0u8; 12];
        rand::thread_rng().fill_bytes(&mut nonce_bytes);
        let nonce = Nonce::from_slice(&nonce_bytes);
        let ciphertext = cipher
            .encrypt(nonce, plaintext)
            .map_err(|e| anyhow!("aes-gcm encrypt: {e}"))?;
        Ok((ciphertext, nonce_bytes.to_vec()))
    }

    pub fn decrypt(&self, ciphertext: &[u8], nonce: &[u8]) -> Result<Vec<u8>> {
        let cipher = Aes256Gcm::new(&self.0);
        let nonce = Nonce::from_slice(nonce);
        cipher
            .decrypt(nonce, ciphertext)
            .map_err(|e| anyhow!("aes-gcm decrypt: {e}"))
    }
}

fn decode_master_key(value: &str) -> Result<[u8; 32]> {
    if let Ok(bytes) = STANDARD.decode(value) {
        if bytes.len() == 32 {
            let mut out = [0u8; 32];
            out.copy_from_slice(&bytes);
            return Ok(out);
        }
    }
    // Fallback: allow raw 32+ character string in dev (we hash it for dev ergonomics).
    if value.len() < 32 {
        return Err(anyhow!(
            "MASTER_KEY must be base64 of 32 bytes or a raw string >= 32 chars"
        ));
    }
    use sha2::{Digest, Sha256};
    let mut hasher = Sha256::new();
    hasher.update(value.as_bytes());
    let digest = hasher.finalize();
    let mut out = [0u8; 32];
    out.copy_from_slice(&digest);
    Ok(out)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn encrypt_decrypt_roundtrip() {
        let key =
            MasterKey::from_env_value("dev-master-key-change-me-000000000000000000000000").unwrap();
        let (ct, nonce) = key.encrypt(b"hello world").unwrap();
        assert_ne!(ct, b"hello world");
        let plain = key.decrypt(&ct, &nonce).unwrap();
        assert_eq!(plain, b"hello world");
    }

    #[test]
    fn rejects_short_master_key() {
        assert!(MasterKey::from_env_value("short").is_err());
    }
}
