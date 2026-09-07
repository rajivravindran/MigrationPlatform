"use client";

import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useEffect, useState } from "react";
import { toast } from "sonner";

import { Badge, Button, Card, ErrorState, FormField, Input, PageHeader } from "@/components/ui";
import { apiFetch } from "@/lib/api";

type LlmConfig = {
  configured: boolean;
  provider?: string;
  base_url?: string;
  model?: string;
  has_api_key?: boolean;
  redact_pii?: boolean;
  enabled?: boolean;
};

type LicenseHeartbeat = {
  required: boolean;
  status: "not_required" | "ok" | "degraded" | "grace_expired" | "revoked";
  last_attested_at?: string | null;
  grace_until?: string | null;
  due?: boolean;
  last_attempt_at?: string | null;
  last_error?: string | null;
  server_configured?: boolean;
};

type LicenseStatus = {
  licensed: boolean;
  mode?: string;
  problem?: string | null;
  revoked_reason?: string | null;
  kind?: string;
  licensee?: string;
  expires_at?: string;
  days_remaining?: number;
  features?: string[];
  max_seats?: number;
  fingerprint?: string;
  /** Full hex install ID; only returned to admins. Vendors bind commercial licenses to it. */
  install_id_full?: string;
  enforce?: boolean;
  heartbeat?: LicenseHeartbeat | null;
  trial_available?: boolean;
  activation_hint?: string;
};

function licenseBadgeLabel(lic: LicenseStatus): string {
  if (lic.licensed) {
    return lic.kind === "trial" ? `trial · ${lic.days_remaining ?? "?"}d left` : "commercial";
  }
  switch (lic.mode) {
    case "development":
      return "development mode";
    case "expired":
      return "expired";
    case "revoked":
      return "revoked";
    case "grace_expired":
      return "heartbeat grace expired";
    case "fingerprint_mismatch":
      return "wrong install ID";
    default:
      return "unlicensed";
  }
}

function licenseBadgeTone(lic: LicenseStatus): "ok" | "warn" | "fail" {
  if (!lic.licensed) return lic.mode === "development" ? "warn" : "fail";
  if (lic.kind === "trial" && (lic.days_remaining ?? 0) <= 3) return "warn";
  if (lic.heartbeat?.status === "degraded") return "warn";
  return "ok";
}

function heartbeatLabel(hb: LicenseHeartbeat): string {
  switch (hb.status) {
    case "not_required":
      return "not required (offline license)";
    case "ok":
      return "ok";
    case "degraded":
      return "last attempt failed — running on offline grace";
    case "grace_expired":
      return "offline grace exhausted";
    case "revoked":
      return "revoked by license server";
  }
}

export default function SettingsPage() {
  const client = useQueryClient();
  const cfgQ = useQuery({ queryKey: ["llm-config"], queryFn: () => apiFetch<LlmConfig>("/llm-config") });
  const licQ = useQuery({ queryKey: ["license"], queryFn: () => apiFetch<LicenseStatus>("/license") });

  const [baseUrl, setBaseUrl] = useState("https://api.openai.com/v1");
  const [model, setModel] = useState("gpt-4o-mini");
  const [apiKey, setApiKey] = useState("");
  const [redactPii, setRedactPii] = useState(true);
  const [enabled, setEnabled] = useState(true);
  const [trialEmail, setTrialEmail] = useState("");

  const startTrial = useMutation({
    mutationFn: () =>
      apiFetch<LicenseStatus>("/license/trial", {
        method: "POST",
        body: JSON.stringify({ email: trialEmail.trim() })
      }),
    onSuccess: (data) => {
      toast.success(
        data.licensed
          ? `Trial active for ${data.licensee ?? "this install"} · ${data.days_remaining ?? "?"} days left`
          : "Trial request accepted"
      );
      setTrialEmail("");
      client.setQueryData(["license"], data);
      client.invalidateQueries({ queryKey: ["license"] });
    },
    onError: (err) => toast.error(err instanceof Error ? err.message : String(err))
  });

  useEffect(() => {
    const c = cfgQ.data;
    if (c?.configured) {
      setBaseUrl(c.base_url ?? "");
      setModel(c.model ?? "");
      setRedactPii(c.redact_pii ?? true);
      setEnabled(c.enabled ?? true);
    }
  }, [cfgQ.data]);

  const save = useMutation({
    mutationFn: () =>
      apiFetch("/llm-config", {
        method: "PUT",
        body: JSON.stringify({
          base_url: baseUrl,
          model,
          // Empty input means "keep the stored key".
          api_key: apiKey || null,
          redact_pii: redactPii,
          enabled
        })
      }),
    onSuccess: () => {
      toast.success("LLM configuration saved");
      setApiKey("");
      client.invalidateQueries({ queryKey: ["llm-config"] });
    },
    onError: (err) => toast.error(err instanceof Error ? err.message : String(err))
  });

  return (
    <div className="mx-auto max-w-4xl space-y-6">
      <PageHeader title="Settings" description="Manage optional AI assistance and inspect this installation’s reported license state." />
      {cfgQ.error ? <ErrorState title="Couldn’t load AI configuration" error={cfgQ.error} retry={() => cfgQ.refetch()} /> : null}

      <Card>
        <div className="mb-3 flex items-center gap-2">
          <h2 className="text-lg font-medium">AI mapping provider</h2>
          {cfgQ.data?.configured ? (
            <Badge tone={cfgQ.data.enabled ? "ok" : "warn"}>
              {cfgQ.data.enabled ? "enabled" : "disabled"}
            </Badge>
          ) : (
            <Badge tone="neutral">not configured</Badge>
          )}
        </div>
        <p className="mb-4 text-sm text-slate-500">
          Any OpenAI-compatible chat-completions endpoint works (OpenAI, Azure OpenAI, Ollama, vLLM…).
          The key is encrypted at rest; sample rows are PII-redacted before being sent unless disabled.
        </p>
        <div className="space-y-3">
          <FormField label="Base URL" required hint="Use an HTTPS endpoint in production.">
            <Input value={baseUrl} onChange={(e) => setBaseUrl(e.target.value)} placeholder="https://api.openai.com/v1" />
          </FormField>
          <FormField label="Model" required>
            <Input value={model} onChange={(e) => setModel(e.target.value)} placeholder="gpt-4o-mini" />
          </FormField>
          <FormField label="API key" hint={cfgQ.data?.has_api_key ? "A key is stored. Leave this blank to keep it." : "Stored encrypted by the API after saving."}>
            <Input
              type="password"
              value={apiKey}
              onChange={(e) => setApiKey(e.target.value)}
              placeholder={cfgQ.data?.has_api_key ? "••••••••" : "sk-…"}
            />
          </FormField>
          <label className="flex items-center gap-2 text-sm text-slate-700">
            <input type="checkbox" checked={redactPii} onChange={(e) => setRedactPii(e.target.checked)} />
            Redact PII (emails, phone numbers, long digit runs) from sample rows before sending
          </label>
          <label className="flex items-center gap-2 text-sm text-slate-700">
            <input type="checkbox" checked={enabled} onChange={(e) => setEnabled(e.target.checked)} />
            Enable AI mapping suggestions
          </label>
          <Button onClick={() => save.mutate()} disabled={save.isPending || !baseUrl || !model}>
            {save.isPending ? "Saving…" : "Save"}
          </Button>
        </div>
      </Card>

      <Card>
        <div className="mb-2 flex items-center gap-2">
          <h2 className="text-lg font-medium">License</h2>
          {licQ.data ? <Badge tone={licenseBadgeTone(licQ.data)}>{licenseBadgeLabel(licQ.data)}</Badge> : null}
          {licQ.data?.enforce !== undefined ? (
            <Badge tone={licQ.data.enforce ? "warn" : "neutral"}>
              {licQ.data.enforce ? "enforcement on" : "enforcement off"}
            </Badge>
          ) : null}
        </div>
        {licQ.error ? <ErrorState title="License status unavailable" error={licQ.error} retry={() => licQ.refetch()} /> : null}
        {licQ.data && !licQ.data.licensed && (licQ.data.mode === "revoked" || licQ.data.mode === "grace_expired") ? (
          <div className="mb-4 rounded-lg border border-rose-300 bg-rose-50 p-4 text-sm">
            <div className="font-medium text-rose-900">
              {licQ.data.mode === "revoked" ? "License revoked by the license server" : "Offline grace exhausted"}
            </div>
            <p className="mt-1 text-rose-800">
              {licQ.data.mode === "revoked"
                ? `Work-producing APIs are blocked.${licQ.data.revoked_reason ? ` Reason: ${licQ.data.revoked_reason}.` : ""}`
                : "This license must be re-attested with the license server every 24 hours and has now been unreachable for more than 72 hours. Restore connectivity; the next heartbeat re-attests automatically."}
            </p>
          </div>
        ) : null}
        {licQ.data?.kind === "trial" && licQ.data.days_remaining !== undefined ? (
          <div className={`mb-4 rounded-lg border p-4 ${licQ.data.days_remaining <= 3 ? "border-amber-300 bg-amber-50" : "border-blue-200 bg-blue-50"}`}>
            <div className="flex items-center justify-between gap-4">
              <div>
                <div className="font-medium">
                  {licQ.data.days_remaining > 0
                    ? `${licQ.data.days_remaining} trial days remaining`
                    : "Trial period ended"}
                </div>
                <p className="mt-1 text-sm text-slate-600">
                  {licQ.data.days_remaining > 0
                    ? licQ.data.enforce
                      ? "Install a commercial license before expiry to keep creating jobs, schedules, and batches."
                      : "Enforcement is off in this process. When LICENSE_ENFORCE=true, an expired trial blocks work-producing APIs."
                    : licQ.data.enforce
                      ? "Work-producing APIs are blocked until a valid commercial license is mounted (adopted within a minute)."
                      : "Enforcement is off, so mutating APIs still work. Set LICENSE_ENFORCE=true in production after mounting a license."}
                </p>
              </div>
              <div className="text-2xl font-semibold tabular-nums">{licQ.data.days_remaining}</div>
            </div>
          </div>
        ) : null}
        {licQ.data?.licensed || licQ.data?.licensee || licQ.data?.kind ? (
          <dl className="grid grid-cols-2 gap-y-1 text-sm">
            <dt className="text-slate-500">Licensee</dt>
            <dd>{licQ.data.licensee ?? "—"}</dd>
            <dt className="text-slate-500">Kind</dt>
            <dd>{licQ.data.kind ?? "—"}</dd>
            <dt className="text-slate-500">Days left</dt>
            <dd>{licQ.data.days_remaining ?? "—"}</dd>
            <dt className="text-slate-500">Expires</dt>
            <dd>{licQ.data.expires_at ? new Date(licQ.data.expires_at).toLocaleDateString() : "—"}</dd>
            <dt className="text-slate-500">Install ID</dt>
            <dd className="font-mono text-xs">{licQ.data.fingerprint ?? "—"}</dd>
            <dt className="text-slate-500">Enforcement</dt>
            <dd>{licQ.data.enforce ? "on (mutating APIs require a valid license)" : "off (development mode)"}</dd>
            {licQ.data.licensed ? (
              <>
                <dt className="text-slate-500">Features</dt>
                <dd>{(licQ.data.features ?? []).join(", ") || "—"}</dd>
                <dt className="text-slate-500">Max seats</dt>
                <dd>{licQ.data.max_seats ?? "unlimited"}</dd>
              </>
            ) : null}
            {licQ.data.heartbeat ? (
              <>
                <dt className="text-slate-500">Heartbeat</dt>
                <dd>
                  <span
                    className={
                      licQ.data.heartbeat.status === "ok" || licQ.data.heartbeat.status === "not_required"
                        ? ""
                        : licQ.data.heartbeat.status === "degraded"
                          ? "text-amber-700"
                          : "text-rose-700"
                    }
                  >
                    {heartbeatLabel(licQ.data.heartbeat)}
                  </span>
                  {licQ.data.heartbeat.required && !licQ.data.heartbeat.server_configured ? (
                    <span className="ml-2 text-xs text-rose-700">LICENSE_SERVER_URL is not configured on the API</span>
                  ) : null}
                </dd>
                {licQ.data.heartbeat.required ? (
                  <>
                    <dt className="text-slate-500">Last attested</dt>
                    <dd>
                      {licQ.data.heartbeat.last_attested_at
                        ? new Date(licQ.data.heartbeat.last_attested_at).toLocaleString()
                        : "—"}
                    </dd>
                    <dt className="text-slate-500">Offline grace until</dt>
                    <dd>
                      {licQ.data.heartbeat.grace_until ? new Date(licQ.data.heartbeat.grace_until).toLocaleString() : "—"}
                    </dd>
                    {licQ.data.heartbeat.last_error ? (
                      <>
                        <dt className="text-slate-500">Last heartbeat error</dt>
                        <dd className="break-words text-xs text-slate-600">{licQ.data.heartbeat.last_error}</dd>
                      </>
                    ) : null}
                  </>
                ) : null}
              </>
            ) : null}
          </dl>
        ) : (
          <dl className="mb-3 grid grid-cols-2 gap-y-1 text-sm">
            <dt className="text-slate-500">Install ID</dt>
            <dd className="font-mono text-xs">{licQ.data?.fingerprint ?? "—"}</dd>
            <dt className="text-slate-500">Enforcement</dt>
            <dd>{licQ.data?.enforce ? "on (mutating APIs require a valid license)" : "off (development mode)"}</dd>
            <dt className="text-slate-500">Days left</dt>
            <dd>0</dd>
          </dl>
        )}
        {licQ.data?.activation_hint ? (
          <p className="mt-3 text-sm text-slate-500">{licQ.data.activation_hint}</p>
        ) : null}
        {licQ.data && !licQ.data.licensed && licQ.data.trial_available && licQ.data.mode !== "revoked" ? (
          <div className="mt-4 rounded border border-blue-200 bg-blue-50 p-3">
            <div className="text-sm font-medium text-slate-800">Start a 10-day trial</div>
            <p className="mt-1 text-xs leading-5 text-slate-600">
              Admins only. The contact email is sent to the vendor license service together with this install ID; the
              trial is bound to the install ID, so re-installing does not restart the clock. The API re-attests the
              trial every 24 hours and keeps working for up to 72 hours if the license service is unreachable.
            </p>
            <div className="mt-3 flex flex-wrap items-end gap-3">
              <div className="min-w-[16rem] flex-1">
                <FormField label="Contact email" required>
                  <Input
                    type="email"
                    value={trialEmail}
                    onChange={(e) => setTrialEmail(e.target.value)}
                    placeholder="ops@example.com"
                    autoComplete="email"
                  />
                </FormField>
              </div>
              <Button
                onClick={() => startTrial.mutate()}
                disabled={startTrial.isPending || !/^[^\s@]+@[^\s@]+\.[^\s@]+$/.test(trialEmail.trim())}
              >
                {startTrial.isPending ? "Activating…" : "Start trial"}
              </Button>
            </div>
          </div>
        ) : null}
        <div className="mt-4 rounded border border-slate-200 bg-slate-50 p-3 text-sm text-slate-600">
          <div className="font-medium text-slate-800">{licQ.data?.licensed ? "Renewal and replacement" : "Commercial activation"}</div>
          {licQ.data?.install_id_full ? (
            <div className="mt-2 flex flex-wrap items-center gap-2 text-xs">
              <span className="text-slate-500">Full install ID for the vendor:</span>
              <code className="break-all rounded bg-slate-100 px-1 font-mono">{licQ.data.install_id_full}</code>
              <Button
                variant="ghost"
                size="sm"
                onClick={() =>
                  navigator.clipboard
                    .writeText(licQ.data?.install_id_full ?? "")
                    .then(() => toast.success("Install ID copied"))
                    .catch(() => toast.error("Clipboard unavailable"))
                }
              >
                Copy
              </Button>
            </div>
          ) : null}
          <p className="mt-1 text-xs leading-5">
          Send the install ID to the vendor; they sign a license bound to it. Mount the file as{" "}
          <code className="rounded bg-slate-100 px-1">LICENSE_FILE</code> (or copy it to{" "}
          <code className="rounded bg-slate-100 px-1">LICENSE_STORE_PATH</code>; a changed store file is adopted within a minute).
          With <code className="rounded bg-slate-100 px-1">LICENSE_ENFORCE=true</code>, unlicensed installs
          return <code className="rounded bg-slate-100 px-1">402 license_required</code> on jobs, schedules, and batches.
          </p>
        </div>
      </Card>
    </div>
  );
}
