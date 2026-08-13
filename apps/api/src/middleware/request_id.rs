use axum::extract::Request;
use axum::http::HeaderValue;
use axum::middleware::Next;
use axum::response::Response;
use tracing::Span;
use uuid::Uuid;

const HEADER: &str = "x-request-id";

pub async fn request_id_layer(mut req: Request, next: Next) -> Response {
    let id = req
        .headers()
        .get(HEADER)
        .and_then(|v| v.to_str().ok().map(str::to_owned))
        .unwrap_or_else(|| Uuid::new_v4().to_string());

    req.headers_mut().insert(
        HEADER,
        HeaderValue::from_str(&id).unwrap_or_else(|_| HeaderValue::from_static("-")),
    );

    Span::current().record("request_id", tracing::field::display(&id));

    let mut resp = next.run(req).await;
    if let Ok(v) = HeaderValue::from_str(&id) {
        resp.headers_mut().insert(HEADER, v);
    }
    resp
}

#[cfg(test)]
mod tests {
    use super::*;
    use axum::{
        body::Body, http::Request as HttpRequest, middleware::from_fn, routing::get, Router,
    };
    use tower::ServiceExt;

    async fn handler() -> &'static str {
        "ok"
    }

    #[tokio::test]
    async fn generates_request_id_when_missing() {
        let app = Router::new()
            .route("/", get(handler))
            .layer(from_fn(request_id_layer));
        let resp = app
            .oneshot(HttpRequest::builder().uri("/").body(Body::empty()).unwrap())
            .await
            .unwrap();
        let id = resp.headers().get(HEADER).unwrap().to_str().unwrap();
        assert!(!id.is_empty());
        assert!(id.len() >= 16, "uuid-like id expected");
    }

    #[tokio::test]
    async fn echoes_incoming_request_id() {
        let app = Router::new()
            .route("/", get(handler))
            .layer(from_fn(request_id_layer));
        let resp = app
            .oneshot(
                HttpRequest::builder()
                    .uri("/")
                    .header(HEADER, "my-test-id-123")
                    .body(Body::empty())
                    .unwrap(),
            )
            .await
            .unwrap();
        assert_eq!(resp.headers().get(HEADER).unwrap(), "my-test-id-123");
    }
}
