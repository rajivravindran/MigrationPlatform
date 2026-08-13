//! Typed representation of the shared rule-template JSON schema.
//!
//! The canonical schema lives in
//! `packages/rule-schema/schema/rule-template.schema.json`. The golden fixture
//! `packages/rule-schema/fixtures/golden.json` round-trips losslessly through
//! every language binding (see the round-trip tests at the bottom of this file
//! for Rust).

use serde::{Deserialize, Serialize};
use serde_json::Value;

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq)]
#[serde(rename_all = "lowercase")]
pub enum SourceType {
    Csv,
    Json,
    Xml,
    Salesforce,
    WatchedPrefix,
}

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq)]
#[serde(rename_all = "lowercase")]
pub enum FieldType {
    String,
    Integer,
    Number,
    Boolean,
    Datetime,
    Object,
}

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq)]
#[serde(deny_unknown_fields)]
pub struct SourceField {
    pub name: String,
    #[serde(rename = "type")]
    pub field_type: FieldType,
}

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq)]
#[serde(deny_unknown_fields)]
pub struct Source {
    #[serde(rename = "type")]
    pub source_type: SourceType,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub schema: Option<Vec<SourceField>>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub options: Option<Value>,
    #[serde(
        default,
        skip_serializing_if = "Option::is_none",
        rename = "connectorId"
    )]
    pub connector_id: Option<i64>,
}

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq)]
#[serde(deny_unknown_fields)]
pub struct PreprocessStep {
    pub id: String,
    pub field: String,
    #[serde(rename = "fn")]
    pub function: String,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub args: Option<Value>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub code: Option<String>,
}

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq)]
#[serde(rename_all = "lowercase")]
pub enum AuthType {
    Bearer,
    Basic,
    ApiKey,
    Oauth2Client,
}

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq)]
pub struct Auth {
    #[serde(rename = "type")]
    pub auth_type: AuthType,
    #[serde(default, skip_serializing_if = "Option::is_none", rename = "secretRef")]
    pub secret_ref: Option<String>,
    #[serde(
        default,
        skip_serializing_if = "Option::is_none",
        rename = "headerName"
    )]
    pub header_name: Option<String>,
}

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq)]
#[serde(rename_all = "UPPERCASE")]
pub enum HttpMethod {
    Get,
    Post,
    Put,
    Patch,
    Delete,
}

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq)]
#[serde(deny_unknown_fields)]
pub struct Destination {
    #[serde(rename = "type")]
    pub destination_type: String,
    pub method: HttpMethod,
    pub url: String,
    /// Substituted into `{name}` placeholders in `url`. Each value is either a
    /// literal string, `{"$from": "field"}`, or `{"$literal": "..."}`.
    #[serde(
        default,
        skip_serializing_if = "Option::is_none",
        rename = "pathParams"
    )]
    pub path_params: Option<std::collections::BTreeMap<String, Value>>,
    /// Appended to `url` as `?k=v`. Array values produce repeated keys.
    #[serde(
        default,
        skip_serializing_if = "Option::is_none",
        rename = "queryParams"
    )]
    pub query_params: Option<std::collections::BTreeMap<String, Value>>,
    /// Header values are templated like `pathParams`; literal strings are kept
    /// as-is for backwards compatibility with templates authored before
    /// templated headers were supported.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub headers: Option<std::collections::BTreeMap<String, Value>>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub auth: Option<Auth>,
    #[serde(
        default,
        skip_serializing_if = "Option::is_none",
        rename = "idempotencyKey"
    )]
    pub idempotency_key: Option<Value>,
    #[serde(
        default,
        skip_serializing_if = "Option::is_none",
        rename = "idempotencyHeader"
    )]
    pub idempotency_header: Option<String>,
}

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq)]
#[serde(rename_all = "lowercase")]
pub enum BackoffKind {
    Exponential,
    Fixed,
}

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq, Default)]
#[serde(deny_unknown_fields)]
pub struct Retry {
    #[serde(
        default,
        skip_serializing_if = "Option::is_none",
        rename = "maxAttempts"
    )]
    pub max_attempts: Option<u32>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub backoff: Option<BackoffKind>,
    #[serde(
        default,
        skip_serializing_if = "Option::is_none",
        rename = "initialIntervalMs"
    )]
    pub initial_interval_ms: Option<u64>,
}

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq)]
#[serde(deny_unknown_fields)]
pub struct Mapping {
    pub payload: serde_json::Map<String, Value>,
}

/// What to do after a step definitively fails (4xx or retries exhausted).
#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq, Default)]
#[serde(rename_all = "lowercase")]
pub enum OnFailure {
    #[default]
    Stop,
    Continue,
}

/// One HTTP call in a multi-step chain. Later steps may reference earlier
/// responses through `{"$fromResponse":"stepName","path":"$.jsonpath"}` value
/// expressions.
#[derive(Debug, Clone, Serialize, Deserialize, PartialEq)]
#[serde(deny_unknown_fields)]
pub struct Step {
    pub name: String,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub description: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub mapping: Option<Mapping>,
    pub destination: Destination,
    /// Defaults to `stop` when omitted.
    #[serde(default, skip_serializing_if = "Option::is_none", rename = "onFailure")]
    pub on_failure: Option<OnFailure>,
}

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq)]
#[serde(deny_unknown_fields)]
pub struct RuleTemplate {
    pub id: String,
    pub version: u32,
    pub name: String,
    pub source: Source,
    pub preprocess: Vec<PreprocessStep>,
    /// Single-destination shape (exactly one of mapping+destination or steps
    /// is present; enforced by the JSON Schema `oneOf`).
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub mapping: Option<Mapping>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub destination: Option<Destination>,
    /// Multi-step chain shape.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub steps: Option<Vec<Step>>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub retry: Option<Retry>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub concurrency: Option<u16>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub export: Option<ExportSpec>,
}

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Default)]
#[serde(deny_unknown_fields)]
pub struct ExportSpec {
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub columns: Option<Vec<ExportColumn>>,
}

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq)]
pub struct ExportColumn {
    pub name: String,
    #[serde(default, rename = "$from", skip_serializing_if = "Option::is_none")]
    pub from: Option<String>,
    #[serde(
        default,
        rename = "$fromResponse",
        skip_serializing_if = "Option::is_none"
    )]
    pub from_response: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub path: Option<String>,
}

impl RuleTemplate {
    /// Normalize either template shape into an ordered step list.
    pub fn execution_steps(&self) -> Vec<Step> {
        if let Some(steps) = &self.steps {
            return steps.clone();
        }
        match (&self.mapping, &self.destination) {
            (Some(mapping), Some(destination)) => vec![Step {
                name: "main".to_string(),
                description: None,
                mapping: Some(mapping.clone()),
                destination: destination.clone(),
                on_failure: None,
            }],
            _ => Vec::new(),
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    const GOLDEN: &str = include_str!("../../../packages/rule-schema/fixtures/golden.json");
    const GOLDEN_URL_TPL: &str =
        include_str!("../../../packages/rule-schema/fixtures/golden_url_templating.json");
    const GOLDEN_STEPS: &str =
        include_str!("../../../packages/rule-schema/fixtures/golden_steps.json");

    #[test]
    fn golden_parses() {
        let tpl: RuleTemplate = serde_json::from_str(GOLDEN).expect("parse");
        assert_eq!(tpl.id, "rt_golden");
        assert!(matches!(tpl.source.source_type, SourceType::Csv));
        assert_eq!(tpl.preprocess.len(), 2);
        // Legacy shape normalizes to a single "main" step.
        let steps = tpl.execution_steps();
        assert_eq!(steps.len(), 1);
        assert_eq!(steps[0].name, "main");
    }

    #[test]
    fn golden_steps_parses_and_roundtrips() {
        let original: Value = serde_json::from_str(GOLDEN_STEPS).expect("parse value");
        let typed: RuleTemplate = serde_json::from_value(original.clone()).expect("parse typed");
        let steps = typed.execution_steps();
        assert_eq!(steps.len(), 2);
        assert_eq!(steps[0].name, "createContact");
        assert!(steps[1]
            .destination
            .path_params
            .as_ref()
            .unwrap()
            .contains_key("contactId"));
        let back: Value = serde_json::to_value(&typed).expect("serialize");
        assert_eq!(original, back);
    }

    #[test]
    fn golden_roundtrips() {
        let original: Value = serde_json::from_str(GOLDEN).expect("parse value");
        let typed: RuleTemplate = serde_json::from_value(original.clone()).expect("parse typed");
        let back: Value = serde_json::to_value(&typed).expect("serialize");
        assert_eq!(original, back);
    }

    #[test]
    fn golden_url_templating_roundtrips() {
        let original: Value = serde_json::from_str(GOLDEN_URL_TPL).expect("parse value");
        let typed: RuleTemplate = serde_json::from_value(original.clone()).expect("parse typed");
        assert!(typed
            .destination
            .as_ref()
            .unwrap()
            .path_params
            .as_ref()
            .unwrap()
            .contains_key("userId"));
        let back: Value = serde_json::to_value(&typed).expect("serialize");
        assert_eq!(original, back);
    }
}
