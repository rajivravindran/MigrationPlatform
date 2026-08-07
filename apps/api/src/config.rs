use std::env;

use anyhow::{Context, Result};

#[derive(Debug, Clone)]
pub struct Config {
    pub api_bind: String,
    pub metrics_bind: String,
    pub database_url: String,
    pub redis_url: String,
    pub temporal_host_port: String,
    pub temporal_namespace: String,
    /// Base URL of the orchestrator's internal Temporal bridge
    /// (e.g. http://orchestrator:7070). Empty = stub mode (tests only).
    pub orchestrator_bridge_url: Option<String>,
    /// Shared secret for the bridge.
    pub bridge_token: Option<String>,
    pub otlp_endpoint: Option<String>,
    pub minio_endpoint: String,
    pub minio_access_key: String,
    pub minio_secret_key: String,
    pub minio_bucket: String,
    pub master_key: String,
    pub jwt_private_key_path: Option<String>,
    pub jwt_public_key_path: Option<String>,
    pub jwt_issuer: String,
    pub jwt_audience: String,
    pub db_max_connections: u32,
}

impl Config {
    pub fn from_env() -> Result<Self> {
        Ok(Self {
            api_bind: env_default("API_BIND", "0.0.0.0:8080"),
            metrics_bind: env_default("METRICS_BIND", "0.0.0.0:9464"),
            database_url: env_default(
                "DATABASE_URL",
                "postgres://postgres:postgres@localhost:5432/migration",
            ),
            redis_url: env_default("REDIS_URL", "redis://localhost:6379/0"),
            temporal_host_port: env_default("TEMPORAL_ADDRESS", "localhost:7233"),
            temporal_namespace: env_default("TEMPORAL_NAMESPACE", "default"),
            orchestrator_bridge_url: env::var("ORCHESTRATOR_BRIDGE_URL").ok().filter(|v| !v.is_empty()),
            bridge_token: env::var("BRIDGE_TOKEN").ok().filter(|v| !v.is_empty()),
            otlp_endpoint: env::var("OTEL_EXPORTER_OTLP_ENDPOINT").ok(),
            minio_endpoint: env_default("MINIO_ENDPOINT", "http://localhost:9000"),
            minio_access_key: env_default("MINIO_ACCESS_KEY", "minio"),
            minio_secret_key: env_default("MINIO_SECRET_KEY", "minio123"),
            minio_bucket: env_default("MINIO_BUCKET", "migration"),
            master_key: env::var("MASTER_KEY").context(
                "MASTER_KEY env var (base64 32 bytes) is required for secret encryption",
            )?,
            jwt_private_key_path: env::var("JWT_PRIVATE_KEY_PATH").ok(),
            jwt_public_key_path: env::var("JWT_PUBLIC_KEY_PATH").ok(),
            jwt_issuer: env_default("JWT_ISSUER", "migration-platform"),
            jwt_audience: env_default("JWT_AUDIENCE", "migration-platform-users"),
            db_max_connections: env::var("DB_MAX_CONNECTIONS")
                .ok()
                .and_then(|v| v.parse().ok())
                .unwrap_or(16),
        })
    }
}

fn env_default(key: &str, default: &str) -> String {
    env::var(key).unwrap_or_else(|_| default.to_string())
}
