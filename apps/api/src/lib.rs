//! migration-api library. Shared by the main HTTP binary and the admin CLI.

pub mod config;
pub mod db;
pub mod error;
pub mod job_source;
pub mod middleware;
pub mod routes;
pub mod rule_template_types;
pub mod sampling;
pub mod security;
pub mod state;
pub mod telemetry;
pub mod temporal;

pub fn build_router(state: state::AppState) -> axum::Router {
    use axum::Router;
    use tower_http::cors::CorsLayer;
    use tower_http::trace::TraceLayer;

    Router::new()
        .merge(routes::health::router())
        .merge(routes::metrics::router())
        .merge(routes::auth::router())
        .merge(routes::templates::router())
        .merge(routes::files::router())
        .merge(routes::connectors::router())
        .merge(routes::jobs::router())
        .merge(routes::batches::router())
        .merge(routes::schedules::router())
        .merge(routes::dry_run::router())
        .merge(routes::openapi::router())
        .merge(routes::specs::router())
        .merge(routes::llm::router())
        .merge(routes::license::router())
        .layer(CorsLayer::permissive())
        .layer(TraceLayer::new_for_http())
        .layer(axum::middleware::from_fn(
            middleware::request_id::request_id_layer,
        ))
        .layer(axum::middleware::from_fn(
            middleware::security_headers::security_headers,
        ))
        .with_state(state)
}
