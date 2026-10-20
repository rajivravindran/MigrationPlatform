//! Execution admission (architecture review A5).
//!
//! Every entry point that binds a template to an execution — a direct job, a
//! schedule, a batch stage — goes through the same checks here, so an
//! unpublished template, a client-supplied version that is not the published
//! one, or a connector the schedule executor cannot read, fails before
//! anything is created.

use sqlx::PgPool;

use crate::error::{ApiError, ApiResult};

/// The immutable template binding an execution runs with.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct TemplateBinding {
    pub id: i64,
    pub version: i32,
}

/// Resolve the published version of `template_id` for `org`. When the client
/// supplied a version it must be that published version; the binding is
/// never silently moved to whatever the client asked for.
pub async fn admit_template(
    db: &PgPool,
    org: i64,
    template_id: i64,
    requested_version: Option<i32>,
) -> ApiResult<TemplateBinding> {
    let row: Option<(i32, bool)> = sqlx::query_as(
        "SELECT version, published FROM rule_templates WHERE id = $1 AND org_id = $2",
    )
    .bind(template_id)
    .bind(org)
    .fetch_optional(db)
    .await?;
    let Some((version, published)) = row else {
        return Err(ApiError::BadRequest(format!(
            "template {template_id} not found"
        )));
    };
    if !published {
        return Err(ApiError::BadRequest(format!(
            "template {template_id} is not published; publish version {version} before running it"
        )));
    }
    if let Some(requested) = requested_version {
        if requested != version {
            return Err(ApiError::BadRequest(format!(
                "template {template_id} version {requested} is not the published version ({version})"
            )));
        }
    }
    Ok(TemplateBinding {
        id: template_id,
        version,
    })
}

/// Connector kinds that the watched-source executor (`WatchPrefixWorkflow`)
/// can read. A schedule bound to any other kind would be accepted and then
/// fail at every fire.
pub fn watched_executor_supports(kind: &str) -> bool {
    matches!(
        kind.to_ascii_lowercase().as_str(),
        "watched_prefix" | "watched-prefix" | "watched_sftp" | "sftp"
    )
}

/// Admit a connector for a schedule: owned by `org` and of a kind the
/// schedule executor supports. Returns the kind.
pub async fn admit_schedule_connector(
    db: &PgPool,
    org: i64,
    connector_id: i64,
) -> ApiResult<String> {
    let kind = crate::sources::require_connector(db, org, connector_id).await?;
    if !watched_executor_supports(&kind) {
        return Err(ApiError::BadRequest(format!(
            "connector {connector_id} is a {kind} connector; schedules watch an object prefix or SFTP path (watched_prefix, watched_sftp). Start a {kind} migration as a job instead"
        )));
    }
    Ok(kind)
}

#[cfg(test)]
mod tests {
    use super::watched_executor_supports;

    #[test]
    fn only_watched_kinds_are_schedulable() {
        for ok in [
            "watched_prefix",
            "watched-prefix",
            "watched_sftp",
            "sftp",
            "WATCHED_PREFIX",
        ] {
            assert!(watched_executor_supports(ok), "{ok}");
        }
        for bad in ["salesforce", "custom", "", "http"] {
            assert!(!watched_executor_supports(bad), "{bad}");
        }
    }
}
