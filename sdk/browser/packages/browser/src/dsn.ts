export interface ParsedDsn {
  endpoint: string;
  project: string;
  token: string;
}

export interface ParseDsnOptions {
  /** Require the token to start with this prefix (e.g. `"lo_"` or `"los_"`). */
  requiredTokenPrefix?: string;
}

const EXPECTED = "DSN must match http(s)://<host>/p/<project>@<token>";
const CONTROL_OR_WHITESPACE = /[\u0000-\u001f\u007f\s]/;
const NON_INGEST_PREFIXES = ["los_", "lou_"] as const;

export function parseDsn(value: string, options: ParseDsnOptions = {}): ParsedDsn {
  if (typeof value !== "string" || !value.trim()) {
    throw new Error(EXPECTED);
  }
  let url: URL;
  try {
    url = new URL(value);
  } catch {
    // Never attach cause or echo the raw DSN so tokens cannot leak into logs.
    throw new Error(EXPECTED);
  }
  if (url.protocol !== "http:" && url.protocol !== "https:") {
    throw new Error("DSN must use http or https");
  }
  if (!url.host || url.username || url.password || url.search || url.hash) {
    throw new Error(EXPECTED);
  }
  const match = url.pathname.match(/^\/p\/([^/@]+)@([^/@]+)\/?$/);
  if (!match) {
    throw new Error(EXPECTED);
  }
  let project: string;
  let token: string;
  try {
    project = decodeURIComponent(match[1]);
    token = decodeURIComponent(match[2]);
  } catch {
    throw new Error(EXPECTED);
  }
  if (!project.trim() || !token.trim() || CONTROL_OR_WHITESPACE.test(token)) {
    throw new Error(EXPECTED);
  }
  if (options.requiredTokenPrefix) {
    if (!token.startsWith(options.requiredTokenPrefix) || token.length <= options.requiredTokenPrefix.length) {
      throw new Error(`DSN token must start with ${options.requiredTokenPrefix}`);
    }
  } else if (NON_INGEST_PREFIXES.some((prefix) => token.startsWith(prefix))) {
    throw new Error("DSN for browser ingest requires a project API token (lo_...), not a user or source map upload token");
  }
  return {
    endpoint: url.origin,
    project,
    token,
  };
}

/** Return a log-safe DSN string with the token replaced by `[redacted]`. */
export function redactDsn(value: string): string {
  try {
    const parsed = parseDsn(value);
    return `${parsed.endpoint}/p/${encodeURIComponent(parsed.project)}@[redacted]`;
  } catch {
    return "[invalid DSN]";
  }
}
