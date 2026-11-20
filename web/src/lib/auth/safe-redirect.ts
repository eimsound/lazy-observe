function hasControlChar(value: string): boolean {
  for (let i = 0; i < value.length; i++) {
    const code = value.charCodeAt(i);
    if (code <= 0x1f || code === 0x7f) return true;
  }
  return false;
}

export interface SafeRedirectOptions {
  /** Base origin used for same-origin validation (defaults to window.location.origin). */
  origin?: string;
  /** Fallback path when the candidate is missing or unsafe. */
  fallback?: string;
  /** Path prefixes that must not be redirected to (prevents redirect loops). */
  disallowPrefixes?: readonly string[];
}

function defaultOrigin(): string {
  if (typeof window !== "undefined" && window.location?.origin) {
    return window.location.origin;
  }
  return "https://lazy-observe.invalid";
}

function matchesPrefix(pathname: string, prefix: string): boolean {
  const normalized = prefix.endsWith("/") && prefix.length > 1 ? prefix.slice(0, -1) : prefix;
  return pathname === normalized || pathname.startsWith(`${normalized}/`);
}

/**
 * Validate and normalize an in-app post-auth redirect target.
 *
 * Rejects protocol-relative URLs (`//evil.example`), backslash-prefixed paths
 * (`/\evil.example`), control characters that browsers strip during URL parsing,
 * cross-origin absolute URLs, and auth-page self-loops (`/login` by default).
 */
export function safeRedirect(
  candidate: string | null | undefined,
  options: SafeRedirectOptions = {},
): string {
  const fallback = options.fallback ?? "/";
  const disallowPrefixes = options.disallowPrefixes ?? ["/login"];
  if (typeof candidate !== "string") return fallback;

  const trimmed = candidate.trim();
  if (!trimmed || hasControlChar(trimmed)) return fallback;
  if (!trimmed.startsWith("/")) return fallback;
  if (trimmed.length > 1 && (trimmed[1] === "/" || trimmed[1] === "\\")) return fallback;
  if (trimmed.includes("\\")) return fallback;

  const baseOrigin = options.origin ?? defaultOrigin();
  let parsed: URL;
  try {
    parsed = new URL(trimmed, baseOrigin);
  } catch {
    return fallback;
  }

  if (parsed.origin !== baseOrigin) return fallback;
  if (!parsed.pathname.startsWith("/") || parsed.pathname.startsWith("//")) return fallback;
  if (disallowPrefixes.some((prefix) => matchesPrefix(parsed.pathname, prefix))) {
    return fallback;
  }

  return `${parsed.pathname}${parsed.search}${parsed.hash}`;
}
