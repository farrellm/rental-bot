/** One named readiness condition, as reported by /readyz and /api/v1/status. */
export interface Check {
  name: string;
  ok: boolean;
  detail?: string;
}

/** A migration recorded in schema_migrations. */
export interface Migration {
  version: number;
  name: string;
  checksum: string;
  applied_at: string;
}

/** The body of GET /api/v1/status. */
export interface Status {
  status: "operational" | "degraded";
  version: string;
  commit: string;
  build_date: string;
  go_version: string;
  started_at: string;
  uptime_seconds: number;
  schema_version: number;
  database: string;
  checks: Check[];
  migrations: Migration[];
  checked_at: string;
}

/** An RFC 7807 error body. Every error this API returns has this shape. */
export interface Problem {
  type: string;
  title: string;
  status: number;
  detail?: string;
  instance?: string;
}

/**
 * Reads the current process status.
 *
 * The endpoint answers 200 even while degraded — the condition is in the
 * body — so a non-200 here means something else went wrong.
 */
export async function fetchStatus(signal?: AbortSignal): Promise<Status> {
  const response = await fetch("/api/v1/status", {
    signal,
    headers: { Accept: "application/json" },
  });

  if (!response.ok) {
    throw new Error(await describeFailure(response));
  }
  return (await response.json()) as Status;
}

async function describeFailure(response: Response): Promise<string> {
  try {
    const problem = (await response.json()) as Problem;
    if (problem.detail) return problem.detail;
    if (problem.title) return problem.title;
  } catch {
    // Not a problem+json body; the status line is all there is.
  }
  return `The server answered ${response.status}.`;
}
