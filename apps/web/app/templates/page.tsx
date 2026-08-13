"use client";

import { useQuery } from "@tanstack/react-query";
import Link from "next/link";

import { Button, EmptyState, ErrorState, LoadingState, PageHeader, StatusBadge, formatDateTime } from "@/components/ui";
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
  const { data, isLoading, error, refetch } = useQuery({
    queryKey: ["templates"],
    queryFn: () => apiFetch<{ items: TemplateRow[] }>("/rule-templates")
  });

  return (
    <div className="space-y-6">
      <PageHeader title="Templates" description="Design, validate, and publish versioned mappings from source rows to destination API calls." actions={<Link href="/templates/new"><Button>New template</Button></Link>} />
      {isLoading ? <LoadingState label="Loading templates" /> : null}
      {error ? <ErrorState error={error} retry={() => refetch()} /> : null}
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
      {data?.items.length ? <div className="overflow-x-auto rounded-lg border border-slate-200 bg-white">
        <table className="min-w-[620px] w-full text-sm">
          <caption className="sr-only">Migration templates</caption>
          <thead className="bg-slate-50 text-slate-600"><tr><th className="px-4 py-2.5 text-left">Name</th><th className="px-4 py-2.5 text-left">Key</th><th className="px-4 py-2.5 text-left">Version</th><th className="px-4 py-2.5 text-left">Status</th><th className="px-4 py-2.5 text-left">Created</th></tr></thead>
          <tbody>{data.items.map((template) => <tr key={template.id} className="border-t border-slate-100 hover:bg-slate-50">
            <td className="px-4 py-3 font-medium"><Link className="text-brand-700 hover:underline" href={`/templates/${template.id}`}>{template.name}</Link></td>
            <td className="px-4 py-3 font-mono text-xs text-slate-600">{template.template_key}</td>
            <td className="px-4 py-3">v{template.version}</td>
            <td className="px-4 py-3"><StatusBadge status={template.published ? "published" : "draft"} /></td>
            <td className="px-4 py-3 text-slate-500">{formatDateTime(template.created_at)}</td>
          </tr>)}</tbody>
        </table>
      </div> : null}
    </div>
  );
}
