/**
 * The fetch wrapper every call goes through.
 *
 * It does three things the callers should not each have to remember: echo the
 * CSRF cookie into the header on mutations, turn an RFC 7807 body into an
 * error that says what happened, and mark a 401 so the router can send the
 * operator to sign in rather than showing them an empty screen.
 */

import type { Problem } from "./types";

/** The CSRF cookie the server sets. It is readable on purpose. */
const CSRF_COOKIE = "rb_csrf";
const CSRF_HEADER = "X-CSRF-Token";

/**
 * A failed request, carrying enough to decide what to do about it.
 *
 * `detail` is the server's own sentence. It is written for the operator, so
 * screens show it rather than inventing their own wording.
 */
export class ApiError extends Error {
  readonly status: number;
  readonly detail: string;

  constructor(status: number, detail: string) {
    super(detail);
    this.name = "ApiError";
    this.status = status;
    this.detail = detail;
  }

  /** True when the session is gone and signing in again is the fix. */
  get isUnauthenticated(): boolean {
    return this.status === 401;
  }
}

function readCookie(name: string): string {
  const prefix = `${name}=`;
  for (const part of document.cookie.split("; ")) {
    if (part.startsWith(prefix)) return decodeURIComponent(part.slice(prefix.length));
  }
  return "";
}

const SAFE_METHODS = new Set(["GET", "HEAD", "OPTIONS"]);

interface RequestOptions {
  method?: string;
  body?: unknown;
  signal?: AbortSignal;
}

export async function request<T>(path: string, options: RequestOptions = {}): Promise<T> {
  const method = options.method ?? "GET";
  const headers: Record<string, string> = { Accept: "application/json" };

  // FormData carries its own multipart Content-Type, boundary and all, and the
  // browser sets it. Naming it here would drop the boundary and the server
  // would see an unparseable body.
  const isForm = options.body instanceof FormData;
  if (options.body !== undefined && !isForm) {
    headers["Content-Type"] = "application/json";
  }
  if (!SAFE_METHODS.has(method)) {
    headers[CSRF_HEADER] = readCookie(CSRF_COOKIE);
  }

  let response: Response;
  try {
    response = await fetch(path, {
      method,
      headers,
      signal: options.signal,
      body: options.body === undefined ? undefined : isForm ? (options.body as FormData) : JSON.stringify(options.body),
      // The session is a cookie; same-origin is the default but saying so
      // keeps it true if this is ever served from somewhere else.
      credentials: "same-origin",
    });
  } catch (err) {
    if (err instanceof DOMException && err.name === "AbortError") throw err;
    // fetch rejects with a TypeError when it never reached the server at all.
    throw new ApiError(0, "No contact with the server. The process may be stopped.");
  }

  if (!response.ok) {
    throw new ApiError(response.status, await describeFailure(response));
  }

  // 204, which logout and delete both answer with.
  if (response.status === 204) return undefined as T;
  return (await response.json()) as T;
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

/** Renders any thrown value as something worth showing a person. */
export function describeError(error: unknown): string {
  if (error instanceof ApiError) return error.detail;
  if (error instanceof Error && error.message) return error.message;
  return "Something went wrong.";
}
