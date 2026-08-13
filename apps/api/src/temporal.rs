//! Client for the orchestrator's internal Temporal bridge.
//!
//! The Go orchestrator exposes a small authenticated HTTP API (`/v1/...`)
//! over the Temporal Go SDK; this client is how the Rust API starts, signals
//! and cancels workflows and manages schedules. When no bridge URL is
//! configured (unit tests, isolated dev) the client degrades to a logging
//! stub that returns deterministic run ids.

use anyhow::{bail, Context, Result};
use serde_json::{json, Value};

#[derive(Clone)]
pub struct TemporalClient {
    bridge_url: Option<String>,
    bridge_token: String,
    http: reqwest::Client,
    namespace: String,
}

impl TemporalClient {
    pub fn new(bridge_url: Option<&str>, bridge_token: &str, namespace: &str) -> Self {
        Self {
            bridge_url: bridge_url
                .map(|u| u.trim_end_matches('/').to_string())
                .filter(|u| !u.is_empty()),
            bridge_token: bridge_token.to_string(),
            http: reqwest::Client::builder()
                .timeout(std::time::Duration::from_secs(20))
                .build()
                .expect("reqwest client"),
            namespace: namespace.to_string(),
        }
    }

    pub fn namespace(&self) -> &str {
        &self.namespace
    }

    pub fn is_stub(&self) -> bool {
        self.bridge_url.is_none()
    }

    async fn post(&self, path: &str, body: &Value) -> Result<Value> {
        let Some(base) = &self.bridge_url else {
            tracing::warn!(
                target = "temporal",
                path,
                ?body,
                "bridge not configured; stub mode"
            );
            return Ok(json!({"stub": true}));
        };
        let resp = self
            .http
            .post(format!("{base}{path}"))
            .bearer_auth(&self.bridge_token)
            .json(body)
            .send()
            .await
            .with_context(|| format!("bridge request {path}"))?;
        let status = resp.status();
        let payload: Value = resp.json().await.unwrap_or_else(|_| json!({}));
        if !status.is_success() {
            bail!(
                "bridge {path} returned {status}: {}",
                payload
                    .get("error")
                    .and_then(|e| e.as_str())
                    .unwrap_or("unknown error")
            );
        }
        Ok(payload)
    }

    /// Start a workflow of the given type on the migration task queue.
    pub async fn start_workflow(
        &self,
        workflow_id: &str,
        workflow_type: &str,
        input: &Value,
    ) -> Result<String> {
        let resp = self
            .post(
                "/v1/workflows/start",
                &json!({
                    "workflowId": workflow_id,
                    "workflowType": workflow_type,
                    "taskQueue": "migration",
                    "input": input,
                }),
            )
            .await?;
        Ok(resp
            .get("runId")
            .and_then(|v| v.as_str())
            .map(str::to_string)
            .unwrap_or_else(|| format!("run-{workflow_id}")))
    }

    /// Start the MigrationWorkflow (Go orchestrator, task queue `migration`).
    pub async fn start_migration_workflow(
        &self,
        workflow_id: &str,
        input: &Value,
    ) -> Result<String> {
        self.start_workflow(workflow_id, "MigrationWorkflow", input)
            .await
    }

    pub async fn signal_workflow(
        &self,
        workflow_id: &str,
        signal: &str,
        payload: &Value,
    ) -> Result<()> {
        self.post(
            "/v1/workflows/signal",
            &json!({"workflowId": workflow_id, "signal": signal, "payload": payload}),
        )
        .await?;
        Ok(())
    }

    pub async fn cancel_workflow(&self, workflow_id: &str) -> Result<()> {
        self.post("/v1/workflows/cancel", &json!({"workflowId": workflow_id}))
            .await?;
        Ok(())
    }

    pub async fn test_sftp_connector(&self, org_id: i64, connector_id: i64) -> Result<()> {
        self.post(
            "/v1/connectors/sftp/test",
            &json!({"orgId": org_id, "connectorId": connector_id}),
        )
        .await?;
        Ok(())
    }

    pub async fn create_schedule(
        &self,
        schedule_id: &str,
        spec: &Value,
        workflow_input: &Value,
        overlap_policy: &str,
        catchup_window_seconds: i32,
        timezone: &str,
    ) -> Result<()> {
        let mut body = json!({
            "scheduleId": schedule_id,
            "timezone": timezone,
            "overlapPolicy": overlap_policy,
            "catchupWindowSeconds": catchup_window_seconds,
            "workflowType": "WatchPrefixWorkflow",
            "taskQueue": "migration",
            "input": workflow_input,
        });
        merge_spec(&mut body, spec);
        self.post("/v1/schedules/create", &body).await?;
        Ok(())
    }

    pub async fn update_schedule(
        &self,
        schedule_id: &str,
        spec: &Value,
        workflow_input: &Value,
    ) -> Result<()> {
        let mut body = json!({"scheduleId": schedule_id, "input": workflow_input});
        merge_spec(&mut body, spec);
        self.post("/v1/schedules/update", &body).await?;
        Ok(())
    }

    pub async fn pause_schedule(&self, schedule_id: &str) -> Result<()> {
        self.post("/v1/schedules/pause", &json!({"scheduleId": schedule_id}))
            .await?;
        Ok(())
    }

    pub async fn unpause_schedule(&self, schedule_id: &str) -> Result<()> {
        self.post("/v1/schedules/unpause", &json!({"scheduleId": schedule_id}))
            .await?;
        Ok(())
    }

    pub async fn delete_schedule(&self, schedule_id: &str) -> Result<()> {
        self.post("/v1/schedules/delete", &json!({"scheduleId": schedule_id}))
            .await?;
        Ok(())
    }

    pub async fn trigger_schedule(&self, schedule_id: &str) -> Result<()> {
        self.post("/v1/schedules/trigger", &json!({"scheduleId": schedule_id}))
            .await?;
        Ok(())
    }
}

fn merge_spec(body: &mut Value, spec: &Value) {
    if let Some(cron) = spec.get("cron").and_then(|v| v.as_str()) {
        body["cron"] = json!(cron);
    }
    if let Some(every) = spec.get("every").and_then(|v| v.as_str()) {
        body["every"] = json!(every);
    }
}

/// Expand POSIX 5-field cron (`m h dom mon dow`) to the 6-field form the
/// `cron` crate expects (`s m h dom mon dow`). Temporal accepts both; we keep
/// the caller's original string in stored specs so the UI/docs stay 5-field.
fn cron_for_validation(expr: &str) -> Result<String> {
    let trimmed = expr.trim();
    let fields = trimmed.split_whitespace().count();
    match fields {
        5 => Ok(format!("0 {trimmed}")),
        6 | 7 => Ok(trimmed.to_string()),
        _ => bail!(
            "invalid cron: expected 5-field POSIX (m h dom mon dow) or 6/7-field with seconds, got {fields} fields"
        ),
    }
}

/// Convert a simple cron expression or interval spec into the structured
/// payload Temporal expects for Schedules.
pub fn normalise_spec(raw: &Value) -> Result<Value> {
    if raw.get("cron").is_some() {
        let cron = raw
            .get("cron")
            .and_then(|v| v.as_str())
            .context("cron missing")?;
        let trimmed = cron.trim();
        let for_parse = cron_for_validation(trimmed)?;
        cron::Schedule::try_from(for_parse.as_str()).context("invalid cron")?;
        return Ok(json!({"cron": trimmed}));
    }
    if let Some(interval) = raw.get("every").and_then(|v| v.as_str()) {
        humantime::parse_duration(interval).context("invalid interval")?;
        return Ok(json!({"every": interval}));
    }
    anyhow::bail!("spec must contain 'cron' or 'every'")
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn accepts_five_field_posix_cron() {
        let out = normalise_spec(&json!({"cron": "0 2 * * *"})).unwrap();
        assert_eq!(out, json!({"cron": "0 2 * * *"}));
    }

    #[test]
    fn accepts_six_field_cron_with_seconds() {
        let out = normalise_spec(&json!({"cron": "0 0 2 * * *"})).unwrap();
        assert_eq!(out, json!({"cron": "0 0 2 * * *"}));
    }

    #[test]
    fn rejects_malformed_cron() {
        assert!(normalise_spec(&json!({"cron": "not-a-cron"})).is_err());
        assert!(normalise_spec(&json!({"cron": "* * *"})).is_err());
    }
}
