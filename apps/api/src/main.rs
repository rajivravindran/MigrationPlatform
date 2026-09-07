use std::net::SocketAddr;

use anyhow::Context;
use tokio::net::TcpListener;
use tracing::info;

use migration_api::config::Config;
use migration_api::{build_router, routes, security, state, telemetry, temporal};

#[tokio::main]
async fn main() -> anyhow::Result<()> {
    dotenvy::dotenv().ok();
    let cfg = Config::from_env().context("loading API config")?;

    telemetry::init_tracing("migration-api", cfg.otlp_endpoint.as_deref())
        .context("initialising tracing")?;

    security::license::enforce_at_startup()
        .await
        .context("license check")?;

    let metrics_handle = telemetry::init_metrics().context("initialising prometheus metrics")?;

    // Heartbeat-required licenses are re-attested every 24h (72h offline
    // grace); the same loop adopts a refreshed license store written by another
    // replica or an operator. Runs on every replica; one small request per day.
    tokio::spawn(async move {
        let mut ticker = tokio::time::interval(std::time::Duration::from_secs(
            security::license::HEARTBEAT_TICK_SECS,
        ));
        ticker.set_missed_tick_behavior(tokio::time::MissedTickBehavior::Delay);
        loop {
            // First tick completes immediately, so a stale store is re-attested at boot.
            ticker.tick().await;
            security::license::heartbeat_tick().await;
        }
    });

    let db_pool = sqlx::postgres::PgPoolOptions::new()
        .max_connections(cfg.db_max_connections)
        .connect(&cfg.database_url)
        .await
        .context("connecting to postgres")?;

    sqlx::migrate!("./migrations")
        .run(&db_pool)
        .await
        .context("running migrations")?;

    let redis = redis::Client::open(cfg.redis_url.clone()).context("parsing redis url")?;
    let temporal_client = temporal::TemporalClient::new(
        cfg.orchestrator_bridge_url.as_deref(),
        cfg.bridge_token.as_deref().unwrap_or(""),
        &cfg.temporal_namespace,
    );
    if temporal_client.is_stub() {
        tracing::warn!(
            "ORCHESTRATOR_BRIDGE_URL is not set: Temporal operations run in stub mode and no workflows will be dispatched"
        );
    }
    let minio = state::build_s3_client(&cfg).await?;
    let jwt_keys = security::JwtKeys::load(&cfg)?;
    let master_key = security::MasterKey::from_env_value(&cfg.master_key)?;

    let state = state::AppState::new(state::AppStateInner {
        cfg: cfg.clone(),
        db: db_pool,
        redis,
        temporal: temporal_client,
        s3: minio,
        jwt: jwt_keys,
        master_key,
        metrics_handle: metrics_handle.clone(),
    });

    let app = build_router(state);

    // Prometheus scrape endpoint on its own port (compose, Prometheus and the
    // Helm chart all target METRICS_BIND); kept off the public API listener.
    let metrics_addr: SocketAddr = cfg.metrics_bind.parse().context("parsing METRICS_BIND")?;
    let metrics_listener = TcpListener::bind(metrics_addr)
        .await
        .context("binding metrics port")?;
    info!(addr = %metrics_addr, "metrics listening");
    tokio::spawn(async move {
        if let Err(e) = axum::serve(metrics_listener, routes::metrics::router(metrics_handle)).await
        {
            tracing::error!(error = %e, "metrics server exited");
        }
    });

    let addr: SocketAddr = cfg.api_bind.parse().context("parsing API_BIND")?;
    let listener = TcpListener::bind(addr).await.context("binding api port")?;
    info!(%addr, "migration-api listening");
    axum::serve(
        listener,
        app.into_make_service_with_connect_info::<SocketAddr>(),
    )
    .await
    .context("axum serve")?;

    Ok(())
}
