import { NextRequest, NextResponse } from "next/server";

// Server-side proxy for file uploads. Avoids CORS + large-body issues that
// happen when the browser posts a multipart form directly to the API running
// on a different origin.
const UPSTREAM = process.env.API_INTERNAL_URL ?? process.env.NEXT_PUBLIC_API_URL ?? "http://localhost:8080";

export async function POST(req: NextRequest) {
  const auth = req.headers.get("authorization");
  if (!auth) return NextResponse.json({ error: { message: "unauthorized" } }, { status: 401 });

  const body = await req.arrayBuffer();
  const res = await fetch(`${UPSTREAM}/files`, {
    method: "POST",
    headers: {
      authorization: auth,
      "content-type": req.headers.get("content-type") ?? "application/octet-stream"
    },
    body
  });
  const text = await res.text();
  return new NextResponse(text, {
    status: res.status,
    headers: { "content-type": res.headers.get("content-type") ?? "application/json" }
  });
}

export const runtime = "nodejs";
export const dynamic = "force-dynamic";
