//! XML sampler. Mirrors the Go orchestrator's XML connector
//! (`apps/orchestrator-go/internal/connectors/xml.go`) so the columns we
//! infer here match exactly what the connector will produce at run time.
//!
//! Each `<recordTag>` element becomes one row. Inside a record:
//!   * Element children appear under their tag name.
//!   * Attributes appear under `@name`.
//!   * Text content of leaf elements is hoisted to the element's value.
//!
//! Repeated children collapse into the *first* occurrence for the preview --
//! we still flag the column type as `string` so the user is never surprised by
//! losing nested structure in the dry-run.

use std::io::BufRead;

use quick_xml::events::Event;
use quick_xml::Reader;
use serde_json::{Map, Value};

use super::type_infer::infer_column_type;
use super::{SampleColumn, SampleError, SampleResult};

pub fn sample(
    bytes: &[u8],
    row_cap: usize,
    record_tag: &str,
    truncated_input: bool,
) -> Result<SampleResult, SampleError> {
    if bytes.is_empty() {
        return Err(SampleError::Empty);
    }
    let tag = if record_tag.is_empty() {
        "record"
    } else {
        record_tag
    };

    let mut reader = Reader::from_reader(std::io::Cursor::new(bytes));
    reader.config_mut().trim_text(true);

    let mut rows: Vec<Map<String, Value>> = Vec::with_capacity(row_cap);
    let mut truncated = truncated_input;
    let mut buf = Vec::new();

    loop {
        if rows.len() >= row_cap {
            // Look ahead one more record so we can decide if the file actually
            // continued past our cap.
            match read_next_record(&mut reader, &mut buf, tag) {
                Ok(Some(_)) => {
                    truncated = true;
                    break;
                }
                Ok(None) => break,
                Err(e) if truncated_input => {
                    tracing::debug!(error = %e, "xml: ignoring trailing parse error on truncated input");
                    truncated = true;
                    break;
                }
                Err(e) => {
                    return Err(SampleError::Invalid {
                        format: "xml",
                        message: e,
                    });
                }
            }
        }
        match read_next_record(&mut reader, &mut buf, tag) {
            Ok(Some(map)) => rows.push(map),
            Ok(None) => break,
            Err(e) if truncated_input => {
                tracing::debug!(error = %e, "xml: ignoring trailing parse error on truncated input");
                truncated = true;
                break;
            }
            Err(e) => {
                return Err(SampleError::Invalid {
                    format: "xml",
                    message: e,
                });
            }
        }
    }

    if rows.is_empty() {
        return Err(SampleError::Invalid {
            format: "xml",
            message: format!("no <{tag}> records found"),
        });
    }

    // Union of keys across rows preserves first-seen order.
    let mut key_order: Vec<String> = Vec::new();
    let mut seen = std::collections::HashSet::new();
    for row in &rows {
        for k in row.keys() {
            if seen.insert(k.clone()) {
                key_order.push(k.clone());
            }
        }
    }

    let mut raw: Vec<Vec<String>> = vec![Vec::with_capacity(rows.len()); key_order.len()];
    let mut nullable = vec![false; key_order.len()];
    for row in &rows {
        for (i, key) in key_order.iter().enumerate() {
            match row.get(key) {
                None | Some(Value::Null) => {
                    nullable[i] = true;
                    raw[i].push(String::new());
                }
                Some(Value::String(s)) => raw[i].push(s.clone()),
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
        format: "xml",
        row_count: rows.len(),
        rows,
        columns,
        truncated,
        bytes_read: bytes.len(),
    })
}

/// Walk the stream until we either hit `<recordTag ...>` or EOF. On a hit,
/// decode that element into a flat-ish map and return it.
fn read_next_record<R: BufRead>(
    reader: &mut Reader<R>,
    buf: &mut Vec<u8>,
    tag: &str,
) -> Result<Option<Map<String, Value>>, String> {
    loop {
        buf.clear();
        match reader.read_event_into(buf).map_err(|e| e.to_string())? {
            Event::Eof => return Ok(None),
            Event::Start(start) if local_name_eq(start.name().as_ref(), tag) => {
                let mut obj = Map::new();
                for attr in start.attributes().with_checks(false).flatten() {
                    let key = format!(
                        "@{}",
                        std::str::from_utf8(attr.key.local_name().as_ref()).unwrap_or("")
                    );
                    let value = attr
                        .decode_and_unescape_value(reader.decoder())
                        .map(|v| v.into_owned())
                        .unwrap_or_default();
                    obj.insert(key, Value::String(value));
                }
                decode_record(reader, &mut obj)?;
                return Ok(Some(obj));
            }
            _ => continue,
        }
    }
}

/// Parse the body of one `<recordTag>` element into `out`, simplifying any
/// child element that has only text content into a `String` value.
fn decode_record<R: BufRead>(
    reader: &mut Reader<R>,
    out: &mut Map<String, Value>,
) -> Result<(), String> {
    let mut buf = Vec::new();
    loop {
        buf.clear();
        match reader
            .read_event_into(&mut buf)
            .map_err(|e| e.to_string())?
        {
            Event::Eof => return Ok(()),
            Event::End(_) => return Ok(()),
            Event::Start(start) => {
                let name = std::str::from_utf8(start.name().as_ref())
                    .map(|s| s.to_string())
                    .unwrap_or_default();
                let local = name.rsplit(':').next().unwrap_or("").to_string();
                let mut child = Map::new();
                for attr in start.attributes().with_checks(false).flatten() {
                    let key = format!(
                        "@{}",
                        std::str::from_utf8(attr.key.local_name().as_ref()).unwrap_or("")
                    );
                    let value = attr
                        .decode_and_unescape_value(reader.decoder())
                        .map(|v| v.into_owned())
                        .unwrap_or_default();
                    child.insert(key, Value::String(value));
                }
                decode_record(reader, &mut child)?;
                let simplified = simplify(child);
                insert_first_only(out, &local, simplified);
            }
            Event::Text(t) => {
                let txt = t.unescape().map_err(|e| e.to_string())?.to_string();
                if !txt.trim().is_empty() {
                    out.insert("$text".into(), Value::String(txt));
                }
            }
            Event::Empty(start) => {
                let name = std::str::from_utf8(start.name().as_ref())
                    .map(|s| s.to_string())
                    .unwrap_or_default();
                let local = name.rsplit(':').next().unwrap_or("").to_string();
                // Empty elements with attributes still surface their attrs.
                let mut child = Map::new();
                for attr in start.attributes().with_checks(false).flatten() {
                    let key = format!(
                        "@{}",
                        std::str::from_utf8(attr.key.local_name().as_ref()).unwrap_or("")
                    );
                    let value = attr
                        .decode_and_unescape_value(reader.decoder())
                        .map(|v| v.into_owned())
                        .unwrap_or_default();
                    child.insert(key, Value::String(value));
                }
                let simplified = if child.is_empty() {
                    Value::Null
                } else {
                    simplify(child)
                };
                insert_first_only(out, &local, simplified);
            }
            _ => continue,
        }
    }
}

fn simplify(m: Map<String, Value>) -> Value {
    if m.len() == 1 {
        if let Some(v) = m.get("$text") {
            return v.clone();
        }
    }
    Value::Object(m)
}

/// First-write-wins so the dry-run preview is stable when the same tag repeats.
/// Type inference will still see only this value, which is acceptable for a
/// 25-row sample.
fn insert_first_only(out: &mut Map<String, Value>, key: &str, value: Value) {
    if !out.contains_key(key) {
        out.insert(key.to_string(), value);
    }
}

fn local_name_eq(qname: &[u8], wanted: &str) -> bool {
    let s = std::str::from_utf8(qname).unwrap_or("");
    let local = s.rsplit(':').next().unwrap_or(s);
    local == wanted
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn samples_basic_records() {
        let body = br#"<root>
            <record><id>1</id><name>Ada</name><active>true</active></record>
            <record><id>2</id><name>Bob</name><active>false</active></record>
        </root>"#;
        let r = sample(body, 25, "record", false).unwrap();
        assert_eq!(r.row_count, 2);
        let by_name: std::collections::HashMap<_, _> = r
            .columns
            .iter()
            .map(|c| (c.name.as_str(), c.r#type))
            .collect();
        assert_eq!(by_name["id"], "integer");
        assert_eq!(by_name["name"], "string");
        assert_eq!(by_name["active"], "boolean");
    }

    #[test]
    fn surfaces_attributes_with_at_prefix() {
        let body = br#"<root><record id="1"><name>Ada</name></record></root>"#;
        let r = sample(body, 25, "record", false).unwrap();
        let names: Vec<_> = r.columns.iter().map(|c| c.name.clone()).collect();
        assert!(names.contains(&"@id".to_string()));
        assert!(names.contains(&"name".to_string()));
    }

    #[test]
    fn caps_rows_and_marks_truncated() {
        let mut body = String::from("<root>");
        for i in 0..50 {
            body.push_str(&format!("<record><id>{i}</id></record>"));
        }
        body.push_str("</root>");
        let r = sample(body.as_bytes(), 5, "record", false).unwrap();
        assert_eq!(r.row_count, 5);
        assert!(r.truncated);
    }

    #[test]
    fn missing_record_tag_errors() {
        let body = br#"<root><thing>x</thing></root>"#;
        let err = sample(body, 25, "record", false).unwrap_err();
        assert!(matches!(err, SampleError::Invalid { format: "xml", .. }));
    }

    #[test]
    fn custom_record_tag_works() {
        let body = br#"<root><user><id>1</id></user></root>"#;
        let r = sample(body, 25, "user", false).unwrap();
        assert_eq!(r.row_count, 1);
    }
}
