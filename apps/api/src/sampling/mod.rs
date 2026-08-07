//! Server-side sampling of input files (CSV/JSON/XML) so the Designer UI can
//! auto-derive a column palette without ever loading the full file in the
//! browser.
//!
//! All samplers share a strict, deterministic contract:
//!   * They read at most `byte_cap` bytes (8 MiB by default).
//!   * They return at most `row_cap` rows (200 by default; UI requests 25).
//!   * They infer types from the *sampled* rows only and clearly mark when
//!     the sample was truncated so the UI can warn the user.
//!
//! Type inference is intentionally strict: only the literal tokens
//! `true|false|0|1` (case-insensitive) become booleans, integers and floats
//! must parse cleanly across *every* non-empty sampled value, and timestamps
//! must match either RFC3339 or `YYYY-MM-DD`. Anything else falls back to
//! `string` so we never silently lose precision.

pub mod csv;
pub mod json;
pub mod type_infer;
pub mod xml;

use serde::Serialize;

/// Maximum payload we'll ever pull out of object storage for sampling purposes.
/// Larger files are still sampleable -- we just stop reading after this many
/// bytes and mark the sample as truncated.
pub const DEFAULT_BYTE_CAP: usize = 8 * 1024 * 1024;
/// Hard ceiling on rows returned to the UI regardless of what the caller asks
/// for. Keeps payloads tiny and prevents cardinality blowups.
pub const MAX_ROW_CAP: usize = 200;
/// Default row count if the caller does not specify one.
pub const DEFAULT_ROW_CAP: usize = 25;

#[derive(Debug, Clone, Serialize)]
pub struct SampleColumn {
    pub name: String,
    /// One of `string|integer|number|boolean|datetime`.
    pub r#type: &'static str,
    /// True iff at least one sampled value for this column was empty/null.
    pub nullable: bool,
}

#[derive(Debug, Clone, Serialize)]
pub struct SampleResult {
    pub format: &'static str,
    pub columns: Vec<SampleColumn>,
    /// Each row is a map keyed by column name. Values are JSON primitives so
    /// the UI can render them as-is in the dry-run preview.
    pub rows: Vec<serde_json::Map<String, serde_json::Value>>,
    pub row_count: usize,
    pub truncated: bool,
    /// Bytes actually read from the source.
    pub bytes_read: usize,
}

/// Returned by callers when the file is structurally invalid for the requested
/// format. We surface this as a 422 to the client.
#[derive(Debug, thiserror::Error)]
pub enum SampleError {
    #[error("empty input")]
    Empty,
    #[error("invalid {format}: {message}")]
    Invalid { format: &'static str, message: String },
    #[error("unsupported sampling format: {0}")]
    Unsupported(String),
}
