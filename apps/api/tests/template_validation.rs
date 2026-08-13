//! Unit-style test for the shared rule-template JSON Schema. Ensures the
//! golden fixture validates and common malformed templates are rejected.

use jsonschema::JSONSchema;
use serde_json::{json, Value};

const SCHEMA: &str = include_str!("../../../packages/rule-schema/schema/rule-template.schema.json");
const GOLDEN: &str = include_str!("../../../packages/rule-schema/fixtures/golden.json");

fn compiled() -> JSONSchema {
    let v: Value = serde_json::from_str(SCHEMA).expect("schema parse");
    JSONSchema::options()
        .with_draft(jsonschema::Draft::Draft202012)
        .compile(&v)
        .expect("compile")
}

#[test]
fn golden_validates() {
    let s = compiled();
    let g: Value = serde_json::from_str(GOLDEN).unwrap();
    let res = s.validate(&g);
    if let Err(errors) = res {
        let msgs: Vec<String> = errors
            .map(|e| format!("{}: {}", e.instance_path, e))
            .collect();
        panic!("golden fixture failed validation: {:?}", msgs);
    }
}

#[test]
fn rejects_missing_destination() {
    let s = compiled();
    let bad = json!({
        "id": "rt_bad",
        "version": 1,
        "name": "missing dest",
        "source": { "type": "csv", "schema": [] },
        "mapping": { "payload": {} }
    });
    assert!(s.validate(&bad).is_err());
}

#[test]
fn rejects_unknown_source_type() {
    let s = compiled();
    let bad = json!({
        "id": "rt_bad",
        "version": 1,
        "name": "unknown src",
        "source": { "type": "quantum", "schema": [] },
        "mapping": { "payload": {} },
        "destination": { "type": "http", "method": "POST", "url": "https://x" }
    });
    assert!(s.validate(&bad).is_err());
}
