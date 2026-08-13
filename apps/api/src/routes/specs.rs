//! Imported OpenAPI specifications: upload or URL-fetch a 3.x spec, list its
//! operations (method/path/params/request schema), and let the Designer bind
//! template steps to concrete operations.

use axum::extract::{Path, State};
use axum::response::IntoResponse;
use axum::routing::{get, post};
use axum::{Json, Router};
use serde::Deserialize;
use serde_json::{json, Map, Value};

use crate::error::{ApiError, ApiResult};
use crate::middleware::auth::{require_role, AuthUser};
use crate::security::audit::record_audit;
use crate::state::AppState;

/// Cap imported spec size (both upload and URL-fetch) at 4 MiB.
const MAX_SPEC_BYTES: usize = 4 * 1024 * 1024;

pub fn router() -> Router<AppState> {
    Router::new()
        .route("/openapi-specs", get(list_specs).post(create_spec))
        .route("/openapi-specs/:id", get(get_spec).delete(delete_spec))
        .route("/openapi-specs/:id/operations", get(list_operations))
        .route("/openapi-specs/parse-preview", post(parse_preview))
}

#[derive(Deserialize)]
pub struct CreateSpec {
    pub name: String,
    /// Either an inline spec document...
    pub spec: Option<Value>,
    /// ...or a URL to fetch it from (must be a public host).
    pub url: Option<String>,
}

async fn create_spec(
    State(state): State<AppState>,
    AuthUser(claims): AuthUser,
    Json(req): Json<CreateSpec>,
) -> ApiResult<Json<Value>> {
    require_role(&claims, &["admin", "editor"])?;

    let (spec, source_url) = match (req.spec, req.url) {
        (Some(spec), _) => (spec, None),
        (None, Some(url)) => (fetch_spec(&url).await?, Some(url)),
        (None, None) => return Err(ApiError::BadRequest("provide spec or url".into())),
    };

    let raw = serde_json::to_vec(&spec)?;
    if raw.len() > MAX_SPEC_BYTES {
        return Err(ApiError::BadRequest(format!(
            "spec too large ({} bytes, max {MAX_SPEC_BYTES})",
            raw.len()
        )));
    }
    validate_openapi(&spec)?;

    let created_by: i64 = claims.sub.parse().map_err(|_| ApiError::Unauthorized)?;
    let row: (i64,) = sqlx::query_as(
        "INSERT INTO openapi_specs (org_id, name, source_url, spec_json, created_by)
         VALUES ($1, $2, $3, $4, $5)
         ON CONFLICT (org_id, name) DO UPDATE SET
            source_url = EXCLUDED.source_url,
            spec_json = EXCLUDED.spec_json,
            updated_at = now()
         RETURNING id",
    )
    .bind(claims.org)
    .bind(&req.name)
    .bind(&source_url)
    .bind(&spec)
    .bind(created_by)
    .fetch_one(&state.db)
    .await?;

    record_audit(
        &state.db,
        claims.org,
        &claims.sub,
        "openapi_spec",
        Some(row.0.to_string()),
        "import",
        None,
        None,
    )
    .await?;

    let ops = parse_operations(&spec, source_url.as_deref());
    Ok(Json(json!({
        "id": row.0,
        "name": req.name,
        "operation_count": ops.len(),
        "operations": ops,
    })))
}

async fn list_specs(
    State(state): State<AppState>,
    AuthUser(claims): AuthUser,
) -> ApiResult<Json<Value>> {
    let rows: Vec<(i64, String, Option<String>, chrono::DateTime<chrono::Utc>)> = sqlx::query_as(
        "SELECT id, name, source_url, updated_at FROM openapi_specs WHERE org_id = $1 ORDER BY name",
    )
    .bind(claims.org)
    .fetch_all(&state.db)
    .await?;
    Ok(Json(json!({
        "items": rows.iter().map(|(id, name, url, updated)| json!({
            "id": id, "name": name, "source_url": url, "updated_at": updated,
        })).collect::<Vec<_>>()
    })))
}

async fn get_spec(
    State(state): State<AppState>,
    AuthUser(claims): AuthUser,
    Path(id): Path<i64>,
) -> ApiResult<Json<Value>> {
    let row: Option<(String, Option<String>, Value)> = sqlx::query_as(
        "SELECT name, source_url, spec_json FROM openapi_specs WHERE id = $1 AND org_id = $2",
    )
    .bind(id)
    .bind(claims.org)
    .fetch_optional(&state.db)
    .await?;
    let Some((name, source_url, spec)) = row else {
        return Err(ApiError::NotFound);
    };
    Ok(Json(
        json!({"id": id, "name": name, "source_url": source_url, "spec": spec}),
    ))
}

async fn delete_spec(
    State(state): State<AppState>,
    AuthUser(claims): AuthUser,
    Path(id): Path<i64>,
) -> ApiResult<impl IntoResponse> {
    require_role(&claims, &["admin", "editor"])?;
    let n = sqlx::query("DELETE FROM openapi_specs WHERE id = $1 AND org_id = $2")
        .bind(id)
        .bind(claims.org)
        .execute(&state.db)
        .await?
        .rows_affected();
    if n == 0 {
        return Err(ApiError::NotFound);
    }
    record_audit(
        &state.db,
        claims.org,
        &claims.sub,
        "openapi_spec",
        Some(id.to_string()),
        "delete",
        None,
        None,
    )
    .await?;
    Ok(Json(json!({"deleted": true})))
}

async fn list_operations(
    State(state): State<AppState>,
    AuthUser(claims): AuthUser,
    Path(id): Path<i64>,
) -> ApiResult<Json<Value>> {
    let row: Option<(Value, Option<String>)> = sqlx::query_as(
        "SELECT spec_json, source_url FROM openapi_specs WHERE id = $1 AND org_id = $2",
    )
    .bind(id)
    .bind(claims.org)
    .fetch_optional(&state.db)
    .await?;
    let Some((spec, source_url)) = row else {
        return Err(ApiError::NotFound);
    };
    Ok(Json(
        json!({"items": parse_operations(&spec, source_url.as_deref())}),
    ))
}

#[derive(Deserialize)]
pub struct ParsePreview {
    pub spec: Value,
}

/// Parse an inline spec without persisting it (used by the import dialog).
async fn parse_preview(
    State(_state): State<AppState>,
    AuthUser(_claims): AuthUser,
    Json(req): Json<ParsePreview>,
) -> ApiResult<Json<Value>> {
    validate_openapi(&req.spec)?;
    Ok(Json(json!({"items": parse_operations(&req.spec, None)})))
}

// ---------------- fetch + validation ----------------

async fn fetch_spec(url: &str) -> ApiResult<Value> {
    let parsed =
        url::Url::parse(url).map_err(|e| ApiError::BadRequest(format!("invalid url: {e}")))?;
    if parsed.scheme() != "https" && parsed.scheme() != "http" {
        return Err(ApiError::BadRequest("url must be http(s)".into()));
    }
    // SSRF guard: resolve the host and refuse private/internal ranges unless
    // explicitly allowed (dev compose networks).
    let allow_private = std::env::var("ALLOW_PRIVATE_DESTINATIONS").ok().as_deref() == Some("true");
    if !allow_private {
        let host = parsed
            .host_str()
            .ok_or_else(|| ApiError::BadRequest("url missing host".into()))?;
        let addrs = tokio::net::lookup_host((host, parsed.port_or_known_default().unwrap_or(443)))
            .await
            .map_err(|e| ApiError::BadRequest(format!("cannot resolve {host}: {e}")))?;
        for addr in addrs {
            if ip_is_private(addr.ip()) {
                return Err(ApiError::BadRequest(format!(
                    "url host {host} resolves to a private/internal address"
                )));
            }
        }
    }
    let client = reqwest::Client::builder()
        .timeout(std::time::Duration::from_secs(15))
        .redirect(reqwest::redirect::Policy::limited(3))
        .build()
        .map_err(|e| ApiError::External(e.to_string()))?;
    let resp = client
        .get(parsed)
        .send()
        .await
        .map_err(|e| ApiError::External(format!("fetch spec: {e}")))?;
    if !resp.status().is_success() {
        return Err(ApiError::External(format!(
            "fetch spec: status {}",
            resp.status()
        )));
    }
    let bytes = resp
        .bytes()
        .await
        .map_err(|e| ApiError::External(format!("read spec: {e}")))?;
    if bytes.len() > MAX_SPEC_BYTES {
        return Err(ApiError::BadRequest("spec too large".into()));
    }
    serde_json::from_slice(&bytes).map_err(|e| {
        ApiError::BadRequest(format!(
            "spec is not valid JSON: {e} (YAML specs must be converted to JSON)"
        ))
    })
}

pub fn ip_is_private(ip: std::net::IpAddr) -> bool {
    match ip {
        std::net::IpAddr::V4(v4) => {
            v4.is_loopback()
                || v4.is_private()
                || v4.is_link_local()
                || v4.is_unspecified()
                || (v4.octets()[0] == 100 && (64..128).contains(&v4.octets()[1]))
            // CGNAT
        }
        std::net::IpAddr::V6(v6) => {
            v6.is_loopback()
                || v6.is_unspecified()
                || (v6.segments()[0] & 0xfe00) == 0xfc00 // unique-local
                || (v6.segments()[0] & 0xffc0) == 0xfe80 // link-local
        }
    }
}

fn validate_openapi(spec: &Value) -> ApiResult<()> {
    let version = spec
        .get("openapi")
        .and_then(|v| v.as_str())
        .ok_or_else(|| {
            ApiError::ValidationFailed("missing `openapi` version field (3.x required)".into())
        })?;
    if !version.starts_with('3') {
        return Err(ApiError::ValidationFailed(format!(
            "unsupported OpenAPI version {version}; 3.x required"
        )));
    }
    if !spec.get("paths").map(|p| p.is_object()).unwrap_or(false) {
        return Err(ApiError::ValidationFailed(
            "spec has no `paths` object".into(),
        ));
    }
    Ok(())
}

// ---------------- operation parsing ----------------

const METHODS: [&str; 5] = ["get", "post", "put", "patch", "delete"];

/// Flatten an OpenAPI 3.x document into the operation summaries the Designer
/// needs: id, method, path, parameters and the (ref-resolved) request/response
/// schemas.
///
/// `source_url` is used when the spec's `servers[0].url` is relative (common
/// for specs served from the same host as the API, e.g. Petstore's `/api/v3`).
pub fn parse_operations(spec: &Value, source_url: Option<&str>) -> Vec<Value> {
    let mut out = Vec::new();
    let empty = Map::new();
    let paths = spec
        .get("paths")
        .and_then(|p| p.as_object())
        .unwrap_or(&empty);
    let base_url = spec
        .get("servers")
        .and_then(|s| s.as_array())
        .and_then(|arr| arr.first())
        .and_then(|s| s.get("url"))
        .and_then(|u| u.as_str())
        .unwrap_or("");

    for (path, item) in paths {
        let Some(item) = item.as_object() else {
            continue;
        };
        let shared_params = item
            .get("parameters")
            .cloned()
            .unwrap_or(Value::Array(vec![]));
        for method in METHODS {
            let Some(op) = item.get(method) else { continue };
            let op_id = op
                .get("operationId")
                .and_then(|v| v.as_str())
                .map(str::to_string)
                .unwrap_or_else(|| {
                    format!("{}_{}", method, path.trim_matches('/').replace('/', "_"))
                });

            let mut params: Vec<Value> = Vec::new();
            for src in [&shared_params, op.get("parameters").unwrap_or(&Value::Null)] {
                if let Some(arr) = src.as_array() {
                    for p in arr {
                        let p = resolve_ref(spec, p, 0);
                        params.push(json!({
                            "name": p.get("name").and_then(|v| v.as_str()).unwrap_or(""),
                            "in": p.get("in").and_then(|v| v.as_str()).unwrap_or("query"),
                            "required": p.get("required").and_then(|v| v.as_bool()).unwrap_or(false),
                            "schema": resolve_ref(spec, p.get("schema").unwrap_or(&Value::Null), 0),
                        }));
                    }
                }
            }

            let request_schema = op
                .get("requestBody")
                .map(|rb| resolve_ref(spec, rb, 0))
                .and_then(|rb| {
                    rb.get("content")
                        .and_then(|c| c.get("application/json"))
                        .and_then(|j| j.get("schema"))
                        .map(|s| resolve_schema(spec, s, 0))
                })
                .unwrap_or(Value::Null);

            let response_schema = op
                .get("responses")
                .and_then(|r| r.as_object())
                .and_then(|r| ["200", "201", "202"].iter().find_map(|code| r.get(*code)))
                .map(|resp| resolve_ref(spec, resp, 0))
                .and_then(|resp| {
                    resp.get("content")
                        .and_then(|c| c.get("application/json"))
                        .and_then(|j| j.get("schema"))
                        .map(|s| resolve_schema(spec, s, 0))
                })
                .unwrap_or(Value::Null);

            out.push(json!({
                "operation_id": op_id,
                "method": method.to_uppercase(),
                "path": path,
                "url": resolve_operation_url(base_url, path, source_url),
                "summary": op.get("summary").and_then(|v| v.as_str()).unwrap_or(""),
                "description": op.get("description").and_then(|v| v.as_str()).unwrap_or(""),
                "parameters": params,
                "request_schema": request_schema,
                "response_schema": response_schema,
            }));
        }
    }
    out
}

/// Build an absolute operation URL from the spec's server entry and path.
fn resolve_operation_url(server_url: &str, path: &str, source_url: Option<&str>) -> String {
    let server = server_url.trim_end_matches('/');
    let path = if path.starts_with('/') {
        path.to_string()
    } else {
        format!("/{path}")
    };

    if server.starts_with("http://") || server.starts_with("https://") {
        return format!("{server}{path}");
    }

    // Relative server URL (e.g. `/api/v3`) — derive origin from where we imported the spec.
    if let Some(src) = source_url {
        if let Ok(u) = url::Url::parse(src) {
            let host = u.host_str().unwrap_or("");
            let mut origin = format!("{}://{}", u.scheme(), host);
            if let Some(port) = u.port() {
                origin = format!("{origin}:{port}");
            }
            if server.is_empty() {
                return format!("{origin}{path}");
            }
            return format!("{origin}{server}{path}");
        }
    }

    format!("{server}{path}")
}

/// Resolve a single `$ref` (non-recursive into children).
fn resolve_ref(spec: &Value, node: &Value, depth: u8) -> Value {
    if depth > 8 {
        return node.clone();
    }
    if let Some(r) = node.get("$ref").and_then(|v| v.as_str()) {
        if let Some(target) = lookup_pointer(spec, r) {
            return resolve_ref(spec, &target, depth + 1);
        }
    }
    node.clone()
}

/// Recursively resolve `$ref`s inside a schema (bounded depth to survive
/// cyclic schemas).
fn resolve_schema(spec: &Value, node: &Value, depth: u8) -> Value {
    if depth > 6 {
        return json!({"$comment": "truncated (max ref depth)"});
    }
    if let Some(r) = node.get("$ref").and_then(|v| v.as_str()) {
        if let Some(target) = lookup_pointer(spec, r) {
            return resolve_schema(spec, &target, depth + 1);
        }
        return node.clone();
    }
    match node {
        Value::Object(map) => {
            let mut out = Map::new();
            for (k, v) in map {
                out.insert(k.clone(), resolve_schema(spec, v, depth + 1));
            }
            Value::Object(out)
        }
        Value::Array(items) => Value::Array(
            items
                .iter()
                .map(|v| resolve_schema(spec, v, depth + 1))
                .collect(),
        ),
        _ => node.clone(),
    }
}

fn lookup_pointer(spec: &Value, reference: &str) -> Option<Value> {
    let pointer = reference.strip_prefix('#')?;
    spec.pointer(pointer).cloned()
}

#[cfg(test)]
mod tests {
    use super::*;

    fn petstore() -> Value {
        json!({
            "openapi": "3.0.0",
            "info": {"title": "t", "version": "1"},
            "servers": [{"url": "https://api.pets.example/v1"}],
            "paths": {
                "/pets": {
                    "post": {
                        "operationId": "createPet",
                        "summary": "Create a pet",
                        "requestBody": {"content": {"application/json": {"schema": {"$ref": "#/components/schemas/Pet"}}}},
                        "responses": {"201": {"content": {"application/json": {"schema": {"$ref": "#/components/schemas/PetOut"}}}}}
                    }
                },
                "/pets/{petId}/toys": {
                    "parameters": [{"name": "petId", "in": "path", "required": true, "schema": {"type": "string"}}],
                    "post": {"operationId": "addToy", "requestBody": {"content": {"application/json": {"schema": {"type": "object"}}}}, "responses": {}}
                }
            },
            "components": {"schemas": {
                "Pet": {"type": "object", "required": ["name"], "properties": {"name": {"type": "string"}, "tag": {"$ref": "#/components/schemas/Tag"}}},
                "PetOut": {"type": "object", "properties": {"id": {"type": "string"}}},
                "Tag": {"type": "string"}
            }}
        })
    }

    #[test]
    fn resolves_relative_server_url_from_import_source() {
        let spec = json!({
            "openapi": "3.0.0",
            "servers": [{"url": "/api/v3"}],
            "paths": {"/pet": {"post": {"operationId": "addPet", "responses": {}}}}
        });
        let ops = parse_operations(
            &spec,
            Some("https://petstore3.swagger.io/api/v3/openapi.json"),
        );
        assert_eq!(ops[0]["url"], "https://petstore3.swagger.io/api/v3/pet");
    }

    #[test]
    fn parses_operations_with_refs() {
        let ops = parse_operations(&petstore(), None);
        assert_eq!(ops.len(), 2);
        let create = ops
            .iter()
            .find(|o| o["operation_id"] == "createPet")
            .unwrap();
        assert_eq!(create["method"], "POST");
        assert_eq!(create["url"], "https://api.pets.example/v1/pets");
        assert_eq!(
            create["request_schema"]["properties"]["name"]["type"],
            "string"
        );
        assert_eq!(
            create["request_schema"]["properties"]["tag"]["type"], "string",
            "nested $ref must resolve"
        );
        assert_eq!(
            create["response_schema"]["properties"]["id"]["type"],
            "string"
        );

        let toy = ops.iter().find(|o| o["operation_id"] == "addToy").unwrap();
        assert_eq!(toy["parameters"][0]["name"], "petId");
        assert_eq!(toy["parameters"][0]["in"], "path");
    }

    #[test]
    fn rejects_v2_specs() {
        let spec = json!({"swagger": "2.0", "paths": {}});
        assert!(validate_openapi(&spec).is_err());
    }

    #[test]
    fn private_ip_detection() {
        use std::net::IpAddr;
        for s in [
            "127.0.0.1",
            "10.0.0.1",
            "192.168.1.1",
            "172.16.9.9",
            "169.254.169.254",
            "100.64.1.1",
            "::1",
            "fc00::1",
        ] {
            assert!(ip_is_private(s.parse::<IpAddr>().unwrap()), "{s}");
        }
        for s in ["8.8.8.8", "93.184.216.34"] {
            assert!(!ip_is_private(s.parse::<IpAddr>().unwrap()), "{s}");
        }
    }
}
