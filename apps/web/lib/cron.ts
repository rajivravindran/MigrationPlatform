// Minimal cron preview helper. Supports 5-field POSIX cron expressions with
// `*`, numeric values, and `*/N` steps. For production-grade preview the
// server also returns next-fire times; this helper is only used to give quick
// feedback in the New Schedule form.

type Field = { values: Set<number> };

function parseField(raw: string, min: number, max: number): Field {
  const values = new Set<number>();
  raw.split(",").forEach((part) => {
    const [range, stepStr] = part.split("/");
    const step = stepStr ? Number(stepStr) : 1;
    let lo = min;
    let hi = max;
    if (range && range !== "*") {
      if (range.includes("-")) {
        const [a, b] = range.split("-").map(Number);
        lo = a;
        hi = b;
      } else {
        lo = hi = Number(range);
      }
    }
    for (let v = lo; v <= hi; v += step) values.add(v);
  });
  return { values };
}

export function previewCron(expr: string, timezone = "UTC", count = 5): string[] {
  const parts = expr.trim().split(/\s+/);
  if (parts.length !== 5) return [];
  let minute: Field, hour: Field, dom: Field, month: Field, dow: Field;
  try {
    minute = parseField(parts[0], 0, 59);
    hour = parseField(parts[1], 0, 23);
    dom = parseField(parts[2], 1, 31);
    month = parseField(parts[3], 1, 12);
    dow = parseField(parts[4], 0, 6);
  } catch {
    return [];
  }
  const now = new Date();
  const out: string[] = [];
  for (let i = 0; i < 24 * 60 * 366 && out.length < count; i++) {
    const d = new Date(now.getTime() + i * 60_000);
    d.setSeconds(0, 0);
    if (
      minute.values.has(d.getUTCMinutes()) &&
      hour.values.has(d.getUTCHours()) &&
      dom.values.has(d.getUTCDate()) &&
      month.values.has(d.getUTCMonth() + 1) &&
      dow.values.has(d.getUTCDay())
    ) {
      out.push(
        d.toLocaleString(undefined, { timeZone: timezone, hour12: false })
      );
    }
  }
  return out;
}
