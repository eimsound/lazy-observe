export interface ParsedDSN {
  endpoint: string;
  project: string;
  token: string;
}

export interface ParseDSNOptions {
  /** Require the token to start with this prefix (e.g. `"lo_"` or `"los_"`). */
  requiredTokenPrefix?: string;
}

const expected = "expected http(s)://host/p/<slug>@<token>";
const CONTROL_OR_WHITESPACE = /[\u0000-\u001f\u007f\s]/;
const NON_INGEST_PREFIXES = ["los_", "lou_"] as const;

export function parseDSN(raw: string, options: ParseDSNOptions = {}): ParsedDSN {
  if (typeof raw !== "string" || !raw.trim()) {
    throw new Error(`parse DSN: ${expected}`);
  }
  let url: URL;
  try {
    url = new URL(raw);
  } catch {
    // Do not attach the original TypeError as cause: Node sets error.input = raw,
    // which would leak plaintext tokens into host logs.
    throw new Error(`parse DSN: ${expected}`);
  }
  if (
    (url.protocol !== "http:" && url.protocol !== "https:")
    || !url.host
    || url.username
    || url.password
    || url.search
    || url.hash
  ) {
    throw new Error(`parse DSN: ${expected}`);
  }
  const match = /^\/p\/([^/@]+)@([^/@]+)\/?$/.exec(url.pathname);
  if (!match) throw new Error(`parse DSN: ${expected}`);
  let project: string;
  let token: string;
  try {
    project = decodeURIComponent(match[1]!);
    token = decodeURIComponent(match[2]!);
  } catch {
    throw new Error("parse DSN: invalid project slug or token");
  }
  if (!project.trim() || !token.trim() || CONTROL_OR_WHITESPACE.test(token)) {
    throw new Error(`parse DSN: ${expected}`);
  }
  if (options.requiredTokenPrefix) {
    if (!token.startsWith(options.requiredTokenPrefix) || token.length <= options.requiredTokenPrefix.length) {
      throw new Error(`parse DSN: token must start with ${options.requiredTokenPrefix}`);
    }
  } else if (NON_INGEST_PREFIXES.some((prefix) => token.startsWith(prefix))) {
    throw new Error("parse DSN: runtime ingest requires a project API token (lo_...), not a user or source map upload token");
  }
  return { endpoint: url.origin, project, token };
}

/** Return a log-safe representation of a DSN without ever exposing its token. */
export function redactDSN(raw: string): string {
  try {
    const parsed = parseDSN(raw);
    return `${parsed.endpoint}/p/${encodeURIComponent(parsed.project)}@[redacted]`;
  } catch {
    return "[invalid DSN]";
  }
}
