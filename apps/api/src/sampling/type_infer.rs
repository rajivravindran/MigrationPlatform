//! Strict type inference shared by every sampler.
//!
//! Inference runs after we have all sampled string values for a column. We
//! keep it deterministic and conservative -- a single non-conforming value
//! collapses the column to `string`. This avoids the classic CSV pitfall of
//! "the first 25 rows look like ints, the 26th is `N/A`, now everything is
//! broken".
//!
//! Booleans use the strict allowlist {`true`, `false`, `0`, `1`} (case-
//! insensitive). We deliberately do **not** treat `yes/no/y/n/t/f` as booleans
//! because real CSVs use them as free-text flags constantly.

use chrono::NaiveDate;

/// Inspect every non-empty value in `values` and return one of
/// `string|integer|number|boolean|datetime`.
pub fn infer_column_type<'a, I: IntoIterator<Item = &'a str>>(values: I) -> &'static str {
    let mut all_bool = true;
    let mut all_int = true;
    let mut all_num = true;
    let mut all_dt = true;
    let mut had_any = false;

    for raw in values {
        let v = raw.trim();
        if v.is_empty() {
            continue;
        }
        had_any = true;

        if all_bool && !is_bool(v) {
            all_bool = false;
        }
        if all_int && v.parse::<i64>().is_err() {
            all_int = false;
        }
        if all_num && v.parse::<f64>().is_err() {
            all_num = false;
        }
        if all_dt && !is_datetime(v) {
            all_dt = false;
        }

        if !(all_bool || all_int || all_num || all_dt) {
            return "string";
        }
    }

    if !had_any {
        return "string";
    }
    if all_bool {
        return "boolean";
    }
    if all_int {
        return "integer";
    }
    if all_num {
        return "number";
    }
    if all_dt {
        return "datetime";
    }
    "string"
}

fn is_bool(v: &str) -> bool {
    matches!(v.to_ascii_lowercase().as_str(), "true" | "false" | "0" | "1")
}

fn is_datetime(v: &str) -> bool {
    if chrono::DateTime::parse_from_rfc3339(v).is_ok() {
        return true;
    }
    NaiveDate::parse_from_str(v, "%Y-%m-%d").is_ok()
}

#[cfg(test)]
mod tests {
    use super::*;

    fn t(values: &[&str]) -> &'static str {
        infer_column_type(values.iter().copied())
    }

    #[test]
    fn empty_sample_is_string() {
        assert_eq!(t(&[]), "string");
        assert_eq!(t(&["", "  ", ""]), "string");
    }

    #[test]
    fn pure_integers() {
        assert_eq!(t(&["1", "2", "-3", "0"]), "integer");
    }

    #[test]
    fn floats_become_number_not_int() {
        assert_eq!(t(&["1", "2.5"]), "number");
    }

    #[test]
    fn booleans_strict_allowlist() {
        assert_eq!(t(&["true", "false", "0", "1"]), "boolean");
        // `yes/no` are NOT booleans -- this is intentional.
        assert_ne!(t(&["yes", "no"]), "boolean");
    }

    #[test]
    fn iso_dates_and_rfc3339() {
        assert_eq!(t(&["2024-01-01", "2024-12-31"]), "datetime");
        assert_eq!(t(&["2024-01-01T00:00:00Z"]), "datetime");
        // Mixed format that isn't recognized -> string.
        assert_eq!(t(&["2024-01-01", "Jan 5 2024"]), "string");
    }

    #[test]
    fn one_bad_value_collapses_to_string() {
        assert_eq!(t(&["1", "2", "N/A"]), "string");
    }

    #[test]
    fn empty_values_are_ignored_for_inference() {
        assert_eq!(t(&["1", "", "2"]), "integer");
    }
}
