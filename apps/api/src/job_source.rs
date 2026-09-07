//! Operator-facing source summary for a job (upload, watched object, or batch stage).

use serde::Serialize;
use serde_json::{json, Value};

#[derive(Debug, Clone, Serialize, PartialEq, Eq)]
pub struct JobSourceSummary {
    pub kind: String,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub bucket: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub key: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub filename: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub size: Option<i64>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub etag: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub sha256: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub batch_id: Option<i64>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub batch_stage_key: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub batch_stage_file: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub package_key: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub package_filename: Option<String>,
}

pub fn summarize_source(
    source_ref: &Value,
    batch_id: Option<i64>,
    batch_source_ref: Option<&Value>,
    stage_key: Option<&str>,
    stage_file: Option<&str>,
) -> JobSourceSummary {
    let parsed = parse_ref(source_ref);
    let package = batch_source_ref.map(parse_ref);
    let stage_file = nonempty(stage_file)
        .or_else(|| parsed.filename.clone())
        .or_else(|| parsed.key.as_deref().map(basename));

    JobSourceSummary {
        kind: parsed.kind,
        bucket: parsed.bucket,
        key: parsed.key,
        filename: parsed.filename,
        size: parsed.size,
        etag: parsed.etag,
        sha256: parsed.sha256,
        batch_id,
        batch_stage_key: nonempty(stage_key),
        batch_stage_file: stage_file,
        package_key: package.as_ref().and_then(|p| p.key.clone()),
        package_filename: package.as_ref().and_then(|p| {
            p.filename
                .clone()
                .or_else(|| p.key.as_deref().map(basename))
        }),
    }
}

pub fn job_json_with_source(
    job: &crate::db::JobRow,
    batch_source_ref: Option<&Value>,
    stage_key: Option<&str>,
    stage_file: Option<&str>,
) -> Value {
    let mut v = serde_json::to_value(job).unwrap_or_else(|_| json!({}));
    if let Some(obj) = v.as_object_mut() {
        obj.insert(
            "source".to_string(),
            serde_json::to_value(summarize_source(
                &job.source_ref,
                job.batch_id,
                batch_source_ref,
                stage_key,
                stage_file,
            ))
            .unwrap_or(Value::Null),
        );
    }
    v
}

struct ParsedRef {
    kind: String,
    bucket: Option<String>,
    key: Option<String>,
    filename: Option<String>,
    size: Option<i64>,
    etag: Option<String>,
    sha256: Option<String>,
}

fn parse_ref(source_ref: &Value) -> ParsedRef {
    if let Some(s) = source_ref.as_str() {
        let filename = s
            .rsplit(':')
            .next()
            .map(str::to_string)
            .filter(|p| !p.is_empty());
        return ParsedRef {
            kind: "upload".to_string(),
            bucket: None,
            key: Some(s.to_string()),
            filename,
            size: None,
            etag: None,
            sha256: None,
        };
    }
    let kind = source_ref
        .get("type")
        .or_else(|| source_ref.get("kind"))
        .and_then(Value::as_str)
        .unwrap_or("unknown")
        .to_string();
    let key = string_field(source_ref, "key");
    let filename = string_field(source_ref, "filename").or_else(|| key.as_deref().map(basename));
    ParsedRef {
        kind,
        bucket: string_field(source_ref, "bucket"),
        key,
        filename,
        size: source_ref.get("size").and_then(Value::as_i64),
        etag: string_field(source_ref, "etag"),
        sha256: string_field(source_ref, "sha256"),
    }
}

fn string_field(v: &Value, name: &str) -> Option<String> {
    v.get(name)
        .and_then(Value::as_str)
        .map(str::trim)
        .filter(|s| !s.is_empty())
        .map(str::to_string)
}

fn nonempty(s: Option<&str>) -> Option<String> {
    s.map(str::trim)
        .filter(|s| !s.is_empty())
        .map(str::to_string)
}

fn basename(key: &str) -> String {
    key.rsplit(['/', '\\'])
        .next()
        .filter(|s| !s.is_empty())
        .unwrap_or(key)
        .to_string()
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    #[test]
    fn upload_object_uses_filename_and_size() {
        let src = json!({
            "type": "minio",
            "bucket": "migration",
            "key": "uploads/1/ab/abc/orders.csv",
            "filename": "orders.csv",
            "size": 128,
            "sha256": "abc"
        });
        let out = summarize_source(&src, None, None, None, None);
        assert_eq!(out.kind, "minio");
        assert_eq!(out.filename.as_deref(), Some("orders.csv"));
        assert_eq!(out.size, Some(128));
        assert_eq!(out.batch_id, None);
    }

    #[test]
    fn batch_stage_distinguishes_package_and_stage_file() {
        let stage = json!({
            "type": "minio",
            "bucket": "migration",
            "key": "batches/8/stages/contacts.csv",
            "filename": "contacts.csv",
            "size": 40
        });
        let package = json!({
            "type": "minio",
            "bucket": "migration",
            "key": "incoming/demo_batch.tar.gz"
        });
        let out = summarize_source(
            &stage,
            Some(8),
            Some(&package),
            Some("contacts"),
            Some("contacts.csv"),
        );
        assert_eq!(out.batch_id, Some(8));
        assert_eq!(out.package_filename.as_deref(), Some("demo_batch.tar.gz"));
        assert_eq!(out.batch_stage_file.as_deref(), Some("contacts.csv"));
        assert_eq!(out.batch_stage_key.as_deref(), Some("contacts"));
        assert_eq!(out.filename.as_deref(), Some("contacts.csv"));
    }

    #[test]
    fn watched_object_keeps_etag() {
        let src = json!({
            "type": "minio",
            "key": "incoming/vip/file.json",
            "etag": "\"abc\"",
            "size": 9
        });
        let out = summarize_source(&src, None, None, None, None);
        assert_eq!(out.filename.as_deref(), Some("file.json"));
        assert_eq!(out.etag.as_deref(), Some("\"abc\""));
    }
}
