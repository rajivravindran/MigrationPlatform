# Input file sampling

The Designer UI lets you upload a sample input file and have the column palette
auto-derived. Sampling runs **server-side** so the browser never has to load a
multi-gigabyte file.

## Endpoint

```
POST /files/sample
Authorization: Bearer <jwt>
Content-Type:  application/json

{
  "bucket":     "migration",          // optional; defaults to the configured upload bucket
  "key":        "uploads/ab/abcd...", // required; key returned by POST /files
  "format":     "csv",                // optional; sniffed from key suffix when omitted
  "rows":       25,                   // optional; clamped to [1, 200], default 25
  "record_tag": "record"              // XML only; element name that delimits one row
}
```

`format` accepts `csv`, `json`, `ndjson` (alias `jsonl`), or `xml`. When omitted
we fall back to the file extension (`.csv|.tsv` -> csv, `.json` -> json,
`.ndjson|.jsonl` -> ndjson, `.xml` -> xml). Pass it explicitly for files with
exotic suffixes.

### Response

```json
{
  "format":     "csv",
  "row_count":  25,
  "truncated":  false,
  "bytes_read": 4137,
  "columns": [
    { "name": "id",     "type": "integer",  "nullable": false },
    { "name": "email",  "type": "string",   "nullable": true  },
    { "name": "joined", "type": "datetime", "nullable": false }
  ],
  "rows": [
    { "id": 1, "email": "ada@example.com", "joined": "2024-01-01" },
    ...
  ],
  "source": { "bucket": "migration", "key": "uploads/ab/abcd..." }
}
```

* `columns[].type` is one of `string | integer | number | boolean | datetime`.
* `nullable` is `true` if at least one sampled value for that column was empty.
* `truncated` is `true` when the source file was larger than the 8 MiB sample
  window or had more than `rows` records. The schema is still trustworthy --
  it's inferred from whatever fit -- but the user should be warned that columns
  appearing only later in the file won't be present.

### Errors

| Status | Code                | When                                           |
| ------ | ------------------- | ---------------------------------------------- |
| 400    | `bad_request`       | missing `key`, unknown `format`, etc.          |
| 401    | `unauthorized`      | no/invalid JWT                                 |
| 403    | `forbidden`         | role lacks read access                         |
| 422    | `validation_failed` | file is empty or structurally invalid          |
| 502    | `external_error`    | object storage (MinIO/S3) read failed          |

## Limits

| Knob                 | Default | Hard ceiling |
| -------------------- | ------- | ------------ |
| Rows returned        | 25      | 200          |
| Bytes read from S3   | 8 MiB   | 8 MiB        |

We deliberately keep these tight: the goal is "give me a column palette in
under a second", not "load the whole file".

## Type inference rules

Inference is intentionally strict and deterministic -- a single non-conforming
value collapses a column to `string`. This keeps the user safe from the
classic CSV pitfall ("the first 25 rows look like ints, the 26th is `N/A`,
now everything is broken").

| Inferred type | Rule                                                           |
| ------------- | -------------------------------------------------------------- |
| `boolean`     | every value matches `true|false|0|1` (case-insensitive)        |
| `integer`     | every value parses as `i64`                                    |
| `number`      | every value parses as `f64` (and at least one isn't an int)    |
| `datetime`    | every value parses as RFC3339 or `YYYY-MM-DD`                  |
| `string`      | anything else                                                  |

`yes/no/y/n/t/f` are **not** treated as booleans -- real CSVs use them as
free-text flags too often for that to be safe.

## XML specifics

The XML sampler mirrors the orchestrator's XML connector
(`apps/orchestrator-go/internal/connectors/xml.go`) so the columns you see in
the Designer match exactly what the connector will emit at run time:

* Each `<recordTag>` element is one row.
* Element children appear under their tag name; if a child has only text
  content, the value is hoisted to a string.
* Attributes appear under `@name` keys.
* Repeated children collapse to the *first* occurrence in the preview (and the
  column type is held at `string`) so the dry-run table stays flat.

Pass a custom `record_tag` to override the default of `record`.

## Designer flow

In the Designer (`/templates/[id]`), click **Sample input file** in the
toolbar. The browser:

1. Uploads the file to MinIO via `POST /files` (existing endpoint).
2. Calls `POST /files/sample` with the returned `{bucket, key}`.
3. If you already have source fields on the canvas, prompts you to confirm
   "replace N existing source fields with M columns from `<filename>`?".
4. Drops any mapping edges that pointed at fields that are no longer present.
5. Pre-fills the **Dry-run rows** buffer with the first 5 sampled rows so you
   can immediately hit **Dry run** and see real, file-shaped output.
6. Shows a small badge above the canvas reporting the file name, row count,
   and whether the sample was truncated.

Sampling never mutates the saved template -- it just stages columns and
preview rows in the editor. You still have to click **Save version**.
