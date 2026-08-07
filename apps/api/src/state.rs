use std::sync::Arc;

use anyhow::{Context, Result};
use aws_sdk_s3::config::Region;
use metrics_exporter_prometheus::PrometheusHandle;
use sqlx::PgPool;

use crate::config::Config;
use crate::security::{JwtKeys, MasterKey};
use crate::temporal::TemporalClient;

pub struct AppStateInner {
    pub cfg: Config,
    pub db: PgPool,
    pub redis: redis::Client,
    pub temporal: TemporalClient,
    pub s3: aws_sdk_s3::Client,
    pub jwt: JwtKeys,
    pub master_key: MasterKey,
    pub metrics_handle: PrometheusHandle,
}

#[derive(Clone)]
pub struct AppState(pub Arc<AppStateInner>);

impl AppState {
    pub fn new(inner: AppStateInner) -> Self {
        Self(Arc::new(inner))
    }
}

impl std::ops::Deref for AppState {
    type Target = AppStateInner;
    fn deref(&self) -> &Self::Target {
        &self.0
    }
}

pub async fn build_s3_client(cfg: &Config) -> Result<aws_sdk_s3::Client> {
    let creds = aws_sdk_s3::config::Credentials::new(
        cfg.minio_access_key.clone(),
        cfg.minio_secret_key.clone(),
        None,
        None,
        "static",
    );
    let s3_cfg = aws_sdk_s3::Config::builder()
        .endpoint_url(cfg.minio_endpoint.clone())
        .region(Region::new("us-east-1"))
        .force_path_style(true)
        .credentials_provider(creds)
        .behavior_version_latest()
        .build();
    let client = aws_sdk_s3::Client::from_conf(s3_cfg);

    let bucket = &cfg.minio_bucket;
    match client.head_bucket().bucket(bucket).send().await {
        Ok(_) => {}
        Err(_) => {
            client
                .create_bucket()
                .bucket(bucket)
                .send()
                .await
                .context("creating bucket")?;
        }
    }
    Ok(client)
}
