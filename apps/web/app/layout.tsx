import "./globals.css";
import type { Metadata } from "next";

import { MobileNav, Sidebar, Topbar } from "@/components/shell";
import { Providers } from "@/lib/providers";
import { WorkspaceProvider } from "@/lib/workspace";

export const metadata: Metadata = {
  title: "Migration Platform",
  description: "Prepare, rehearse, run and reconcile migrations into HTTP-based applications."
};

export default function RootLayout({ children }: { children: React.ReactNode }) {
  return (
    <html lang="en">
      <body>
        <Providers>
          <WorkspaceProvider>
            <a href="#main-content" className="sr-only z-50 rounded bg-white px-3 py-2 focus:not-sr-only focus:fixed focus:left-3 focus:top-3">
              Skip to content
            </a>
            <Topbar />
            <div className="flex min-h-[calc(100vh-3.5rem)]">
              <Sidebar />
              <div className="min-w-0 flex-1">
                <MobileNav />
                <main id="main-content" className="mx-auto max-w-[1180px] px-4 py-6 sm:px-8 lg:py-8">
                  {children}
                </main>
              </div>
            </div>
          </WorkspaceProvider>
        </Providers>
      </body>
    </html>
  );
}
