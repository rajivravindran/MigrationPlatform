"use client";

import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { CheckCircle2, ChevronDown, ChevronRight, FlaskConical, Trash2 } from "lucide-react";
import { useState } from "react";
import { toast } from "sonner";

import {
  Button,
  Card,
  EmptyState,
  ErrorState,
  FormField,
  Input,
  LoadingState,
  PageHeader,
  Select,
  StatusBadge,
  Textarea,
  formatDateTime
} from "@/components/ui";
import { apiFetch } from "@/lib/api";

type Connector = {
  id: number;
  name: string;
  /** API stores `watched_sftp` (create also accepts legacy alias `sftp`). */
  connector_kind: string;
  config_json: Record<string, unknown>;
  created_at: string;
};

const KINDS = ["salesforce", "watched_prefix", "watched_sftp", "custom"] as const;
type Kind = (typeof KINDS)[number];

const KIND_LABELS: Record<string, string> = {
  salesforce: "Salesforce",
  watched_prefix: "S3 / MinIO watched prefix",
  watched_sftp: "SFTP watched folder",
  sftp: "SFTP watched folder",
  custom: "Custom"
};

/** Real remote validation is implemented only for watched SFTP today. */
function supportsRemoteTest(kind: string) {
  return kind === "watched_sftp" || kind === "sftp";
}

function kindLabel(kind: string) {
  return KIND_LABELS[kind] ?? kind;
}

const DEFAULTS: Record<Kind, Record<string, unknown>> = {
  salesforce: { instance_url: "", client_id: "", username: "" },
  watched_prefix: { bucket: "", prefix: "incoming/", glob: "*.csv", sort: "key" },
  watched_sftp: {
    host: "", port: 22, username: "", path: "/incoming", glob: "*", sort: "mtime",
    staging_bucket: "", staging_prefix: "sftp-landing/", insecure_ignore_host_key: false
  },
  custom: { kind: "http", base_url: "" }
};

function validate(kind: Kind, name: string, config: Record<string, unknown>, secret: string) {
  const errors: Record<string, string> = {};
  if (!name.trim()) errors.name = "Enter a connector name.";
  if (kind === "salesforce") {
    if (!String(config.instance_url ?? "").startsWith("https://")) errors.instance_url = "Use an HTTPS Salesforce instance URL.";
    if (!String(config.client_id ?? "").trim()) errors.client_id = "Consumer key is required.";
    if (!String(config.username ?? "").includes("@")) errors.username = "Enter a valid Salesforce username.";
  } else if (kind === "watched_prefix") {
    if (!String(config.bucket ?? "").trim()) errors.bucket = "Bucket is required.";
  } else if (kind === "watched_sftp") {
    if (!String(config.host ?? "").trim()) errors.host = "Host is required.";
    if (!String(config.username ?? "").trim()) errors.username = "Username is required.";
    if (!String(config.staging_bucket ?? "").trim()) errors.staging_bucket = "A staging bucket is required.";
    if (!secret.trim()) errors.secret = "Provide a password or private key JSON.";
    if (!config.insecure_ignore_host_key && !config.host_key) errors.host_key = "Provide a host key, or explicitly allow insecure verification for development.";
  } else if (!String(config.base_url ?? "").startsWith("https://")) {
    errors.base_url = "Use an HTTPS base URL.";
  }
  return errors;
}

export default function ConnectorsPage() {
  const client = useQueryClient();
  const { data, isLoading, error, refetch } = useQuery({
    queryKey: ["connectors"],
    queryFn: () => apiFetch<{ items: Connector[] }>("/connectors")
  });

  const [name, setName] = useState("");
  const [kind, setKind] = useState<Kind>("salesforce");
  const [config, setConfig] = useState<Record<string, unknown>>({ ...DEFAULTS.salesforce });
  const [secret, setSecret] = useState("");
  const [advanced, setAdvanced] = useState(false);
  const [advancedJson, setAdvancedJson] = useState(JSON.stringify(DEFAULTS.salesforce, null, 2));
  const [advancedError, setAdvancedError] = useState("");
  const [errors, setErrors] = useState<Record<string, string>>({});
  const [expanded, setExpanded] = useState<number | null>(null);
  const [tested, setTested] = useState<Record<number, { ok: boolean; message: string }>>({});

  const setField = (key: string, value: unknown) => {
    setConfig((current) => {
      const next = { ...current, [key]: value };
      setAdvancedJson(JSON.stringify(next, null, 2));
      return next;
    });
    setErrors((current) => ({ ...current, [key]: "" }));
  };

  const create = useMutation({
    mutationFn: () => {
      const nextErrors = validate(kind, name, config, secret);
      setErrors(nextErrors);
      if (Object.keys(nextErrors).length) throw new Error("Review the highlighted fields.");
      return apiFetch("/connectors", {
        method: "POST",
        body: JSON.stringify({
          name: name.trim(),
          connector_kind: kind,
          config_json: config,
          secret: secret || null
        })
      });
    },
    onSuccess: () => {
      toast.success("Connector created");
      setName(""); setSecret(""); setConfig({ ...DEFAULTS[kind] }); setAdvancedJson(JSON.stringify(DEFAULTS[kind], null, 2));
      client.invalidateQueries({ queryKey: ["connectors"] });
    },
    onError: (err) => toast.error(err instanceof Error ? err.message : String(err))
  });

  const test = useMutation({
    mutationFn: (id: number) => apiFetch<{ ok: boolean; message: string }>(`/connectors/${id}/test`, { method: "POST" }),
    onSuccess: (result, id) => {
      setTested((current) => ({ ...current, [id]: { ok: true, message: result.message } }));
      toast.success(result.message || "Connector test succeeded");
    },
    onError: (err, id) => {
      const message = err instanceof Error ? err.message : String(err);
      setTested((current) => ({ ...current, [id]: { ok: false, message } }));
      toast.error(message);
    }
  });

  const remove = useMutation({
    mutationFn: (id: number) => apiFetch(`/connectors/${id}`, { method: "DELETE" }),
    onSuccess: () => {
      toast.success("Connector deleted");
      client.invalidateQueries({ queryKey: ["connectors"] });
    },
    onError: (err) => toast.error(err instanceof Error ? err.message : String(err))
  });

  return (
    <div className="space-y-6">
      <PageHeader
        title="Connectors"
        description="Configure secure source connections for scheduled and watched migrations. Secrets are stored separately and are not shown again."
      />
      {isLoading ? <LoadingState label="Loading connectors" /> : null}
      {error ? <ErrorState error={error} retry={() => refetch()} /> : null}

      <div className="grid grid-cols-1 gap-6 xl:grid-cols-[minmax(0,1fr)_440px]">
      <section className="space-y-3" aria-labelledby="configured-connectors">
        <h2 id="configured-connectors" className="text-lg font-semibold">Configured connectors</h2>
        {data && data.items?.length === 0 ? (
          <EmptyState title="No connectors yet" hint="Add the source connection used by a schedule or watched migration." />
        ) : null}
        <div className="space-y-2">
          {data?.items?.map((c) => (
            <Card key={c.id} className="!p-0">
              <div className="flex items-center gap-3 p-4">
                <button className="rounded p-1 text-slate-500 hover:bg-slate-100" aria-label={`${expanded === c.id ? "Hide" : "Show"} ${c.name} details`} onClick={() => setExpanded(expanded === c.id ? null : c.id)}>
                  {expanded === c.id ? <ChevronDown className="h-4 w-4" /> : <ChevronRight className="h-4 w-4" />}
                </button>
                <div className="min-w-0 flex-1">
                  <div className="text-base font-medium">{c.name}</div>
                  <div className="text-xs text-slate-500">{kindLabel(c.connector_kind)} · Added {formatDateTime(c.created_at)}</div>
                </div>
                {tested[c.id]?.ok ? <StatusBadge status="healthy" /> : null}
                <Button
                  size="sm"
                  variant="secondary"
                  title={
                    supportsRemoteTest(c.connector_kind)
                      ? "Run SFTP handshake and bounded listing via the orchestrator"
                      : "Remote validation is only implemented for SFTP watched connectors"
                  }
                  onClick={() => test.mutate(c.id)}
                  disabled={test.isPending}
                >
                  <FlaskConical className="h-3.5 w-3.5" />Test
                </Button>
                <Button
                  size="sm"
                  variant="ghost"
                  aria-label={`Delete ${c.name}`}
                  onClick={() => window.confirm(`Delete connector “${c.name}”? Schedules using it may stop working.`) && remove.mutate(c.id)}
                >
                  <Trash2 className="h-4 w-4 text-rose-600" />
                </Button>
              </div>
              {expanded === c.id ? (
                <div className="border-t border-slate-100 bg-slate-50 p-4">
                  {tested[c.id] ? (
                    <div
                      className={`mb-3 flex gap-2 rounded border p-3 text-xs ${
                        tested[c.id].ok
                          ? "border-emerald-200 bg-emerald-50 text-emerald-900"
                          : "border-amber-200 bg-amber-50 text-amber-900"
                      }`}
                    >
                      <CheckCircle2 className="h-4 w-4 shrink-0" />
                      <span>
                        <strong>{tested[c.id].message}</strong>
                        {!tested[c.id].ok && !supportsRemoteTest(c.connector_kind)
                          ? " Real remote validation is only implemented for SFTP watched connectors."
                          : null}
                      </span>
                    </div>
                  ) : supportsRemoteTest(c.connector_kind) ? (
                    <p className="mb-3 text-xs text-slate-500">
                      Test runs an SFTP handshake and bounded listing through the orchestrator.
                    </p>
                  ) : (
                    <p className="mb-3 text-xs text-slate-500">
                      Remote connection tests are not implemented for this connector kind yet; the API will reject the request.
                    </p>
                  )}
                  <dl className="grid gap-2 text-sm sm:grid-cols-2">
                    {Object.entries(c.config_json).map(([key, value]) => (
                      <div key={key}><dt className="text-xs text-slate-500">{key.replaceAll("_", " ")}</dt><dd className="break-all font-mono text-xs">{String(value)}</dd></div>
                    ))}
                  </dl>
                </div>
              ) : null}
            </Card>
          ))}
        </div>
      </section>

      <aside>
      <Card className="xl:sticky xl:top-20">
        <h2 className="text-lg font-semibold">Add connector</h2>
        <p className="mb-4 mt-1 text-sm text-slate-500">Choose a type to see only the settings it needs.</p>
        <div className="space-y-4">
        <FormField label="Connector type" required>
          <Select
            value={kind}
            onChange={(e) => {
              const k = e.target.value as Kind;
              setKind(k);
              const next = { ...DEFAULTS[k] };
              setConfig(next); setAdvancedJson(JSON.stringify(next, null, 2)); setErrors({});
            }}
          >
            {KINDS.map((k) => <option key={k} value={k}>{KIND_LABELS[k]}</option>)}
          </Select>
        </FormField>
        <FormField label="Display name" required error={errors.name} hint="Use a name operators will recognize in schedules.">
          <Input value={name} onChange={(e) => { setName(e.target.value); setErrors((x) => ({ ...x, name: "" })); }} placeholder="Production inbound files" />
        </FormField>

        {kind === "salesforce" ? <>
          <FormField label="Instance URL" required error={errors.instance_url}><Input value={String(config.instance_url)} onChange={(e) => setField("instance_url", e.target.value)} placeholder="https://my.salesforce.com" /></FormField>
          <FormField label="Consumer key" required error={errors.client_id}><Input value={String(config.client_id)} onChange={(e) => setField("client_id", e.target.value)} /></FormField>
          <FormField label="Salesforce username" required error={errors.username}><Input value={String(config.username)} onChange={(e) => setField("username", e.target.value)} placeholder="integration@example.com" /></FormField>
        </> : null}

        {kind === "watched_prefix" ? <>
          <FormField label="Bucket" required error={errors.bucket}><Input value={String(config.bucket)} onChange={(e) => setField("bucket", e.target.value)} placeholder="migration-inbound" /></FormField>
          <FormField label="Prefix" hint="Only objects under this path are watched."><Input value={String(config.prefix)} onChange={(e) => setField("prefix", e.target.value)} /></FormField>
          <div className="grid grid-cols-2 gap-3">
            <FormField label="File pattern"><Input value={String(config.glob)} onChange={(e) => setField("glob", e.target.value)} /></FormField>
            <FormField label="Sort order"><Select value={String(config.sort)} onChange={(e) => setField("sort", e.target.value)}><option value="key">Object key</option><option value="mtime">Modified time</option></Select></FormField>
          </div>
        </> : null}

        {kind === "watched_sftp" ? <>
          <div className="grid grid-cols-[1fr_100px] gap-3">
            <FormField label="Host" required error={errors.host}><Input value={String(config.host)} onChange={(e) => setField("host", e.target.value)} placeholder="sftp.example.com" /></FormField>
            <FormField label="Port"><Input type="number" value={Number(config.port)} onChange={(e) => setField("port", Number(e.target.value))} /></FormField>
          </div>
          <FormField label="Username" required error={errors.username}><Input value={String(config.username)} onChange={(e) => setField("username", e.target.value)} /></FormField>
          <FormField label="Remote path"><Input value={String(config.path)} onChange={(e) => setField("path", e.target.value)} /></FormField>
          <FormField label="Staging bucket" required error={errors.staging_bucket} hint="Downloaded files are staged here before processing."><Input value={String(config.staging_bucket)} onChange={(e) => setField("staging_bucket", e.target.value)} /></FormField>
          <FormField label="SSH host key" error={errors.host_key} hint="Recommended for production to prevent machine-in-the-middle attacks."><Input value={String(config.host_key ?? "")} onChange={(e) => setField("host_key", e.target.value)} placeholder="ssh-ed25519 AAAA…" /></FormField>
          <label className="flex items-start gap-2 text-sm text-slate-700"><input className="mt-1" type="checkbox" checked={Boolean(config.insecure_ignore_host_key)} onChange={(e) => setField("insecure_ignore_host_key", e.target.checked)} /><span>Skip host-key verification <span className="block text-xs text-rose-700">Development only. This weakens connection security.</span></span></label>
        </> : null}

        {kind === "custom" ? <>
          <FormField label="Adapter kind" required><Input value={String(config.kind)} onChange={(e) => setField("kind", e.target.value)} placeholder="http" /></FormField>
          <FormField label="Base URL" required error={errors.base_url}><Input value={String(config.base_url)} onChange={(e) => setField("base_url", e.target.value)} placeholder="https://api.example.com" /></FormField>
        </> : null}

        <FormField label={kind === "watched_sftp" ? "Password or private key JSON" : "Secret"} required={kind === "watched_sftp"} error={errors.secret} hint="Encrypted at rest. The value cannot be retrieved after saving.">
          <Input type="password" value={secret} onChange={(e) => { setSecret(e.target.value); setErrors((x) => ({ ...x, secret: "" })); }} autoComplete="new-password" />
        </FormField>

        <div className="border-t border-slate-200 pt-3">
          <button type="button" className="flex w-full items-center justify-between text-sm font-medium text-slate-700" onClick={() => setAdvanced(!advanced)} aria-expanded={advanced}>
            Advanced JSON <ChevronDown className={`h-4 w-4 transition ${advanced ? "rotate-180" : ""}`} />
          </button>
          {advanced ? (
            <FormField label="Configuration JSON" error={advancedError} hint="Expert mode. Changes here update the typed fields above.">
              <Textarea
                className="font-mono text-xs"
                rows={10}
                value={advancedJson}
                onChange={(e) => {
                  setAdvancedJson(e.target.value);
                  try { const parsed = JSON.parse(e.target.value); setConfig(parsed); setAdvancedError(""); }
                  catch { setAdvancedError("Enter valid JSON before saving."); }
                }}
              />
            </FormField>
          ) : null}
        </div>
        <Button className="w-full" onClick={() => create.mutate()} disabled={create.isPending || Boolean(advancedError)}>
          {create.isPending ? "Saving…" : "Add connector"}
        </Button>
        </div>
      </Card>
      </aside>
      </div>
    </div>
  );
}
