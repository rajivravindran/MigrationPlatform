//! JSON sampler. Accepts two shapes:
//!   * a top-level array of objects (`[{...}, {...}]`)
//!   * NDJSON / JSONL (one object per line)
//!
//! Whichever shape is detected, we project each record into a flat map of
//! top-level keys (nested objects/arrays are stringified for the preview but
//! still typed as `string`) and union the column set across all sampled rows.

use serde_json::{Map, Value};

use super::type_infer::infer_column_type;
use super::{SampleColumn, SampleError, SampleResult};

pub fn sample(
    bytes: &[u8],
    row_cap: usize,
    truncated_input: bool,
) -> Result<SampleResult, SampleError> {
    if bytes.is_empty() {
        return Err(SampleError::Empty);
    }

    // Strip BOM if present.
    let trimmed = strip_bom(bytes);
    let first_non_ws = trimmed
        .iter()
        .find(|b| !b.is_ascii_whitespace())
        .copied()
        .unwrap_or(0);

    let records: Vec<Value> = if first_non_ws == b'[' {
        parse_array(trimmed, truncated_input)?
    } else {
        parse_ndjson(trimmed, truncated_input)?
    };

    if records.is_empty() {
        return Err(SampleError::Invalid {
            format: "json",
            message: "no records".into(),
        });
    }

    let mut truncated = truncated_input;
    let take_n = if records.len() > row_cap {
        truncated = true;
        row_cap
    } else {
        records.len()
    };

    // Determine the union of keys across the sampled records, preserving the
    // order in which we first saw them so the UI palette is stable.
    let mut key_order: Vec<String> = Vec::new();
    let mut seen = std::collections::HashSet::new();
    let mut rows: Vec<Map<String, Value>> = Vec::with_capacity(take_n);

    for rec in records.into_iter().take(take_n) {
        let obj = match rec {
            Value::Object(o) => o,
            other => {
                return Err(SampleError::Invalid {
                    format: "json",
                    message: format!("expected object record, got {}", type_of(&other)),
                });
            }
        };
        for k in obj.keys() {
            if seen.insert(k.clone()) {
                key_order.push(k.clone());
            }
        }
        rows.push(obj);
    }

    // Build per-column raw value buffers for type inference. Nested values are
    // serialized via `to_string` so we keep `infer_column_type`'s string-only
    // contract; that way nested data conservatively shows up as `string`.
    let mut raw: Vec<Vec<String>> = vec![Vec::with_capacity(rows.len()); key_order.len()];
    let mut nullable = vec![false; key_order.len()];

    for row in &rows {
        for (i, key) in key_order.iter().enumerate() {
            match row.get(key) {
                Some(Value::Null) | None => {
                    nullable[i] = true;
                    raw[i].push(String::new());
                }
                Some(Value::String(s)) => raw[i].push(s.clone()),
                Some(Value::Bool(b)) => raw[i].push(b.to_string()),
                Some(Value::Number(n)) => raw[i].push(n.to_string()),
                Some(other) => raw[i].push(other.to_string()),
            }
        }
    }

    let columns: Vec<SampleColumn> = key_order
        .iter()
        .enumerate()
        .map(|(i, name)| SampleColumn {
            name: name.clone(),
            r#type: infer_column_type(raw[i].iter().map(String::as_str)),
            nullable: nullable[i],
        })
        .collect();

    Ok(SampleResult {
        format: "json",
        row_count: rows.len(),
        rows,
        columns,
        truncated,
        bytes_read: bytes.len(),
    })
}

fn parse_array(bytes: &[u8], truncated_input: bool) -> Result<Vec<Value>, SampleError> {
    match serde_json::from_slice::<Vec<Value>>(bytes) {
        Ok(v) => Ok(v),
        Err(e) if truncated_input => {
            // The input was clipped mid-array, so a parse failure is expected.
            // Fall back to a streaming parse: pull as many complete elements as
            // we can.
            tracing::debug!(error = %e, "json: array parse failed on truncated input, falling back");
            best_effort_array(bytes)
        }
        Err(e) => Err(SampleError::Invalid {
            format: "json",
            message: e.to_string(),
        }),
    }
}

fn parse_ndjson(bytes: &[u8], _truncated_input: bool) -> Result<Vec<Value>, SampleError> {
    let mut out = Vec::new();
    for (lineno, line) in bytes.split(|b| *b == b'\n').enumerate() {
        let s = std::str::from_utf8(line).unwrap_or("");
        let s = s.trim();
        if s.is_empty() {
            continue;
        }
        match serde_json::from_str::<Value>(s) {
            Ok(v) => out.push(v),
            // Tolerate a malformed last line -- it's almost certainly the byte
            // cap clipping us mid-record.
            Err(e) => {
                tracing::debug!(line = lineno + 1, error = %e, "ndjson: skipping malformed line");
                continue;
            }
        }
    }
    Ok(out)
}

/// When the byte cap clipped a `[ ... ]` array we can't parse the whole thing,
/// but we can usually extract the first N complete object literals by walking
/// brace depth.
fn best_effort_array(bytes: &[u8]) -> Result<Vec<Value>, SampleError> {
    let s = std::str::from_utf8(bytes).map_err(|_| SampleError::Invalid {
        format: "json",
        message: "non-utf8 input".into(),
    })?;
    let mut out = Vec::new();
    let mut depth = 0i32;
    let mut start: Option<usize> = None;
    let mut in_string = false;
    let mut escape = false;

    for (i, ch) in s.char_indices() {
        if in_string {
            if escape {
                escape = false;
            } else if ch == '\\' {
                escape = true;
            } else if ch == '"' {
                in_string = false;
            }
            continue;
        }
        match ch {
            '"' => in_string = true,
            '{' => {
                if depth == 0 {
                    start = Some(i);
                }
                depth += 1;
            }
            '}' => {
                depth -= 1;
                if depth == 0 {
                    if let Some(st) = start.take() {
                        if let Ok(v) = serde_json::from_str::<Value>(&s[st..=i]) {
                            out.push(v);
                        }
                    }
                }
            }
            _ => {}
        }
    }
    if out.is_empty() {
        return Err(SampleError::Invalid {
            format: "json",
            message: "could not extract any complete records".into(),
        });
    }
    Ok(out)
}

fn type_of(v: &Value) -> &'static str {
    match v {
        Value::Null => "null",
        Value::Bool(_) => "bool",
        Value::Number(_) => "number",
        Value::String(_) => "string",
        Value::Array(_) => "array",
        Value::Object(_) => "object",
    }
}

fn strip_bom(bytes: &[u8]) -> &[u8] {
    if bytes.len() >= 3 && &bytes[..3] == b"\xef\xbb\xbf" {
        &bytes[3..]
    } else {
        bytes
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn samples_top_level_array() {
        let body = br#"[{"id":1,"name":"Ada"},{"id":2,"name":"Bob"}]"#;
        let r = sample(body, 25, false).unwrap();
        assert_eq!(r.row_count, 2);
        assert_eq!(r.columns.len(), 2);
    }

    #[test]
    fn samples_ndjson() {
        let body = b"{\"id\":1}\n{\"id\":2}\n{\"id\":3}\n";
        let r = sample(body, 25, false).unwrap();
        assert_eq!(r.row_count, 3);
    }

    #[test]
    fn unions_keys_across_rows() {
        let body = br#"[{"a":1},{"b":2}]"#;
        let r = sample(body, 25, false).unwrap();
        let names: Vec<_> = r.columns.iter().map(|c| c.name.clone()).collect();
        assert!(names.contains(&"a".to_string()));
        assert!(names.contains(&"b".to_string()));
    }

    #[test]
    fn nested_values_become_string() {
        let body = br#"[{"meta":{"x":1}}]"#;
        let r = sample(body, 25, false).unwrap();
        assert_eq!(r.columns[0].r#type, "string");
    }

    #[test]
    fn caps_rows_and_marks_truncated() {
        let body = br#"[{"id":1},{"id":2},{"id":3},{"id":4}]"#;
        let r = sample(body, 2, false).unwrap();
        assert_eq!(r.row_count, 2);
        assert!(r.truncated);
    }

    #[test]
    fn empty_input_errors() {
        assert!(matches!(sample(b"", 25, false), Err(SampleError::Empty)));
    }
}
