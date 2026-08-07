use argon2::password_hash::{rand_core::OsRng, PasswordHasher, SaltString};
use argon2::{Argon2, PasswordHash, PasswordVerifier};

use crate::error::{ApiError, ApiResult};

pub fn hash_password(password: &str) -> ApiResult<String> {
    let salt = SaltString::generate(&mut OsRng);
    let argon = Argon2::default();
    argon
        .hash_password(password.as_bytes(), &salt)
        .map(|h| h.to_string())
        .map_err(|e| ApiError::Internal(anyhow::anyhow!("argon2: {e}")))
}

pub fn verify_password(password: &str, hash: &str) -> ApiResult<bool> {
    let parsed = PasswordHash::new(hash)
        .map_err(|e| ApiError::Internal(anyhow::anyhow!("argon2 parse: {e}")))?;
    Ok(Argon2::default()
        .verify_password(password.as_bytes(), &parsed)
        .is_ok())
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn roundtrip() {
        let h = hash_password("correct horse battery").unwrap();
        assert!(verify_password("correct horse battery", &h).unwrap());
        assert!(!verify_password("wrong", &h).unwrap());
    }
}
