use axum::extract::Request;
use axum::http::header::{HeaderName, HeaderValue};
use axum::http::header;
use axum::middleware::Next;
use axum::response::Response;

pub async fn security_headers(req: Request, next: Next) -> Response {
    let mut resp = next.run(req).await;
    let h = resp.headers_mut();
    h.insert(
        header::STRICT_TRANSPORT_SECURITY,
        HeaderValue::from_static("max-age=63072000; includeSubDomains; preload"),
    );
    h.insert(
        header::CONTENT_SECURITY_POLICY,
        HeaderValue::from_static(
            "default-src 'self'; img-src 'self' data:; object-src 'none'; base-uri 'self'",
        ),
    );
    h.insert(header::X_CONTENT_TYPE_OPTIONS, HeaderValue::from_static("nosniff"));
    h.insert(header::REFERRER_POLICY, HeaderValue::from_static("strict-origin-when-cross-origin"));
    if let Ok(name) = HeaderName::from_static("permissions-policy").to_string().parse::<HeaderName>() {
        h.insert(name, HeaderValue::from_static("geolocation=(), camera=(), microphone=()"));
    }
    resp
}

#[cfg(test)]
mod tests {
    use super::*;
    use axum::{body::Body, http::Request as HttpRequest, middleware::from_fn, routing::get, Router};
    use tower::ServiceExt;

    async fn handler() -> &'static str { "ok" }

    #[tokio::test]
    async fn attaches_security_headers() {
        let app = Router::new().route("/", get(handler)).layer(from_fn(security_headers));
        let resp = app
            .oneshot(HttpRequest::builder().uri("/").body(Body::empty()).unwrap())
            .await
            .unwrap();
        let h = resp.headers();
        assert!(h.get(header::STRICT_TRANSPORT_SECURITY).is_some());
        assert!(h.get(header::CONTENT_SECURITY_POLICY).is_some());
        assert_eq!(h.get(header::X_CONTENT_TYPE_OPTIONS).unwrap(), "nosniff");
        assert!(h.get(header::REFERRER_POLICY).is_some());
    }
}
