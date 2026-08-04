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

function datePart(at: Date): string {
  return `${at.getFullYear()}-${pad(at.getMonth() + 1)}-${pad(at.getDate())}`;
}

function pad(n: number): string {
  return String(n).padStart(2, "0");
}
