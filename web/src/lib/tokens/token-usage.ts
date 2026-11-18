/**
 * Copyable token-usage snippets (ADR 0010).
 *
 * A project API token (`lo_…`) only sends telemetry; every read — query API,
 * MCP, Grafana — needs a user API token (`lou_…`) plus `X-LO-Project: <slug>`.
 * The strings below mirror `docs/deployment.md` (§Query API, MCP, and Grafana)
 * and the SDK DSN shape `<origin>/p/<slug>@<token>` (`docs/sdk/*.md`).
 *
 * Pure and Vite-free so it runs under `node --test`.
 */

/** Project the gateway serves when a read carries no `X-LO-Project` header. */
export const DEFAULT_PROJECT_SLUG = "default";

const trimOrigin = (origin: string) => origin.replace(/\/+$/, "");

/** The project a read targets: the selected slug, else the gateway default. */
export function readProjectSlug(slug: string | null | undefined): string {
  return (slug ?? "").trim() || DEFAULT_PROJECT_SLUG;
}

/** SDK DSN: `<origin>/p/<slug>@<token>`. */
export function projectDsn(origin: string, slug: string, token: string): string {
  return `${trimOrigin(origin)}/p/${readProjectSlug(slug)}@${token}`;
}

/** `lazy-observe mcp --dsn '<origin>/p/<slug>@<lou_token>'`. */
export function mcpCommand(origin: string, slug: string, token: string): string {
  return `lazy-observe mcp --dsn ${shellSingleQuoted(projectDsn(origin, slug, token))}`;
}

/**
 * Wrap a value in single quotes for POSIX shells. Slugs are not restricted to
 * quote-free characters (nor is a pasted token), so every `'` inside the value
 * closes the quote, escapes itself and reopens it: `it's` → `'it'\''s'`.
 */
export function shellSingleQuoted(value: string): string {
  return `'${value.replaceAll("'", "'\\''")}'`;
}

/** Grafana Prometheus data-source URL: the gateway root, with no path prefix. */
export function grafanaDataSourceUrl(origin: string): string {
  return trimOrigin(origin);
}

/** Grafana's Prometheus data source has no bearer field, so both headers are custom. */
export function grafanaHeaders(token: string, slug: string): { name: string; value: string }[] {
  return [
    { name: "Authorization", value: `Bearer ${token}` },
    { name: "X-LO-Project", value: readProjectSlug(slug) },
  ];
}

/** The headers as one copyable `Name: value` block, one per line. */
export function grafanaHeaderBlock(token: string, slug: string): string {
  return grafanaHeaders(token, slug)
    .map((header) => `${header.name}: ${header.value}`)
    .join("\n");
}
