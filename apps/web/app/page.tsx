import Link from "next/link";
import { Card } from "@/components/ui";

export default function HomePage() {
  const tiles = [
    { href: "/templates", title: "Rule Templates", desc: "Design & version rule templates with drag-and-drop mapping." },
    { href: "/jobs", title: "Jobs", desc: "Start, monitor, pause, resume, and retry migration runs." },
    { href: "/schedules", title: "Schedules", desc: "Run recurring migrations driven by Temporal Schedules." },
    { href: "/connectors", title: "Connectors", desc: "Manage Salesforce OAuth and watched-prefix sources." }
  ];
  return (
    <div className="space-y-6">
      <section>
        <h1 className="text-2xl font-semibold">Welcome</h1>
        <p className="text-slate-600">Configure, schedule, and operate data migrations at 10M-row scale.</p>
      </section>
      <section className="grid grid-cols-1 gap-4 md:grid-cols-2">
        {tiles.map((t) => (
          <Link key={t.href} href={t.href}>
            <Card className="transition hover:shadow-md">
              <h2 className="mb-1 text-lg font-medium text-brand-700">{t.title}</h2>
              <p className="text-sm text-slate-600">{t.desc}</p>
            </Card>
          </Link>
        ))}
      </section>
    </div>
  );
}
