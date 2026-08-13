# Real-world GTM and enterprise value — Migration Platform

**Date:** 2026-08-07  
**Scope:** How this self-hosted Migration Platform can reach paying companies, where value accrues, what to build next, and whether SAP (and adjacent enterprise) migrations belong in product scope.  
**Method:** Repo baseline from first-party docs/code; market claims traced to vendor official docs, product pages, SAP Help/Support, Microsoft Learn, Salesforce Developers, HL7, and similar primary sources. Secondary sources are labeled when used.

---

## Executive summary / recommendation

**Position this product as a self-hosted cutover & file-to-API migration orchestrator** — not a general iPaaS and not an SAP S/4HANA migration product.

The shipped MVP already matches a real buyer job: ingest CSV/JSON/XML (and Salesforce), map in a designer, call HTTP destinations with Temporal-backed retries/resumability, watch S3/MinIO and SFTP drops, run ordered/DAG batch archives with manifests, and operate under a phone-home trial license ([README.md](../README.md), [AGENT_HANDOFF.md](../AGENT_HANDOFF.md), [architecture.md](../architecture.md), [install.md](../install.md)). Enterprise iPaaS vendors (Boomi, MuleSoft, Informatica, SAP Integration Suite) sell broad, continuous integration platforms with connector libraries and consumption pricing ([Boomi pricing](https://boomi.com/pricing/), [MuleSoft Anypoint pricing](https://www.mulesoft.com/anypoint-pricing), [Informatica IPU pricing](https://www.informatica.com/products/cloud-integration/pricing.html), [SAP Integration Suite](https://www.sap.com/products/technology-platform/integration-suite.html)). Cloud ETL (Azure Data Factory, AWS Glue) sells usage-metered pipelines inside a hyperscaler bill ([Microsoft Learn — ADF costs](https://learn.microsoft.com/en-us/azure/data-factory/plan-manage-costs), [AWS Glue pricing](https://aws.amazon.com/glue/pricing/)).

**SAP recommendation: option (a) — stay a general API/file orchestrator that can call SAP OData/HTTP APIs; partner with SAP tools for true S/4 migrations; do not become an SAP specialist or rebuild Migration Cockpit.** SAP’s own Migration Cockpit is license-included in S/4HANA and owns staging-table / direct-transfer migration objects ([SAP S/4HANA Installation Guide excerpt](https://help.sap.com/doc/6b11678926d3409bbfea8897cb34d10f/2025/en-US/INST_OP2025.pdf)). Selective Data Transition is a partner/SAP-engagement DB-level path ([SAP SDTE](https://support.sap.com/en/offerings-programs/support-services/data-management-landscape-transformation/selective-data-transition-engagement.html)). Competing with those is high cost and low win rate. Calling published OData/REST APIs (catalogued on [api.sap.com](https://api.sap.com/)) fits this repo’s HTTP destination model.

**First paying customers** are mid-market / upper-mid IT ops and SI project teams running **CRM/ERP cutovers and recurring watched-file loads** who hate Data Loader + scripts + spreadsheet runbooks, cannot justify full iPaaS for a finite migration program, and need auditability + retries on-prem or in their VPC.

---

## Current product baseline (from repo)

### What exists today (shipped MVP)

| Capability | Evidence |
|------------|----------|
| File ingest CSV/JSON/XML; Salesforce source (Bulk API 2.0 for large queries) | [connectors.md](../connectors.md), [README.md](../README.md) |
| Rule templates: preprocess + payload map + HTTP destination; designer UI | [architecture.md](../architecture.md) |
| Temporal job orchestration, shards, retry/backoff, pause/cancel, row idempotency | [architecture.md](../architecture.md) |
| Schedules (cron, overlap policies, incremental connectors) | [scheduling.md](../scheduling.md) |
| Watched MinIO/S3 prefix → per-file jobs or batch archives | [AGENT_HANDOFF.md](../AGENT_HANDOFF.md) P1–P2 |
| Watched SFTP (poll, host-key pin, stage to MinIO) | [AGENT_HANDOFF.md](../AGENT_HANDOFF.md) P3 |
| Archive + `manifest.json` stages; optional `dependsOn` DAG | [AGENT_HANDOFF.md](../AGENT_HANDOFF.md) P2/P4 |
| RBAC roles viewer/operator/editor/admin; JWT; AES-GCM secrets; audit_log | [security.md](../security.md) |
| OTEL → Prometheus/Loki/Tempo/Grafana; alerts | [observability.md](../observability.md) |
| Helm/Compose; 10-day phone-home trial; commercial license sign | [install.md](../install.md), [AGENT_HANDOFF.md](../AGENT_HANDOFF.md) P5 |
| Claimed scale target: 10M-row class | [README.md](../README.md) |

### Explicit gaps / non-goals in current codebase docs

| Gap | Notes |
|-----|--------|
| Not production-unconditional | Release gates: license enforce tests, Helm RWX license PVC, SFTP bounds, license server HA/KMS not done ([AGENT_HANDOFF.md](../AGENT_HANDOFF.md)) |
| No SSO/SAML/OIDC in docs | Auth is JWT + password ([security.md](../security.md)); grep of `docs/` found no SSO/SAML |
| No connector marketplace | Six connectors documented; local-folder & richer connector UI listed as follow-ups |
| No DLQ product concept | Row retry exists; no first-class dead-letter queue product surface in docs |
| No air-gap trial path | Phone-home trial; commercial license can be file-mounted ([install.md](../install.md)) — air-gapped *trial* needs offline issuance |
| Events deferred | S3 object-created push deferred as P4b |
| License server MVP | Single-instance SQLite; not HA ([AGENT_HANDOFF.md](../AGENT_HANDOFF.md)) |
| Stripe / feature SKUs | Planned P5.2, not shipped |

**Do not claim:** native IDoc/RFC/BAPI adapters, Migration Cockpit parity, EDI X12/EDIFACT engines, HL7 MLLP, Workday/NetSuite/Dynamics first-party connectors, or SaaS multi-tenant hosting — none are in the current docs as shipped features.

---

## Buyer personas & value props

### Who buys / uses tools in this category

| Persona | Job to be done | Why this platform fits vs alternatives |
|---------|----------------|----------------------------------------|
| **IT ops / platform eng** | Run watched drops and scheduled loads in VPC without owning a full iPaaS | Self-hosted compose/Helm + Temporal durability; already their ops model |
| **Integration / data migration team** | Cutover CRM/ERP data with mapping reuse and auditable retries | Designer + templates + audit_log + job console |
| **SI / consulting partner** | Deliver migration projects faster; reuse packs across clients | Batch manifests + DAG stages + template library; license-bound installs |
| **Business program (cutover PM)** | Predictable go-live; evidence for auditors | Batches UI, stage status, Grafana alerts, audit export path in docs |

### How companies find value (mapped to this product)

| Value | Mechanism in product | Buyer metric |
|-------|----------------------|--------------|
| **Time-to-cutover** | Reusable templates; watched arrival; batch packages | Weeks of scripting → days of configuration |
| **Auditability** | `audit_log`, job/row outcomes, batch stage history | Pass SOX/security review for “who ran what” |
| **Retries & resumability** | Temporal + idempotency keys + ingest offsets | Survive mid-cutover failures without re-load chaos |
| **Mapping reuse** | Published rule templates / `template_key` in manifests | Same mapping across dry-run → prod |
| **Watched drops** | S3/MinIO + SFTP watch workflows | Legacy “drop a file on the feed” still works |
| **Batch hierarchies** | Ordered stages + DAG `dependsOn` | Master-then-child loads without manual sequencing |
| **Cost control vs iPaaS** | Self-hosted; no Mule Message / IPU / Boomi Message meter for runtime | Capex/opex on customer infra; license seats/features |

**Competitive contrast (primary):** Salesforce Data Loader is a free client for CSV ↔ Salesforce with Bulk API 2.0 up to 150M records, but it is not a multi-system orchestrator with watched drops and multi-stage batches ([Salesforce Data Loader](https://developer.salesforce.com/tools/data-loader)). Bulk API 2.0 itself is async REST for large sets; Salesforce documents no SLA on completion time ([Bulk API intro](https://developer.salesforce.com/docs/atlas.en-us.api_asynch.meta/api_asynch/asynch_api_intro.htm) — page fetch timed out in this research pass; capability confirmed via Data Loader docs citing Bulk API 2.0).

---

## Category & competitive context (cited)

### Category framing

This product sits at the intersection of:

1. **Cutover / migration tooling** (project-shaped, finite or episodic) — closest narrative for GTM.  
2. **Lightweight self-hosted iPaaS / API orchestration** — continuous sync is possible via schedules but not the differentiator vs Boomi/MuleSoft.  
3. **File-drop ETL ops** — overlaps ADF/Glue when cloud-native; differentiates when customer wants **VPC/on-prem control** and **HTTP business APIs** rather than warehouse loads.

### Vendor landscape (primary sources)

| Vendor / product | What they are (vendor’s words) | Packaging signal | Implication for us |
|------------------|--------------------------------|------------------|--------------------|
| **Boomi** | Enterprise platform: Integration editions + APIM + Data Hub + Data Integration; trial + PAYG | Annual subscription editions; PAYG **$99/mo + $0.05/Message** ([Boomi pricing](https://boomi.com/pricing/), [Boomi PAYG FAQ](https://help.boomi.com/docs/Atomsphere/Platform/atm-Boomi_Pay_As_You_Go_FAQ_88ebfd22-b959-43ee-828b-0c4f99a5a8ef)); also [AWS Marketplace](https://aws.amazon.com/marketplace/pp/prodview-kuv4iv5qyhc3g) | Compete on **narrower job + self-host + simpler price**; lose on connector breadth |
| **MuleSoft Anypoint** | Design/deploy APIs & integrations; Starter vs Advanced; hybrid deploy on Advanced | Subscription by **Mule Flow + Mule Message** capacity; contact for pricing ([mulesoft.com/anypoint-pricing](https://www.mulesoft.com/anypoint-pricing)) | Enterprise RFPs often already own MuleSoft — sell **migration program overlay** or **air-gapped alternative**, not head-to-head iPaaS |
| **Informatica IDMC** | Consumption via **IPUs** across cloud services | IPU / Flex IPU; volume tier; quote-based ([Informatica pricing](https://www.informatica.com/products/cloud-integration/pricing.html); scalars in [Informatica docs](https://docs.informatica.com/cloud-common-services/administrator/current-version/organization-administration/metering/informatica-processing-unit-metrics/ipu-scalars.html)) | Same: too heavy for mid-market cutovers |
| **Qlik Talend Cloud** | Data movement + integration tiers Starter→Enterprise | Capacity: **Data Moved, Job executions, Job duration**; SAP/mainframe real-time only on Enterprise ([Qlik Help](https://help.qlik.com/en-US/cloud-services/Subsystems/Hub/Content/Sense_Hub/Admin/subscription-options-QTC.htm)) | Validates “SAP/mainframe = premium SKU” — we should not chase that SKU early |
| **Azure Data Factory** | Serverless cloud ETL; pay for orchestration/DIU/vCore | Consumption meters ([Microsoft Learn](https://learn.microsoft.com/en-us/azure/data-factory/plan-manage-costs)) | Win when customer refuses Azure lock-in or needs HTTP cutover UX |
| **AWS Glue** | Serverless ETL; DPU-hour | e.g. ETL billed per DPU-hour ([AWS Glue pricing](https://aws.amazon.com/glue/pricing/)) | Same as ADF for lakehouse; weak for “POST this mapped row to CRM API” UX |
| **SAP Integration Suite** | iPaaS for SAP + third-party; APIs, events, B2B/EDI, prebuilt content | Cloud subscription / BTP; hybrid via Edge Integration Cell ([sap.com Integration Suite](https://www.sap.com/products/technology-platform/integration-suite.html)) | Default for SAP-centric estates; partner beside it for **non-SAP file→API** legs |
| **SAP S/4 Migration Cockpit** | Transfer master/business data into S/4; **included in S/4 license** | Staging tables (templates / ETL fill) or direct from SAP system ([Installation Guide](https://help.sap.com/doc/6b11678926d3409bbfea8897cb34d10f/2025/en-US/INST_OP2025.pdf); objects list alias [help.sap.com/S4_OP_MO](https://help.sap.com/S4_OP_MO)) | **Do not rebuild**; optionally **feed staging** or call **OData** |
| **Salesforce Data Loader** | CSV import/export client | Free tool in eligible editions ([developer.salesforce.com/tools/data-loader](https://developer.salesforce.com/tools/data-loader)) | Displace for multi-system / watched / batched programs; not for one-off SF-only CSV |

---

## Packaging & GTM options

### How peers sell (patterns from primary pages)

1. **Annual subscription + usage meters** — MuleSoft Flows/Messages; Informatica IPUs; Qlik capacity bands; Boomi edition ladders.  
2. **Low-friction trial → card PAYG** — Boomi 30-day trial then PAYG ([Boomi PAYG FAQ](https://help.boomi.com/docs/Atomsphere/Platform/atm-Boomi_Pay_As_You_Go_FAQ_88ebfd22-b959-43ee-828b-0c4f99a5a8ef)).  
3. **Hyperscaler marketplace** — Boomi PAYG on AWS Marketplace (monthly fee + message meter).  
4. **Edition gates for hard connectors** — Qlik reserves real-time SAP/mainframe for Enterprise.  
5. **Self-hosted / hybrid as premium** — MuleSoft Advanced lists hybrid deployment; Boomi Enterprise lists clustered runtime.

### Recommended packaging for *this* product (opinionated)

Fits existing license model (`trial` | `commercial`, `features`, `max_seats`, fingerprint) in [AGENT_HANDOFF.md](../AGENT_HANDOFF.md):

| SKU | Includes | Meter / limit | Target |
|-----|----------|---------------|--------|
| **Trial** | Full product, 10 days, phone-home | Fingerprint-bound (shipped) | POC |
| **Team** | Core file+HTTP+SFTP/S3 watch, N seats | Seats + max concurrent jobs | Mid-market IT |
| **Project** | Time-boxed commercial license (90–180 days) for SI cutovers | Seats + job/row soft caps | SI channel |
| **Enterprise** | SSO, air-gap license, audit export SLA, support | Seats + installs + support tier | Regulated / air-gapped |
| **Add-on: Destination packs** | Curated template packs (Salesforce Bulk dest, Dataverse, ServiceNow Table API, SAP OData samples) — *not* proprietary protocol engines | Per pack annual | Expand TAM without iPaaS sprawl |

**Avoid early:** message-level metering like Boomi/MuleSoft (operationally hard on self-hosted; customers distrust opaque meters). Prefer **install + seats + feature flags** already sketched in license `features` / `max_seats`.

### Discovery / evaluation paths

| Path | Why it works | What to offer |
|------|--------------|---------------|
| **POC pack** | Peers lead with trial (Boomi 30d; our 10d) | Sample batch archive, SF→HTTP recipe, Grafana dashboard, 2-week success criteria |
| **SI channel** | Cutover projects are SI-delivered; SDTE shows SAP uses partner engagement model | Partner license, template packs, co-sell brief |
| **Marketplace** | Boomi lists on AWS Marketplace for procurement ease | Later: container offer + BYOL license (after P5.2 billing) |
| **Content SEO / docs** | ADF/Glue/Data Loader own “how do I migrate X” searches | Public runbooks: “watched SFTP → Salesforce API”, “CSV cutover with DAG stages” |
| **Replace scripts** | Teams already running Python + cron + Data Loader | Side-by-side: same CSV, our job console + audit |

---

## Feature roadmap (P0 / P1 / P2) mapped to value

### P0 — must for first paying customers

| Feature | Buyer value | Codebase fit |
|---------|-------------|--------------|
| **SSO (OIDC/SAML)** | Security questionnaire pass | New IdP middleware beside JWT; roles already exist ([security.md](../security.md)) |
| **Production license path hardened** | Trust to buy | Close AGENT_HANDOFF gates: enforce tests, Helm license PVC, offline commercial bind (P5.1) |
| **Audit export UX + retention controls** | Compliance evidence | `migration-admin audit export` exists in docs — productize in UI/API |
| **Validation / dry-run gate** | Cutover confidence | Extend simulate-before-POST; sample N rows; fail job if error rate > threshold (alerts already exist) |
| **Failed-row DLQ + export** | Ops recovery | Build on `Retry failed` + job_rows; export CSV of failures |
| **Connector config UI by kind + docs pack** | Time-to-first-job | Listed follow-up in AGENT_HANDOFF; unblocks SI demos |
| **Support runbook + SLA language** | Procurement | Ops docs exist; package severity/response for Enterprise SKU |

### P1 — expand TAM

| Feature | Buyer value | Codebase fit |
|---------|-------------|--------------|
| **HTTP destination auth presets** (OAuth2 client credentials, API key vault) | Call more SaaS APIs safely | Secrets AES-GCM already; extend destination step |
| **Salesforce destination** (Bulk API 2.0 write) | Close SF migration loops | Source connector exists; mirror as destination |
| **Dataverse / Dynamics Web API** (`CreateMultiple` / `$batch`) | Mid-market ERP/CRM cutovers | Pure HTTP — [Microsoft Learn bulk ops](https://learn.microsoft.com/en-us/power-apps/developer/data-platform/bulk-operations) |
| **ServiceNow Table API templates** | ITSM migrations | HTTP CRUD — [ServiceNow Table API](https://www.servicenow.com/docs/r/api-reference/rest-apis/c_TableAPI.html) |
| **SAP OData destination pack** (samples from Business Accelerator Hub) | SAP-adjacent without Cockpit | HTTP OData — [api.sap.com](https://api.sap.com/), [APIs for S/4HANA PDF](https://help.sap.com/doc/57063b559ae04c86a281dc626f474091/2008.500/en-US/APIsforSAPS4HANA.pdf) |
| **S3 event arrival (P4b)** | Lower poll latency | Documented deferred work |
| **Air-gapped install + offline license** | Gov / bank | Commercial `LICENSE_FILE` already; harden docs + signing process |
| **Multi-env promotion** (dev→prod template promote) | SI delivery | Versioned templates already |

### P2 — differentiating

| Feature | Buyer value | Codebase fit |
|---------|-------------|--------------|
| **Partner template marketplace** (signed packs) | Channel scale | Manifest `templateKey` + license feature flags |
| **Contract / schema validation gates** (JSON Schema, OpenAPI response asserts) | Quality at cutover | Transform worker + publish hooks |
| **Cost/usage telemetry for SKUs** | FinOps for vendor & customer | Metrics already; add licensed meters |
| **EDI / HL7 as *file* packs** (X12/EDI flat → JSON → HTTP; FHIR JSON REST) | Regulated verticals | Prefer file+HTTP over MLLP/X12 engines initially ([FHIR R4 REST](https://www.hl7.org/fhir/R4/http.html)) |
| **Stripe portal (P5.2)** | Self-serve Team SKU | Planned |

---

## SAP deep-dive & recommendation

### What “SAP migration” means in practice

| Approach | Meaning (primary) | Tooling owners |
|----------|-------------------|----------------|
| **System conversion (brownfield)** | Technical upgrade ECC→S/4 with data mapping in-place | SAP conversion toolchain; not file/API ETL |
| **New implementation (greenfield)** | New S/4; load master/transactional data | **Migration Cockpit** (staging / direct transfer); included in S/4 license ([Installation Guide](https://help.sap.com/doc/6b11678926d3409bbfea8897cb34d10f/2025/en-US/INST_OP2025.pdf)) |
| **Selective Data Transition (SDT)** | Mix of both; selective history; often DB-level; phased company codes; near-zero downtime themes | SAP + SDTE partners (cbs, Natuvion, SNP, SAP) ([SAP SDTE](https://support.sap.com/en/offerings-programs/support-services/data-management-landscape-transformation/selective-data-transition-engagement.html)) |

Migration Cockpit explicitly allows filling staging tables via **template files** or **preferred tools (e.g. SAP Data Services)** and notes CSV guidance via SAP Note 3210687 ([Installation Guide](https://help.sap.com/doc/6b11678926d3409bbfea8897cb34d10f/2025/en-US/INST_OP2025.pdf)). That is a **complementary** wedge (prepare/stage files), not a replacement for Cockpit’s object semantics, simulation, and SAP-supported migration objects ([help.sap.com/S4_OP_MO](https://help.sap.com/S4_OP_MO)).

### Integration surfaces (fit to this architecture)

| Surface | Nature | Fit |
|---------|--------|-----|
| **OData / REST APIs** | HTTP; catalogued on Business Accelerator Hub | **Strong** — same as current destinations ([APIs for S/4HANA](https://help.sap.com/doc/57063b559ae04c86a281dc626f474091/2008.500/en-US/APIsforSAPS4HANA.pdf); [api.sap.com tutorial](https://developers.sap.com/tutorials/hcp-abh-getting-started.html)) |
| **File / staging CSV/XML** | Files into Cockpit staging | **Strong** — watched drop + transforms → staging location |
| **IDoc / RFC / BAPI** | SAP-proprietary protocols | **Weak** — needs adapters SAP Integration Suite already sells; out of scope early |
| **CDS** | ABAP data model / exposure path to OData | Consume via **published OData**, don’t “do CDS” |
| **BTP Integration Suite** | Full iPaaS | **Partner / coexist**; don’t clone |

### Options & trade-offs

| Option | Pros | Cons | Verdict |
|--------|------|------|---------|
| **(a) General orchestrator that calls SAP OData/APIs** | Matches stack; low R&D; honest positioning | Won’t win “S/4 migration” RFPs alone | **Recommended** |
| **(b) Become SAP specialist** | Huge budgets | Rebuild Cockpit/SDT; IDoc/RFC; partner politics; years | **Reject** |
| **(c) Partner with SAP tools** | Cockpit for objects; us for non-SAP legs & file prep | Requires SI relationships | **Do with (a)** |

### Clear recommendation

**Ship (a)+(c):** market as *“file and API migration for systems around SAP — and OData loads where APIs exist”*; publish a pack that fills Cockpit staging or posts to S/4 OData; never claim Migration Cockpit or SDT replacement. Qlik’s packaging (SAP real-time only on Enterprise) shows even large vendors treat deep SAP as a late/expensive tier ([Qlik Help](https://help.qlik.com/en-US/cloud-services/Subsystems/Hub/Content/Sense_Hub/Admin/subscription-options-QTC.htm)).

---

## Adjacent systems prioritization

Given **HTTP destinations + CSV/JSON/XML + Temporal + existing Salesforce source**:

| Priority | System | Why leverage is high | Primary API evidence |
|----------|--------|----------------------|----------------------|
| **1** | **Salesforce** | Source already shipped; Data Loader gap is orchestration | [Data Loader](https://developer.salesforce.com/tools/data-loader); Bulk API 2.0 |
| **2** | **ServiceNow** | Table API is straightforward REST CRUD | [Table API](https://www.servicenow.com/docs/r/api-reference/rest-apis/c_TableAPI.html) |
| **3** | **Dynamics / Dataverse** | Bulk/Web API HTTP; common mid-market cutovers | [Bulk operations](https://learn.microsoft.com/en-us/power-apps/developer/data-platform/bulk-operations) |
| **4** | **SAP OData (not Cockpit)** | Large estate adjacency; HTTP-only wedge | [api.sap.com](https://api.sap.com/), [APIs PDF](https://help.sap.com/doc/57063b559ae04c86a281dc626f474091/2008.500/en-US/APIsforSAPS4HANA.pdf) |
| **5** | **NetSuite REST** | REST preferred over SOAP for new integrations | [Oracle NetSuite REST upgrade guide](https://docs.oracle.com/en/cloud/saas/netsuite/ns-online-help/article_2095713813.html) |
| **6** | **Workday** | REST + SOAP WWS directories exist; auth/BP complexity higher | [Workday REST directory](https://community.workday.com/sites/default/files/file-hosting/restapi/), [WWS directory](https://community.workday.com/sites/default/files/file-hosting/productionapi/index.html) |
| **7** | **Mainframe / legacy file feeds** | Already our strength (SFTP/S3 + XML/CSV) | Product connectors docs |
| **8** | **EDI** | High value but needs translation; Integration Suite/Boomi own this | Defer engine; allow file-in JSON-out packs later |
| **9** | **HL7/FHIR** | FHIR is HTTP/JSON REST — good late P2; classic HL7v2/MLLP is not | [FHIR R4 HTTP](https://www.hl7.org/fhir/R4/http.html) |

---

## Recommended product narrative (one paragraph)

**Migration Platform** is the self-hosted control plane for **cutover and watched-file data migration**: map CSV/JSON/XML (and Salesforce) once, drop archives on S3 or SFTP, run ordered or DAG stages, and call business HTTP APIs with retries, row-level outcomes, and audit trails — without buying an enterprise iPaaS or rebuilding SAP’s Migration Cockpit. Teams use it when go-live risk and operational evidence matter more than having every connector on earth.

---

## 12–18 month phased roadmap (architecture-fit)

```
Now ── MVP (shipped): files + SF source + HTTP dest + watch S3/SFTP + batches/DAG + trial license
 │
 ├─ Months 0–3   Harden & sellable: SSO, license gates, audit export UI, DLQ export, dry-run gates,
 │               connector UI, Project/Team SKUs, 3 reference recipes (SF, ServiceNow, SFTP batch)
 │
 ├─ Months 3–9   Destination packs: SF Bulk write, Dataverse, ServiceNow, SAP OData samples;
 │               S3 events; air-gap license; SI partner program; first marketplace BYOL (optional)
 │
 └─ Months 9–18  Differentiating: signed template marketplace, usage telemetry, FHIR JSON pack,
                 optional EDI file maps; Stripe self-serve; still NO IDoc/RFC/Cockpit clone
```

Cost/complexity guardrail: **do not** staff a proprietary SAP protocol stack or selective DB transition engine; attach to Cockpit staging and public APIs instead.

---

## Risks & non-goals

### Risks

| Risk | Mitigation |
|------|------------|
| Mistaken for iPaaS → lose bake-offs on connectors | Narrative discipline; sell cutover + watched files |
| SAP RFP trap | Explicit non-goals; partner brief with Cockpit |
| Enterprise security blockers (SSO, air-gap) | P0 SSO + offline license |
| License server HA / phone-home distrust | Commercial file license; document offline grace (P5.1) |
| Hyperscaler free tools (Glue/ADF/Data Loader) | Win on multi-system HTTP + self-host + batch DAG UX |
| SI expects professional services | Price Project SKU; don’t become unpaid implementer |

### Non-goals (next 18 months)

- Rebuilding **SAP S/4HANA Migration Cockpit** or **SDT** tooling  
- Native **IDoc / RFC / BAPI** adapters  
- Full **EDI trading partner management** (SAP Integration Suite / Boomi territory)  
- Multi-tenant SaaS control plane (stay self-hosted; license phone-home only)  
- Competing as general **API management** platform  

---

## Sources appendix

### First-party (this repo)

- [README.md](../README.md)  
- [docs/AGENT_HANDOFF.md](../AGENT_HANDOFF.md)  
- [docs/architecture.md](../architecture.md)  
- [docs/connectors.md](../connectors.md)  
- [docs/security.md](../security.md)  
- [docs/observability.md](../observability.md)  
- [docs/scheduling.md](../scheduling.md)  
- [docs/install.md](../install.md)  

### Vendor / platform primary

- Boomi pricing: https://boomi.com/pricing/  
- Boomi Pay-As-You-Go FAQ: https://help.boomi.com/docs/Atomsphere/Platform/atm-Boomi_Pay_As_You_Go_FAQ_88ebfd22-b959-43ee-828b-0c4f99a5a8ef  
- Boomi on AWS Marketplace: https://aws.amazon.com/marketplace/pp/prodview-kuv4iv5qyhc3g  
- MuleSoft Anypoint pricing: https://www.mulesoft.com/anypoint-pricing  
- Informatica consumption pricing: https://www.informatica.com/products/cloud-integration/pricing.html  
- Informatica IPU scalars: https://docs.informatica.com/cloud-common-services/administrator/current-version/organization-administration/metering/informatica-processing-unit-metrics/ipu-scalars.html  
- Qlik Talend Cloud subscription options: https://help.qlik.com/en-US/cloud-services/Subsystems/Hub/Content/Sense_Hub/Admin/subscription-options-QTC.htm  
- Microsoft Learn — Plan ADF costs: https://learn.microsoft.com/en-us/azure/data-factory/plan-manage-costs  
- AWS Glue pricing: https://aws.amazon.com/glue/pricing/  
- SAP Integration Suite: https://www.sap.com/products/technology-platform/integration-suite.html  
- SAP S/4HANA Installation Guide (Migration Cockpit §): https://help.sap.com/doc/6b11678926d3409bbfea8897cb34d10f/2025/en-US/INST_OP2025.pdf  
- SAP Help aliases: https://help.sap.com/S4_OP_DM , https://help.sap.com/S4_OP_MO  
- SAP Selective Data Transition Engagement: https://support.sap.com/en/offerings-programs/support-services/data-management-landscape-transformation/selective-data-transition-engagement.html  
- APIs for SAP S/4HANA (OData/BAPI/IDoc overview): https://help.sap.com/doc/57063b559ae04c86a281dc626f474091/2008.500/en-US/APIsforSAPS4HANA.pdf  
- SAP Business Accelerator Hub tutorial: https://developers.sap.com/tutorials/hcp-abh-getting-started.html  
- Salesforce Data Loader: https://developer.salesforce.com/tools/data-loader  
- Microsoft Dataverse bulk operations: https://learn.microsoft.com/en-us/power-apps/developer/data-platform/bulk-operations  
- ServiceNow Table API: https://www.servicenow.com/docs/r/api-reference/rest-apis/c_TableAPI.html  
- NetSuite REST upgrade guide: https://docs.oracle.com/en/cloud/saas/netsuite/ns-online-help/article_2095713813.html  
- Workday REST directory: https://community.workday.com/sites/default/files/file-hosting/restapi/  
- HL7 FHIR R4 RESTful API: https://www.hl7.org/fhir/R4/http.html  

### Secondary (used sparingly / labeled)

- SAP Community blogs on Migration Cockpit process (non-normative walkthroughs)  
- Integrate.io / Redress pricing explainers (third-party cost commentary — **not** used for list prices)  
- Non-SAP partner blogs on brownfield/greenfield/SDT (prefer SAP SDTE page for definitions)

### Sources attempted but not fully retrieved

| Source | Issue |
|--------|--------|
| https://developer.salesforce.com/docs/atlas.en-us.api_asynch.meta/api_asynch/asynch_api_intro.htm | WebFetch timed out; Bulk API 2.0 capabilities corroborated via Data Loader official pages |
| Live interactive browse of https://api.sap.com/ package pages | Relied on SAP Help PDF + SAP developers tutorial + SAP Community API index (Community post marked secondary where counts cited) |
| MuleSoft subscription PDF legal annex | Retrieved partially; pricing remains “contact sales” on primary HTML page |

---

*End of research note. No product implementation changes accompany this document.*
