use serde_json::Value;
use sqlx::PgPool;

use crate::error::ApiResult;

/// Record an audit log entry. Should be called from every mutating handler,
/// inside the same transaction when possible. Callers never block UI on audit
/// write failures - errors are logged and propagated so ops can investigate.
#[allow(clippy::too_many_arguments)]
pub async fn record_audit(
    db: &PgPool,
    org_id: i64,
    actor: &str,
    entity: &str,
    entity_id: Option<String>,
    action: &str,
    before: Option<&Value>,
    after: Option<&Value>,
) -> ApiResult<()> {
    sqlx::query(
        "INSERT INTO audit_log (org_id, actor, entity, entity_id, action, before_json, after_json)
         VALUES ($1, $2, $3, $4, $5, $6, $7)",
    )
    .bind(org_id)
    .bind(actor)
    .bind(entity)
    .bind(entity_id)
    .bind(action)
    .bind(before)
    .bind(after)
    .execute(db)
    .await?;
    Ok(())
}
