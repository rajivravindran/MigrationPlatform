//! JWT RS256 signing and verification.
//!
//! If `JWT_PRIVATE_KEY_PATH` and `JWT_PUBLIC_KEY_PATH` are provided we load
//! PEM-encoded RSA keys. Otherwise we generate an ephemeral RSA-2048 pair at
//! boot (dev only). Production deployments MUST mount stable keys to prevent
//! token invalidation across restarts.

use std::fs;

use anyhow::{Context, Result};
use chrono::{Duration, Utc};
use jsonwebtoken::{decode, encode, Algorithm, DecodingKey, EncodingKey, Header, Validation};
use rsa::pkcs8::{EncodePrivateKey, EncodePublicKey};
use rsa::{RsaPrivateKey, RsaPublicKey};
use serde::{Deserialize, Serialize};

use crate::config::Config;
use crate::error::{ApiError, ApiResult};

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct Claims {
    pub sub: String,
    pub org: i64,
    pub role: String,
    pub iss: String,
    pub aud: String,
    pub iat: i64,
    pub exp: i64,
}

#[derive(Clone)]
pub struct JwtKeys {
    encoding: EncodingKey,
    decoding: DecodingKey,
    issuer: String,
    audience: String,
}

impl JwtKeys {
    pub fn load(cfg: &Config) -> Result<Self> {
        let (private_pem, public_pem) =
            match (&cfg.jwt_private_key_path, &cfg.jwt_public_key_path) {
                (Some(priv_path), Some(pub_path)) => (
                    fs::read(priv_path).context("read JWT private key")?,
                    fs::read(pub_path).context("read JWT public key")?,
                ),
                _ => generate_ephemeral_keypair()?,
            };

        let encoding = EncodingKey::from_rsa_pem(&private_pem).context("parse RSA private key")?;
        let decoding = DecodingKey::from_rsa_pem(&public_pem).context("parse RSA public key")?;
        Ok(Self {
            encoding,
            decoding,
            issuer: cfg.jwt_issuer.clone(),
            audience: cfg.jwt_audience.clone(),
        })
    }

    pub fn issue(&self, user_id: i64, org_id: i64, role: &str) -> ApiResult<String> {
        let now = Utc::now();
        let claims = Claims {
            sub: user_id.to_string(),
            org: org_id,
            role: role.to_string(),
            iss: self.issuer.clone(),
            aud: self.audience.clone(),
            iat: now.timestamp(),
            exp: (now + Duration::hours(12)).timestamp(),
        };
        encode(&Header::new(Algorithm::RS256), &claims, &self.encoding)
            .map_err(|e| ApiError::Internal(anyhow::anyhow!("jwt encode: {e}")))
    }

    pub fn verify(&self, token: &str) -> ApiResult<Claims> {
        let mut v = Validation::new(Algorithm::RS256);
        v.set_issuer(&[self.issuer.clone()]);
        v.set_audience(&[self.audience.clone()]);
        let data = decode::<Claims>(token, &self.decoding, &v).map_err(|_| ApiError::Unauthorized)?;
        Ok(data.claims)
    }
}

fn generate_ephemeral_keypair() -> Result<(Vec<u8>, Vec<u8>)> {
    tracing::warn!(
        "generating ephemeral RSA keypair for JWT; tokens will be invalidated on restart"
    );
    let mut rng = rand::thread_rng();
    let priv_key = RsaPrivateKey::new(&mut rng, 2048).context("generate RSA")?;
    let pub_key = RsaPublicKey::from(&priv_key);
    let priv_pem = priv_key
        .to_pkcs8_pem(rsa::pkcs8::LineEnding::LF)
        .context("encode private PEM")?
        .as_bytes()
        .to_vec();
    let pub_pem = pub_key
        .to_public_key_pem(rsa::pkcs8::LineEnding::LF)
        .context("encode public PEM")?
        .as_bytes()
        .to_vec();
    Ok((priv_pem, pub_pem))
}

#[cfg(test)]
mod tests {
    use super::*;

    fn make_keys(issuer: &str, audience: &str) -> JwtKeys {
        let (priv_pem, pub_pem) = generate_ephemeral_keypair().unwrap();
        JwtKeys {
            encoding: EncodingKey::from_rsa_pem(&priv_pem).unwrap(),
            decoding: DecodingKey::from_rsa_pem(&pub_pem).unwrap(),
            issuer: issuer.to_string(),
            audience: audience.to_string(),
        }
    }

    #[test]
    fn issue_and_verify_roundtrip() {
        let keys = make_keys("mp-test", "mp-audience");
        let token = keys.issue(42, 7, "admin").unwrap();
        let claims = keys.verify(&token).unwrap();
        assert_eq!(claims.sub, "42");
        assert_eq!(claims.org, 7);
        assert_eq!(claims.role, "admin");
    }

    #[test]
    fn rejects_wrong_audience() {
        let a = make_keys("mp-test", "aud-a");
        let b = make_keys("mp-test", "aud-b");
        let token = a.issue(1, 1, "viewer").unwrap();
        assert!(b.verify(&token).is_err());
    }
}
