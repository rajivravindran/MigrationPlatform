# Handbook sample files

Companion files for [docs/USER_HANDBOOK.md](../../docs/USER_HANDBOOK.md).

| File | Use |
|------|-----|
| `guests.csv` | First job (5 hotel guests) |
| `guests.json` | Same data as a JSON array |
| `guests.xml` | Same data as XML (`<record>` elements) |
| `guests-with-errors.csv` | Partial-failure demo (one bad email row) |
| `templates/guest-httpbin.json` | Ready-to-POST single-step template (`template_key=guest-httpbin`) |
| `templates/guest-two-step.json` | Ready-to-POST two-step chain |
| `templates/property-upsert.json` | Batch stage 1 (`template_key=property-upsert`) |
| `templates/room-type-upsert.json` | Batch stage 2 (`template_key=room-type-upsert`) |
| `batch/` | Zip these three files for a watched-prefix batch |

JSON sources **must** be a top-level array. XML defaults to each `<record>` as one row.
