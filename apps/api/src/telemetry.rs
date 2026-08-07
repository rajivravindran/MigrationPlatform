use anyhow::{Context, Result};
use metrics_exporter_prometheus::{PrometheusBuilder, PrometheusHandle};
use opentelemetry::global;
use opentelemetry::trace::TracerProvider as _;
use opentelemetry_otlp::WithExportConfig;
use opentelemetry_sdk::{runtime, trace as sdktrace, Resource};
use tracing_subscriber::layer::SubscriberExt;
use tracing_subscriber::util::SubscriberInitExt;
use tracing_subscriber::{fmt, EnvFilter};

/// Initialize structured JSON logs, request-ID propagation, and OpenTelemetry
/// trace export when an OTLP endpoint is configured.
pub fn init_tracing(service_name: &'static str, otlp_endpoint: Option<&str>) -> Result<()> {
    let filter = EnvFilter::try_from_default_env()
        .unwrap_or_else(|_| EnvFilter::new("info,sqlx=warn,hyper=warn"));

    let fmt_layer = fmt::layer()
        .json()
        .with_target(false)
        .with_current_span(true);

    if let Some(endpoint) = otlp_endpoint {
        let provider = opentelemetry_otlp::new_pipeline()
            .tracing()
            .with_exporter(
                opentelemetry_otlp::new_exporter()
                    .tonic()
                    .with_endpoint(endpoint),
            )
            .with_trace_config(
                sdktrace::Config::default()
                    .with_resource(Resource::new([opentelemetry::KeyValue::new(
                        "service.name",
                        service_name,
                    )])),
            )
            .install_batch(runtime::Tokio)
            .context("failed to install OTLP tracer")?;
        global::set_tracer_provider(provider.clone());
        let tracer = provider.tracer(service_name);
        let otel_layer = tracing_opentelemetry::layer().with_tracer(tracer);
        tracing_subscriber::registry()
            .with(filter)
            .with(fmt_layer)
            .with(otel_layer)
            .init();
    } else {
        tracing_subscriber::registry()
            .with(filter)
            .with(fmt_layer)
            .init();
    }

    tracing::info!(service_name, "tracing initialised");
    Ok(())
}

pub fn init_metrics() -> Result<PrometheusHandle> {
    let handle = PrometheusBuilder::new()
        .install_recorder()
        .context("install prometheus recorder")?;
    Ok(handle)
}

pub fn shutdown_otel() {
    global::shutdown_tracer_provider();
}
