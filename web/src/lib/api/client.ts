import { getProjectSlug, IS_MOCK } from "@/lib/settings";

/**
 * Thin API abstraction. All backend access funnels through here so that:
 *  - project scope is attached in exactly one place;
 *  - VITE_MOCK swaps the transport without any caller change;
 *  - the base path (`/api/v1` for data, `/api` for platform) is defined once.
 *
 * Identity (see web/README.md §API): every request rides the httpOnly session
 * cookie (`credentials: include`). Data reads are additionally scoped by the
 * selected project via `X-LO-Project`.
 */

export const API_BASE = "/api/v1";
/** Platform/management + auth endpoints live under /api (not /api/v1). */
export const API_ROOT = "/api";

/** Error carrying the backend's structured message + HTTP status when present. */
export class ApiError extends Error {
  status: number;
  /** Stable machine-readable error code from the backend, when present. */
  code?: string;
  constructor(message: string, status = 0, code?: string) {
    super(message);
    this.name = "ApiError";
    this.status = status;
    this.code = code;
  }
}

export interface RequestOptions {
  params?: Record<string, string | number | undefined>;
  signal?: AbortSignal;
  /** Override the base path (defaults to /api/v1). */
  base?: string;
}

export type FormFieldValue = string | number | boolean;
export type FormFields = Record<
  string,
  FormFieldValue | readonly FormFieldValue[] | null | undefined
>;

export interface FormRequestOptions extends Pick<RequestOptions, "base" | "signal"> {
  /** Value for the Accept header. It is intentionally optional for raw streams. */
  accept?: string;
}

/** Attach the selected project scope in one place. */
function identityHeaders(): Record<string, string> {
  const headers: Record<string, string> = {};
  const project = getProjectSlug();
  if (project) headers["X-LO-Project"] = project;
  return headers;
}

/**
 * Echo the double-submit CSRF cookie back through the X-CSRF-Token header on
 * mutating requests. The cookie is set at login when the backend enables CSRF;
 * when it is absent the header is simply omitted (compatible with both modes).
 */
function csrfHeaders(): Record<string, string> {
  const match = document.cookie.match(/(?:^|;\s*)lo_csrf=([^;]+)/);
  if (!match) return {};
  return { "X-CSRF-Token": decodeURIComponent(match[1]) };
}

function buildUrl(base: string, path: string, params?: RequestOptions["params"]): string {
  const url = new URL(base + path, window.location.origin);
  if (params) {
    for (const [k, v] of Object.entries(params)) {
      if (v !== undefined) url.searchParams.set(k, String(v));
    }
  }
  return url.pathname + url.search;
}

async function parse(res: Response): Promise<unknown> {
  const text = await res.text();
  if (!text) return undefined;
  try {
    return JSON.parse(text);
  } catch {
    return text;
  }
}

function toError(body: unknown, status: number): ApiError {
  const msg =
    (body as { error?: string; message?: string })?.error ??
    (body as { message?: string })?.message ??
    (typeof body === "string" && body ? body : `HTTP ${status}`);
  const code = (body as { code?: unknown })?.code;
  return new ApiError(msg, status, typeof code === "string" ? code : undefined);
}

export async function apiGet<T>(path: string, opts: RequestOptions = {}): Promise<T> {
  const headers: Record<string, string> = {
    Accept: "application/json",
    ...identityHeaders(),
  };

  let res: Response;
  try {
    res = await fetch(buildUrl(opts.base ?? API_BASE, path, opts.params), {
      method: "GET",
      headers,
      credentials: "include",
      signal: opts.signal,
    });
  } catch (e) {
    if (e instanceof DOMException && e.name === "AbortError") throw e;
    throw new ApiError((e as Error).message || "network error", 0);
  }

  const body = await parse(res);
  if (!res.ok) throw toError(body, res.status);
  return body as T;
}

/** Send a JSON body with an arbitrary method (POST/PATCH/PUT/DELETE). */
export async function apiSend<T>(
  method: "POST" | "PATCH" | "PUT" | "DELETE",
  path: string,
  body?: unknown,
  opts: RequestOptions = {},
): Promise<T> {
  const headers: Record<string, string> = {
    Accept: "application/json",
    ...identityHeaders(),
    ...csrfHeaders(),
  };
  if (body !== undefined) headers["Content-Type"] = "application/json";

  let res: Response;
  try {
    res = await fetch(buildUrl(opts.base ?? API_ROOT, path, opts.params), {
      method,
      headers,
      credentials: "include",
      body: body !== undefined ? JSON.stringify(body) : undefined,
      signal: opts.signal,
    });
  } catch (e) {
    if (e instanceof DOMException && e.name === "AbortError") throw e;
    throw new ApiError((e as Error).message || "network error", 0);
  }

  const parsed = await parse(res);
  if (!res.ok) throw toError(parsed, res.status);
  return parsed as T;
}

function encodeFormFields(fields: FormFields | URLSearchParams): URLSearchParams {
  if (fields instanceof URLSearchParams) return new URLSearchParams(fields);

  const form = new URLSearchParams();
  for (const [key, value] of Object.entries(fields)) {
    if (value === undefined || value === null) continue;
    const values = Array.isArray(value) ? value : [value];
    for (const item of values) form.append(key, String(item));
  }
  return form;
}

/**
 * POST an `application/x-www-form-urlencoded` body and leave a successful
 * response untouched. This is the transport for streaming and downloadable
 * endpoints: callers retain ownership of `response.body`.
 *
 * Error responses are consumed here so every caller receives the same
 * structured ApiError behavior as apiGet/apiSend.
 */
export async function apiPostFormResponse(
  path: string,
  fields: FormFields | URLSearchParams,
  opts: FormRequestOptions = {},
): Promise<Response> {
  const headers: Record<string, string> = {
    "Content-Type": "application/x-www-form-urlencoded",
    ...identityHeaders(),
  };
  if (opts.accept) headers.Accept = opts.accept;

  let res: Response;
  try {
    res = await fetch(buildUrl(opts.base ?? API_BASE, path), {
      method: "POST",
      headers,
      credentials: "include",
      body: encodeFormFields(fields).toString(),
      signal: opts.signal,
    });
  } catch (e) {
    if (e instanceof DOMException && e.name === "AbortError") throw e;
    throw new ApiError((e as Error).message || "network error", 0);
  }

  if (!res.ok) throw toError(await parse(res), res.status);
  return res;
}

/**
 * POST an `application/x-www-form-urlencoded` body and return the RAW response
 * text. Needed for the logs query endpoint, which streams JSON-lines (one JSON
 * object per line) rather than a single JSON document — the caller parses it.
 * Shares the single identity-header + error-handling path with apiGet.
 */
export async function apiPostForm(
  path: string,
  fields: FormFields | URLSearchParams,
  opts: FormRequestOptions = {},
): Promise<string> {
  const res = await apiPostFormResponse(path, fields, opts);
  return res.text();
}

/** Re-exported for callers that need to branch on mock mode (e.g. metrics API). */
export { IS_MOCK };
