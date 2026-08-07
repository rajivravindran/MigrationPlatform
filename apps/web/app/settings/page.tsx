"use client";

import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useEffect, useState } from "react";
import { toast } from "sonner";

import { Badge, Button, Card, Input } from "@/components/ui";
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

type LicenseStatus = {
  licensed: boolean;
  mode?: string;
  kind?: string;
  licensee?: string;
  expires_at?: string;
  days_remaining?: number;
  features?: string[];
  max_seats?: number;
  fingerprint?: string;
  enforce?: boolean;
  activation_hint?: string;
};

export default function SettingsPage() {
  const client = useQueryClient();
  const cfgQ = useQuery({ queryKey: ["llm-config"], queryFn: () => apiFetch<LlmConfig>("/llm-config") });
  const licQ = useQuery({ queryKey: ["license"], queryFn: () => apiFetch<LicenseStatus>("/license") });

  const [baseUrl, setBaseUrl] = useState("https://api.openai.com/v1");
  const [model, setModel] = useState("gpt-4o-mini");
  const [apiKey, setApiKey] = useState("");
  const [redactPii, setRedactPii] = useState(true);
  const [enabled, setEnabled] = useState(true);

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
    <div className="mx-auto max-w-3xl space-y-6">
      <h1 className="text-2xl font-semibold">Settings</h1>

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
          <div>
            <label className="mb-1 block text-xs text-slate-600">Base URL</label>
            <Input value={baseUrl} onChange={(e) => setBaseUrl(e.target.value)} placeholder="https://api.openai.com/v1" />
          </div>
          <div>
            <label className="mb-1 block text-xs text-slate-600">Model</label>
            <Input value={model} onChange={(e) => setModel(e.target.value)} placeholder="gpt-4o-mini" />
          </div>
          <div>
            <label className="mb-1 block text-xs text-slate-600">
              API key {cfgQ.data?.has_api_key ? "(stored — leave blank to keep)" : ""}
            </label>
            <Input
              type="password"
              value={apiKey}
              onChange={(e) => setApiKey(e.target.value)}
              placeholder={cfgQ.data?.has_api_key ? "••••••••" : "sk-…"}
            />
          </div>
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
          {licQ.data ? (
            <Badge
              tone={
                licQ.data.licensed
                  ? licQ.data.kind === "trial" && (licQ.data.days_remaining ?? 0) <= 3
                    ? "warn"
                    : "ok"
                  : "warn"
              }
            >
              {licQ.data.licensed
                ? licQ.data.kind === "trial"
                  ? `trial · ${licQ.data.days_remaining ?? "?"}d left`
                  : "commercial"
                : licQ.data.mode === "development"
                  ? "development mode"
                  : licQ.data.mode === "expired"
                    ? "expired"
                    : "unlicensed"}
            </Badge>
          ) : null}
        </div>
        {licQ.data?.licensed ? (
          <dl className="grid grid-cols-2 gap-y-1 text-sm">
            <dt className="text-slate-500">Licensee</dt>
            <dd>{licQ.data.licensee}</dd>
            <dt className="text-slate-500">Kind</dt>
            <dd>{licQ.data.kind ?? "—"}</dd>
            <dt className="text-slate-500">Days left</dt>
            <dd>{licQ.data.days_remaining ?? "—"}</dd>
            <dt className="text-slate-500">Expires</dt>
            <dd>{licQ.data.expires_at ? new Date(licQ.data.expires_at).toLocaleDateString() : "—"}</dd>
            <dt className="text-slate-500">Install ID</dt>
            <dd className="font-mono text-xs">{licQ.data.fingerprint ?? "—"}</dd>
            <dt className="text-slate-500">Features</dt>
            <dd>{(licQ.data.features ?? []).join(", ") || "—"}</dd>
            <dt className="text-slate-500">Max seats</dt>
            <dd>{licQ.data.max_seats ?? "unlimited"}</dd>
          </dl>
        ) : (
          <dl className="mb-3 grid grid-cols-2 gap-y-1 text-sm">
            <dt className="text-slate-500">Install ID</dt>
            <dd className="font-mono text-xs">{licQ.data?.fingerprint ?? "—"}</dd>
            <dt className="text-slate-500">Days left</dt>
            <dd>0</dd>
          </dl>
        )}
        {licQ.data?.activation_hint ? (
          <p className="mt-3 text-sm text-slate-500">{licQ.data.activation_hint}</p>
        ) : null}
        <p className="mt-2 text-xs text-slate-400">
          Activate a commercial license by mounting a vendor-signed JSON file as{" "}
          <code className="rounded bg-slate-100 px-1">LICENSE_FILE</code> (or copying it to{" "}
          <code className="rounded bg-slate-100 px-1">LICENSE_STORE_PATH</code>) and restarting the API.
        </p>
      </Card>
    </div>
  );
}
