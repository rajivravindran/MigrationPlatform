# Connectors

Connectors decouple the orchestrator from source specifics. They all
implement the Go interface:

```go
type SourceConnector interface {
    Open(ctx context.Context, ref SourceRef, offset Offset) (Iter, error)
}
type Iter interface {
    Next(ctx context.Context) (Row, Offset, bool, error)
    Close() error
}
```

The platform ships six first-class implementations.

## CSV

- Streams rows without buffering the whole file.
- Configurable delimiter, quote char, header toggle.
- Resume: byte-offset + record index; partial uploads continue from the
  last flushed boundary.

`source_ref` (upload):
```json
"sha256:ae3f…:orders.csv"
```

`rule_template.source`:
```json
{ "type":"csv",
  "schema":[{ "name":"email","type":"string"}, ...],
  "options": { "header": true, "delimiter": "," } }
```

## JSON

- Accepts a top-level JSON **array**; anything else errors with
  `connector.json.expected_array`.
- Row numbering is array index.
- Resume: array index.

## XML

- Streaming record parser via `encoding/xml`; user specifies the record
  element name in `options.record_tag` (e.g. `"Order"`).
- Attributes are exposed as `@attr_name`; inner text lives under the
  element's tag.
- Resume: record index.

## Salesforce

- OAuth2 Web Server flow; refresh token stored in `secrets` (AES-GCM).
- Uses Bulk API 2.0 for queries > 2k rows; REST otherwise.
- Incremental mode: tracks `SystemModstamp` (or template-specified field)
  in `connectors.config_json.cursor`.
- Rate limiting: respects Salesforce daily API limits; degrades gracefully
  via retry-after.

Config:
```json
{
  "instance_url": "https://acme.my.salesforce.com",
  "client_id": "<consumer-key>",
  "username": "svc@acme.com",
  "soql": "SELECT Id, Email, Country, Amount FROM Account WHERE SystemModstamp > {{ cursor }}",
  "cursor_field": "SystemModstamp"
}
```

Bootstrap:
1. Create the connector via API or UI.
2. `POST /connectors/{id}/oauth/salesforce/start` → returns a `login_url`.
3. User authorises; the callback stores the refresh token.

## Watched prefix (MinIO/S3)

- Lists a bucket prefix matching a glob (basename or full-key `path.Match`).
- Used with a **Schedule**: each fire runs `WatchPrefixWorkflow`, which starts
  **one MigrationWorkflow / job per newly seen plain file**, or **one
  BatchWorkflow / batch per archive** (`.tar.gz` / `.tgz` / `.zip` with root
  `manifest.json`). Idempotency is keyed by `key+etag`.
- Configurable sort (`lexical` or `mtime`) and glob (`*.csv`, `*`, `*.tar.gz`, …).
- Cursor: `config_json.cursor.seen` map of object key → etag.

Config:
```json
{
  "bucket":"migration",
  "prefix":"incoming/",
  "glob":"*",
  "sort":"mtime"
}
```

### Batch archives (P2 + P4a)

Archive at the watched prefix must contain root-level `manifest.json`:

```json
{
  "version": 1,
  "batchId": "optional",
  "onStageFailure": "stop",
  "stages": [
    { "id": "customers", "file": "customers.csv", "templateKey": "customer-upsert" },
    { "id": "orders", "file": "orders.csv", "templateKey": "order-upsert" }
  ]
}
```

**Scheduling**

- If **no** stage has `dependsOn`: stages run in **array order** (sequential; P2).
- If **any** stage has `dependsOn: string[]` (stage ids): `BatchWorkflow` runs a
  **DAG** (P4a). Stages with satisfied dependencies run in parallel as child
  `MigrationWorkflow`s. Unknown deps or cycles fail validation → package is
  quarantined.

```json
{
  "version": 1,
  "onStageFailure": "continue",
  "stages": [
    { "id": "customers", "file": "customers.csv", "templateKey": "customer-upsert" },
    { "id": "products", "file": "products.csv", "templateKey": "product-upsert" },
    { "id": "orders", "file": "orders.csv", "templateKey": "order-upsert",
      "dependsOn": ["customers", "products"] }
  ]
}
```

`onStageFailure` (`stop` \| `continue`, batch default or per-stage): after a
failure, `stop` skips all not-yet-started stages; `continue` keeps scheduling
siblings whose deps still succeed (dependents of a failed stage are skipped).

`templateKey` resolves to the latest **published** template in the org.
Hard caps (env-overridable): compressed 256 MiB, uncompressed 1 GiB, 1000 entries;
path traversal / symlinks rejected. Bad packages are copied to
`{parent}/failed/yyyy/mm/dd/` and marked `quarantined`. See **Batches** UI /
`GET /batches/:id`.

**Deferred (P4b):** S3/MinIO object-created event triggers (poll-based watch remains).

- Paginated GET with a JSONPath expression to extract the records array.
- Supports cursor or page-based pagination.

Config:
```json
{
  "base_url":"https://api.example.com",
  "request": { "method":"GET", "path":"/v1/customers" },
  "pagination": { "strategy":"cursor",
                   "next_field":"$.next",
                   "query_param":"cursor" },
  "records_path":"$.data"
}
```

## Offsets + resumability

Every connector emits an `Offset` bytes blob alongside each row. The
orchestrator persists the latest offset per shard in Temporal workflow
state (and in `job_rows.ingest_offset` for visibility). On replay,
`Open(ctx, ref, offset)` is called and the connector fast-forwards.

## Writing a new connector

1. Implement the interface in `apps/orchestrator-go/internal/connectors/`.
2. Add a case to the `factory.go` switch.
3. Extend `config_json` validation in the Rust API
   (`apps/api/src/routes/connectors.rs`).
4. Add a golden test under `connectors/<name>_test.go`.
5. If the UI needs a new form, extend `apps/web/app/connectors/page.tsx`.
