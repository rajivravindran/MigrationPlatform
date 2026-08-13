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
    if (!Number.isInteger(step) || step < 1) throw new Error("invalid step");
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
    if (!Number.isInteger(lo) || !Number.isInteger(hi) || lo < min || hi > max || lo > hi) {
      throw new Error("field out of range");
    }
    for (let v = lo; v <= hi; v += step) values.add(v);
  });
  if (values.size === 0) throw new Error("empty field");
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
  let formatter: Intl.DateTimeFormat;
  try {
    formatter = new Intl.DateTimeFormat("en-US", {
      timeZone: timezone,
      minute: "numeric",
      hour: "numeric",
      day: "numeric",
      month: "numeric",
      weekday: "short",
      hourCycle: "h23"
    });
    formatter.format(new Date());
  } catch {
    return [];
  }
  const now = new Date();
  const out: string[] = [];
  const weekdays: Record<string, number> = { Sun: 0, Mon: 1, Tue: 2, Wed: 3, Thu: 4, Fri: 5, Sat: 6 };
  for (let i = 0; i < 24 * 60 * 366 && out.length < count; i++) {
    const d = new Date(now.getTime() + i * 60_000);
    d.setSeconds(0, 0);
    const parts = Object.fromEntries(formatter.formatToParts(d).map((part) => [part.type, part.value]));
    if (
      minute.values.has(Number(parts.minute)) &&
      hour.values.has(Number(parts.hour)) &&
      dom.values.has(Number(parts.day)) &&
      month.values.has(Number(parts.month)) &&
      dow.values.has(weekdays[parts.weekday])
    ) {
      out.push(d.toLocaleString(undefined, { timeZone: timezone, dateStyle: "medium", timeStyle: "short" }));
    }
  }
  return out;
}
