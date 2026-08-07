"use client";

import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useState } from "react";
import { toast } from "sonner";

import { Badge, Button, Card, EmptyState, Input, Textarea } from "@/components/ui";
import { apiFetch } from "@/lib/api";

type Connector = {
  id: number;
  name: string;
  connector_kind: "salesforce" | "watched_prefix" | "watched_sftp" | "custom";
  config_json: Record<string, unknown>;
  created_at: string;
};

const KINDS = ["salesforce", "watched_prefix", "watched_sftp", "custom"] as const;

const PLACEHOLDER_CONFIG: Record<string, string> = {
  salesforce: JSON.stringify({ instance_url: "https://my.salesforce.com", client_id: "<consumer-key>", username: "user@example.com" }, null, 2),
  watched_prefix: JSON.stringify({ bucket: "migration", prefix: "incoming/", glob: "*.csv" }, null, 2),
  watched_sftp: JSON.stringify({
    host: "sftp.example.com",
    port: 22,
    username: "dropuser",
    path: "/incoming",
    glob: "*",
    sort: "mtime",
    staging_bucket: "migration",
    staging_prefix: "sftp-landing/",
    insecure_ignore_host_key: true
  }, null, 2),
  custom: JSON.stringify({ kind: "http", base_url: "https://api.example.com" }, null, 2)
};

export default function ConnectorsPage() {
  const client = useQueryClient();
  const { data, isLoading } = useQuery({
    queryKey: ["connectors"],
    queryFn: () => apiFetch<{ items: Connector[] }>("/connectors")
  });

  const [name, setName] = useState("");
  const [kind, setKind] = useState<(typeof KINDS)[number]>("salesforce");
  const [config, setConfig] = useState(PLACEHOLDER_CONFIG["salesforce"]);
  const [secret, setSecret] = useState("");

  const create = useMutation({
    mutationFn: () =>
      apiFetch("/connectors", {
        method: "POST",
        body: JSON.stringify({
          name,
          connector_kind: kind,
          config_json: JSON.parse(config || "{}"),
          secret: secret || null
        })
      }),
    onSuccess: () => {
      toast.success("Connector created");
      setName(""); setSecret("");
      client.invalidateQueries({ queryKey: ["connectors"] });
    },
    onError: (err) => toast.error(err instanceof Error ? err.message : String(err))
  });

  return (
    <div className="grid grid-cols-1 gap-6 lg:grid-cols-2">
      <div className="space-y-3">
        <h1 className="text-2xl font-semibold">Connectors</h1>
        <p className="text-sm text-slate-500">Secrets are encrypted with AES-GCM using the platform master key.</p>
        {isLoading ? <Card>Loading&hellip;</Card> : null}
        {data && data.items?.length === 0 ? (
          <EmptyState title="No connectors yet" hint="Add a Salesforce, watched-prefix, SFTP, or custom HTTP connector." />
        ) : null}
        <div className="space-y-2">
          {data?.items?.map((c) => (
            <Card key={c.id}>
              <div className="flex items-center justify-between">
                <div>
                  <div className="text-base font-medium">{c.name}</div>
                  <div className="text-xs text-slate-500">Added {c.created_at}</div>
                </div>
                <Badge tone="neutral">{c.connector_kind}</Badge>
              </div>
              <pre className="mt-2 max-h-40 overflow-auto rounded bg-slate-50 p-2 text-xs">
{JSON.stringify(c.config_json, null, 2)}
              </pre>
            </Card>
          ))}
        </div>
      </div>

      <Card>
        <h2 className="mb-3 text-lg font-medium">Add a connector</h2>
        <label className="mb-2 block text-sm">
          Name
          <Input className="mt-1" value={name} onChange={(e) => setName(e.target.value)} />
        </label>
        <label className="mb-2 block text-sm">
          Kind
          <select
            className="mt-1 w-full rounded border border-slate-300 px-2 py-1.5 text-sm"
            value={kind}
            onChange={(e) => {
              const k = e.target.value as (typeof KINDS)[number];
              setKind(k);
              setConfig(PLACEHOLDER_CONFIG[k]);
            }}
          >
            {KINDS.map((k) => <option key={k}>{k}</option>)}
          </select>
        </label>
        <label className="mb-2 block text-sm">
          Config JSON
          <Textarea rows={10} value={config} onChange={(e) => setConfig(e.target.value)} />
        </label>
        <label className="mb-4 block text-sm">
          Secret (optional, encrypted at rest)
          <Input type="password" value={secret} onChange={(e) => setSecret(e.target.value)} placeholder="OAuth refresh token, SFTP password, or private_key JSON" />
        </label>
        <Button onClick={() => create.mutate()} disabled={create.isPending || !name}>
          {create.isPending ? "Saving..." : "Add connector"}
        </Button>
      </Card>
    </div>
  );
}
