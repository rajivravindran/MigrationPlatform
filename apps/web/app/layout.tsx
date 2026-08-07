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
          <header className="sticky top-0 z-30 border-b border-slate-200/80 bg-white/90 backdrop-blur">
            <div className="mx-auto flex max-w-[1400px] items-center justify-between px-4 py-3 sm:px-6">
              <a href="/" className="text-lg font-semibold tracking-tight text-slate-900">
                Migration Platform
              </a>
              <Nav />
            </div>
          </header>
          <main className="mx-auto max-w-[1400px] px-4 py-6 sm:px-6">{children}</main>
        </Providers>
      </body>
    </html>
  );
}
