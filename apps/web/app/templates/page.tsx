"use client";

import { useQuery } from "@tanstack/react-query";
import Link from "next/link";

import { Badge, Button, Card, EmptyState } from "@/components/ui";
import { apiFetch } from "@/lib/api";

type TemplateRow = {
  id: number;
  template_key: string;
  name: string;
  version: number;
  published: boolean;
  created_at: string;
};

export default function TemplatesPage() {
  const { data, isLoading, error } = useQuery({
    queryKey: ["templates"],
    queryFn: () => apiFetch<{ items: TemplateRow[] }>("/rule-templates")
  });

  return (
    <div className="space-y-4">
      <div className="flex items-center justify-between">
        <div>
          <h1 className="text-2xl font-semibold">Rule Templates</h1>
          <p className="text-sm text-slate-500">
            Design versioned mapping templates that transform input rows into destination payloads.
          </p>
        </div>
        <Link href="/templates/new">
          <Button>New template</Button>
        </Link>
      </div>
      {isLoading ? <Card>Loading&hellip;</Card> : null}
      {error ? <Card className="border-rose-200 bg-rose-50 text-sm text-rose-700">{String(error)}</Card> : null}
      {data && data.items?.length === 0 ? (
        <EmptyState
          title="No templates yet"
          hint="Create your first template to define how incoming rows map to an API payload."
          action={
            <Link href="/templates/new">
              <Button>Create template</Button>
            </Link>
          }
        />
      ) : null}
      <div className="grid grid-cols-1 gap-3 md:grid-cols-2">
        {data?.items?.map((t) => (
          <Link key={t.id} href={`/templates/${t.id}`}>
            <Card className="transition hover:shadow-md">
              <div className="flex items-center justify-between">
                <div>
                  <div className="text-lg font-medium">{t.name}</div>
                  <div className="text-sm text-slate-500">
                    {t.template_key} &middot; v{t.version}
                  </div>
                </div>
                <Badge tone={t.published ? "ok" : "warn"}>{t.published ? "published" : "draft"}</Badge>
              </div>
            </Card>
          </Link>
        ))}
      </div>
    </div>
  );
}
