//! Prometheus scrape endpoint.
//!
//! Served on its own listener (`METRICS_BIND`, default `0.0.0.0:9464`) rather
//! than the public API port so metrics are reachable by the scraper without
//! being exposed to API clients (they leak internal detail such as licensee
//! state, queue depths and error rates).

use axum::response::IntoResponse;
use axum::routing::get;
use axum::Router;
use metrics_exporter_prometheus::PrometheusHandle;

pub fn router(handle: PrometheusHandle) -> Router {
    Router::new().route("/metrics", get(move || render(handle.clone())))
}

async fn render(handle: PrometheusHandle) -> impl IntoResponse {
    (
        [("content-type", "text/plain; version=0.0.4")],
        handle.render(),
    )
}
