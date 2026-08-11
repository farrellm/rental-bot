/** Formatting for the value column. Every result is fixed-width friendly. */

/**
 * What an unknown value reads as, everywhere.
 *
 * A blank cell says the column is broken; a dash says the record does not hold
 * the fact. Every screen means the second thing, so they all spell it from
 * here rather than each carrying its own literal.
 */
export const DASH = "—";

/** Renders a duration in seconds as "00:14:22", or "3d 04:12:07" past a day. */
export function uptime(seconds: number): string {
  if (!Number.isFinite(seconds) || seconds < 0) return DASH;

  const days = Math.floor(seconds / 86_400);
  const clock = [
    Math.floor((seconds % 86_400) / 3_600),
    Math.floor((seconds % 3_600) / 60),
    Math.floor(seconds % 60),
  ]
    .map((n) => String(n).padStart(2, "0"))
    .join(":");

  return days > 0 ? `${days}d ${clock}` : clock;
}

/**
 * Renders an RFC3339 timestamp as "2026-08-04 21:00" in the reader's own
 * timezone, in a fixed pattern rather than a locale one so the mono column
 * stays aligned.
 */
export function timestamp(iso: string): string {
  const at = new Date(iso);
  if (Number.isNaN(at.getTime())) return DASH;

  return `${datePart(at)} ${pad(at.getHours())}:${pad(at.getMinutes())}`;
}

/** Renders just the wall-clock time, "21:04:12". */
export function clock(iso: string): string {
  const at = new Date(iso);
  if (Number.isNaN(at.getTime())) return DASH;

  return `${pad(at.getHours())}:${pad(at.getMinutes())}:${pad(at.getSeconds())}`;
}

/** Shortens a commit hash to the length people actually read. */
export function commit(hash: string): string {
  if (!hash || hash === "unknown") return DASH;
  return hash.length > 7 ? hash.slice(0, 7) : hash;
}

/** Falls back to an em dash so the column never shows an empty cell. */
export function orDash(value: string | undefined): string {
  return value && value !== "unknown" ? value : DASH;
}

/**
 * Renders whole cents as "$285,000.00".
 *
 * The wire carries an integer count of cents and this is the only place it
 * becomes a decimal — on its way to a person's eyes and nowhere else. Null is
 * an em dash, because a property with no recorded purchase price is not a
 * property that cost nothing.
 */
export function money(cents: number | null | undefined): string {
  if (cents === null || cents === undefined || !Number.isFinite(cents)) return DASH;

  const negative = cents < 0;
  const abs = Math.abs(Math.trunc(cents));
  const whole = String(Math.floor(abs / 100)).replace(/\B(?=(\d{3})+(?!\d))/g, ",");
  const fraction = String(abs % 100).padStart(2, "0");

  return `${negative ? "-" : ""}$${whole}.${fraction}`;
}

/**
 * Parses a typed amount back into whole cents, or null when the field is
 * empty. Anything that is not a number is `undefined`, which the caller
 * reports rather than silently rounding.
 */
export function parseMoney(input: string): number | null | undefined {
  const trimmed = input.trim().replace(/[$,\s]/g, "");
  if (trimmed === "") return null;
  if (!/^-?\d*(\.\d{0,2})?$/.test(trimmed) || trimmed === "-" || trimmed === ".") return undefined;

  const negative = trimmed.startsWith("-");
  const [whole = "0", fraction = ""] = trimmed.replace("-", "").split(".");
  const cents = Number(whole || "0") * 100 + Number(fraction.padEnd(2, "0") || "0");

  return Number.isFinite(cents) ? (negative ? -cents : cents) : undefined;
}

/** Renders a nullable number, so an unknown value reads as unknown. */
export function orDashNumber(value: number | null | undefined, suffix = ""): string {
  if (value === null || value === undefined || !Number.isFinite(value)) return DASH;
  return `${value}${suffix}`;
}

/** Renders a count with its noun: "1 unit", "3 units". */
export function plural(count: number, one: string, many: string): string {
  return `${count} ${count === 1 ? one : many}`;
}

/**
 * The file number in the corner of a card.
 *
 * It is the record's own id, zero-padded so the column reads as a column.
 * Nothing decorative: two properties never share one, and it is what you would
 * say out loud to name a record.
 */
export function fileNumber(id: number): string {
  return `No. ${String(id).padStart(4, "0")}`;
}

/** A calendar date exactly as stored, never reinterpreted through a timezone. */
export function calendarDate(value: string | null | undefined): string {
  return value ? value : DASH;
}

/**
 * The shape a calendar date has to have on its way in.
 *
 * Only the shape. Whether 2026-02-31 is a day is the server's question, and
 * asking it twice in two places would let the two answers drift. Every form
 * that takes a date checks this and then says its own sentence about it.
 */
export function isCalendarDate(value: string): boolean {
  return /^\d{4}-\d{2}-\d{2}$/.test(value.trim());
}

/**
 * Today, as a calendar date in the reader's own timezone.
 *
 * `toISOString` would answer in UTC, which is a different day for most of the
 * evening in the Americas — a receipt entered at 9pm would default to
 * tomorrow's date.
 */
export function today(): string {
  return datePart(new Date());
}

/** The parts of an address, all optional: a record may hold only some. */
export interface AddressParts {
  address_line1?: string;
  city?: string;
  state?: string;
  postal_code?: string;
}

/**
 * The address on one line, skipping the parts that were never filled in.
 *
 * The index card leaves the postal code out and the record head puts it in,
 * which is the only difference between the two callers — so it is a field
 * that is absent rather than a second function.
 */
export function oneLineAddress(parts: AddressParts | undefined): string {
  if (!parts) return DASH;
  const region = [parts.city, parts.state, parts.postal_code].filter(Boolean).join(" ");
  return [parts.address_line1, region].filter(Boolean).join(", ") || DASH;
}

function datePart(at: Date): string {
  return `${at.getFullYear()}-${pad(at.getMonth() + 1)}-${pad(at.getDate())}`;
}

function pad(n: number): string {
  return String(n).padStart(2, "0");
}

/**
 * Renders a byte count as "1.2 MB".
 *
 * Decimal units, because that is what a mail client and an operating system
 * both report, and a document that Finder calls 2.4 MB should not read as
 * 2.3 MB here.
 */
export function bytes(count: number | null | undefined): string {
  if (count === null || count === undefined || !Number.isFinite(count)) return DASH;
  if (count < 1_000) return `${Math.trunc(count)} B`;

  const units = ["kB", "MB", "GB"];
  let value = count / 1_000;
  let unit = 0;
  while (value >= 1_000 && unit < units.length - 1) {
    value /= 1_000;
    unit += 1;
  }
  return `${value < 10 ? value.toFixed(1) : Math.round(value)} ${units[unit]}`;
}

/**
 * Renders how long ago something happened: "4 minutes ago", "just now".
 *
 * The register's head answers "is mail still arriving", and a reader answers
 * that from an interval rather than from a timestamp they have to subtract.
 * The exact time is still on the line beside it.
 */
export function ago(iso: string | undefined, now = Date.now()): string {
  if (!iso) return DASH;
  const at = new Date(iso).getTime();
  if (Number.isNaN(at)) return DASH;

  const seconds = Math.round((now - at) / 1_000);
  if (seconds < 0) return "just now";
  if (seconds < 45) return "just now";

  const steps: [number, string, string][] = [
    [60, "minute", "minutes"],
    [24, "hour", "hours"],
    [Infinity, "day", "days"],
  ];
  let value = seconds / 60;
  for (const [limit, one, many] of steps) {
    if (value < limit) {
      const whole = Math.round(value);
      return `${whole} ${whole === 1 ? one : many} ago`;
    }
    value /= limit;
  }
  return timestamp(iso);
}

/**
 * The day a register entry belongs to: "Wed 6 Aug", in the reader's timezone.
 *
 * A received-at is a real instant with a real timezone, unlike the calendar
 * dates that come off documents — so unlike `calendarDate`, this one converts.
 */
export function dayRule(iso: string): string {
  const at = new Date(iso);
  if (Number.isNaN(at.getTime())) return DASH;

  const days = ["Sun", "Mon", "Tue", "Wed", "Thu", "Fri", "Sat"];
  const months = [
    "Jan",
    "Feb",
    "Mar",
    "Apr",
    "May",
    "Jun",
    "Jul",
    "Aug",
    "Sep",
    "Oct",
    "Nov",
    "Dec",
  ];
  return `${days[at.getDay()]} ${at.getDate()} ${months[at.getMonth()]}`;
}

/** The key two timestamps share when they fall on the same local day. */
export function dayKey(iso: string): string {
  const at = new Date(iso);
  if (Number.isNaN(at.getTime())) return "";
  return `${at.getFullYear()}-${pad(at.getMonth() + 1)}-${pad(at.getDate())}`;
}

/** Just the hour and minute: "14:02". */
export function hourMinute(iso: string): string {
  const at = new Date(iso);
  if (Number.isNaN(at.getTime())) return DASH;
  return `${pad(at.getHours())}:${pad(at.getMinutes())}`;
}
