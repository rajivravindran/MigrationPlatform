//! CSV sampler. Auto-detects delimiter (`,` `;` `\t` `|`), assumes the first
//! line is a header row (matches the rest of the platform's CSV connector
//! convention), and returns at most `row_cap` rows.

use std::io::Cursor;

use serde_json::{json, Map, Value};

use super::type_infer::infer_column_type;
use super::{SampleColumn, SampleError, SampleResult};

/// Sample a CSV byte slice. The slice is expected to be at most
/// `super::DEFAULT_BYTE_CAP` bytes; if the original file was larger, callers
/// should pass `truncated_input = true` so we can flag the result accordingly.
pub fn sample(
    bytes: &[u8],
    row_cap: usize,
    truncated_input: bool,
) -> Result<SampleResult, SampleError> {
    if bytes.is_empty() {
        return Err(SampleError::Empty);
    }

    let delim = detect_delimiter(bytes);
    let mut reader = ::csv::ReaderBuilder::new()
        .delimiter(delim)
        .has_headers(true)
        .flexible(true)
        .from_reader(Cursor::new(bytes));

    let headers: Vec<String> = reader
        .headers()
        .map_err(|e| SampleError::Invalid {
            format: "csv",
            message: format!("reading header: {e}"),
        })?
        .iter()
        .map(str::to_string)
        .collect();
    if headers.is_empty() {
        return Err(SampleError::Invalid {
            format: "csv",
            message: "no header row".into(),
        });
    }

    // Per-column buffers of the raw string values we'll feed into type
    // inference.
    let mut raw_columns: Vec<Vec<String>> = vec![Vec::with_capacity(row_cap); headers.len()];
    let mut nullable = vec![false; headers.len()];
    let mut rows: Vec<Map<String, Value>> = Vec::with_capacity(row_cap);
    let mut truncated = truncated_input;

    for record in reader.records().take(row_cap + 1) {
        if rows.len() == row_cap {
            // We were able to read one more record beyond the cap, so the
            // sample is definitely truncated relative to the file.
            truncated = true;
            break;
        }
        let record = match record {
            Ok(r) => r,
            // If the very last partial line is malformed because we cut the
            // input mid-row, treat that as expected truncation rather than a
            // hard failure -- otherwise the sampler would reject huge but
            // otherwise-valid CSVs.
            Err(e) if truncated_input => {
                tracing::debug!(error = %e, "csv: ignoring trailing partial row");
                truncated = true;
                break;
            }
            Err(e) => {
                return Err(SampleError::Invalid {
                    format: "csv",
                    message: format!("row {}: {e}", rows.len() + 1),
                });
            }
        };

        let mut obj = Map::with_capacity(headers.len());
        for (i, h) in headers.iter().enumerate() {
            let cell = record.get(i).unwrap_or("");
            if cell.is_empty() {
                nullable[i] = true;
                obj.insert(h.clone(), Value::Null);
            } else {
                obj.insert(h.clone(), json!(cell));
            }
            raw_columns[i].push(cell.to_string());
        }
        rows.push(obj);
    }

    let columns: Vec<SampleColumn> = headers
        .iter()
        .enumerate()
        .map(|(i, name)| SampleColumn {
            name: name.clone(),
            r#type: infer_column_type(raw_columns[i].iter().map(String::as_str)),
            nullable: nullable[i],
        })
        .collect();

    Ok(SampleResult {
        format: "csv",
        row_count: rows.len(),
        rows,
        columns,
        truncated,
        bytes_read: bytes.len(),
    })
}

/// Pick a delimiter by counting candidates on the first non-empty line.
/// Comma wins ties so behaviour is intuitive on plain CSV.
fn detect_delimiter(bytes: &[u8]) -> u8 {
    let first_line = bytes
        .split(|b| *b == b'\n')
        .find(|line| !line.is_empty())
        .unwrap_or(&[]);
    let candidates = [b',', b';', b'\t', b'|'];
    let mut best = (b',', -1i32);
    for c in candidates {
        let count = first_line.iter().filter(|b| **b == c).count() as i32;
        if count > best.1 {
            best = (c, count);
        }
    }
    best.0
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn samples_basic_csv_with_inferred_types() {
        let csv =
            "id,name,age,active,joined\n1,Ada,37,true,2024-01-01\n2,Bob,42,false,2024-02-15\n";
        let r = sample(csv.as_bytes(), 25, false).unwrap();
        assert_eq!(r.row_count, 2);
        assert!(!r.truncated);
        let by_name: std::collections::HashMap<_, _> = r
            .columns
            .iter()
            .map(|c| (c.name.as_str(), c.r#type))
            .collect();
        assert_eq!(by_name["id"], "integer");
        assert_eq!(by_name["name"], "string");
        assert_eq!(by_name["age"], "integer");
        assert_eq!(by_name["active"], "boolean");
        assert_eq!(by_name["joined"], "datetime");
    }

    #[test]
    fn detects_semicolon_delimiter() {
        let csv = "id;name\n1;Ada\n";
        let r = sample(csv.as_bytes(), 25, false).unwrap();
        assert_eq!(r.columns.len(), 2);
        assert_eq!(r.columns[0].name, "id");
    }

    #[test]
    fn marks_nullable_columns() {
        let csv = "id,email\n1,ada@example.com\n2,\n";
        let r = sample(csv.as_bytes(), 25, false).unwrap();
        let email = r.columns.iter().find(|c| c.name == "email").unwrap();
        assert!(email.nullable);
    }

    #[test]
    fn caps_rows_and_marks_truncated() {
        let mut csv = String::from("id\n");
        for i in 0..50 {
            csv.push_str(&format!("{i}\n"));
        }
        let r = sample(csv.as_bytes(), 10, false).unwrap();
        assert_eq!(r.row_count, 10);
        assert!(r.truncated);
    }

    #[test]
    fn empty_input_errors() {
        assert!(matches!(sample(b"", 25, false), Err(SampleError::Empty)));
    }
}
