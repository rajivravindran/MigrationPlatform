//! DB model types used by multiple modules.

use chrono::{DateTime, Utc};
use serde::{Deserialize, Serialize};
use sqlx::FromRow;

#[derive(Debug, Clone, FromRow, Serialize)]
pub struct User {
    pub id: i64,
    pub org_id: i64,
    pub email: Option<String>,
    pub role: String,
    #[serde(skip)]
    pub password_hash: String,
    pub disabled: bool,
    pub created_at: DateTime<Utc>,
    pub updated_at: DateTime<Utc>,
}

#[derive(Debug, Clone, FromRow, Serialize, Deserialize)]
pub struct RuleTemplateRow {
    pub id: i64,
    pub org_id: i64,
    pub template_key: String,
    pub version: i32,
    pub name: String,
    pub schema_json: serde_json::Value,
    pub published: bool,
    pub created_by: Option<i64>,
    pub created_at: DateTime<Utc>,
}

#[derive(Debug, Clone, FromRow, Serialize)]
pub struct ConnectorRow {
    pub id: i64,
    pub org_id: i64,
    pub name: String,
    pub connector_kind: String,
    pub config_json: serde_json::Value,
    pub secret_id: Option<i64>,
    pub created_by: Option<i64>,
    pub created_at: DateTime<Utc>,
    pub updated_at: DateTime<Utc>,
}

#[derive(Debug, Clone, FromRow, Serialize)]
pub struct JobRow {
    pub id: i64,
    pub org_id: i64,
    pub rule_template_id: i64,
    pub rule_template_version: i32,
    pub schedule_id: Option<i64>,
    pub source_ref: serde_json::Value,
    pub status: String,
    pub totals_json: serde_json::Value,
    pub temporal_workflow_id: Option<String>,
    pub temporal_run_id: Option<String>,
    pub started_at: Option<DateTime<Utc>>,
    pub paused_at: Option<DateTime<Utc>>,
    pub finished_at: Option<DateTime<Utc>>,
    pub created_by: Option<i64>,
    pub created_at: DateTime<Utc>,
    pub updated_at: DateTime<Utc>,
    pub batch_id: Option<i64>,
    pub batch_stage_id: Option<i64>,
    pub results_ref: Option<serde_json::Value>,
}

#[derive(Debug, Clone, FromRow, Serialize)]
pub struct JobRowDetail {
    pub job_id: i64,
    pub row_index: i64,
    pub status: String,
    pub attempts: i32,
    pub last_error: Option<String>,
    pub idempotency_key: Option<String>,
    pub row_json: Option<serde_json::Value>,
    pub payload_json: Option<serde_json::Value>,
    pub response_json: Option<serde_json::Value>,
    pub updated_at: DateTime<Utc>,
}

#[derive(Debug, Clone, FromRow, Serialize)]
pub struct JobRowStepDetail {
    pub job_id: i64,
    pub row_index: i64,
    pub step_index: i32,
    pub step_name: String,
    pub status: String,
    pub attempts: i32,
    pub request_url: Option<String>,
    pub request_json: Option<serde_json::Value>,
    pub response_status: Option<i32>,
    pub response_json: Option<serde_json::Value>,
    pub last_error: Option<String>,
    pub updated_at: DateTime<Utc>,
}

#[derive(Debug, Clone, FromRow, Serialize)]
pub struct ScheduleRow {
    pub id: i64,
    pub org_id: i64,
    pub name: String,
    pub rule_template_id: i64,
    pub rule_template_version: i32,
    pub connector_id: i64,
    pub spec_json: serde_json::Value,
    pub timezone: String,
    pub overlap_policy: String,
    pub catchup_window_seconds: i32,
    pub enabled: bool,
    pub next_run_at: Option<DateTime<Utc>>,
    pub last_run_at: Option<DateTime<Utc>>,
    pub temporal_schedule_id: Option<String>,
    pub created_by: Option<i64>,
    pub created_at: DateTime<Utc>,
    pub updated_at: DateTime<Utc>,
}
