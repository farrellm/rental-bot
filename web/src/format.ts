/** Formatting for the value column. Every result is fixed-width friendly. */

const EM_DASH = "—";

/** Renders a duration in seconds as "00:14:22", or "3d 04:12:07" past a day. */
export function uptime(seconds: number): string {
  if (!Number.isFinite(seconds) || seconds < 0) return EM_DASH;

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
  if (Number.isNaN(at.getTime())) return EM_DASH;

  return `${datePart(at)} ${pad(at.getHours())}:${pad(at.getMinutes())}`;
}

/** Renders just the wall-clock time, "21:04:12". */
export function clock(iso: string): string {
  const at = new Date(iso);
  if (Number.isNaN(at.getTime())) return EM_DASH;

  return `${pad(at.getHours())}:${pad(at.getMinutes())}:${pad(at.getSeconds())}`;
}

/** Shortens a commit hash to the length people actually read. */
export function commit(hash: string): string {
  if (!hash || hash === "unknown") return EM_DASH;
  return hash.length > 7 ? hash.slice(0, 7) : hash;
}

/** Falls back to an em dash so the column never shows an empty cell. */
export function orDash(value: string | undefined): string {
  return value && value !== "unknown" ? value : EM_DASH;
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
  if (cents === null || cents === undefined || !Number.isFinite(cents)) return EM_DASH;

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
  if (value === null || value === undefined || !Number.isFinite(value)) return EM_DASH;
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
  return value ? value : EM_DASH;
}

function datePart(at: Date): string {
  return `${at.getFullYear()}-${pad(at.getMonth() + 1)}-${pad(at.getDate())}`;
}

function pad(n: number): string {
  return String(n).padStart(2, "0");
}
