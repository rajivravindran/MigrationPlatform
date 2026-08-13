import "./globals.css";
import type { Metadata } from "next";
import { Providers } from "@/lib/providers";
import { Nav } from "@/components/ui";

export const metadata: Metadata = {
  title: "Migration Platform",
  description: "Configure, schedule, and operate data migration jobs."
};

export default function RootLayout({ children }: { children: React.ReactNode }) {
  return (
    <html lang="en">
      <body>
        <Providers>
          <a href="#main-content" className="sr-only z-50 rounded bg-white px-3 py-2 focus:not-sr-only focus:fixed focus:left-3 focus:top-3">
            Skip to content
          </a>
          <header className="sticky top-0 z-30 border-b border-slate-200 bg-white">
            <div className="mx-auto flex max-w-[1480px] items-center gap-4 px-4 py-2.5 sm:px-6">
              <a href="/" className="shrink-0 text-base font-semibold tracking-tight text-slate-950">
                <span className="hidden sm:inline">Migration Platform</span>
                <span className="sm:hidden">MP</span>
              </a>
              <Nav />
            </div>
          </header>
          <main id="main-content" className="mx-auto max-w-[1480px] px-4 py-6 sm:px-6 lg:py-8">{children}</main>
        </Providers>
      </body>
    </html>
  );
}
