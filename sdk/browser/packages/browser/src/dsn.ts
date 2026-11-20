export interface ParsedDsn { endpoint: string; project: string; token: string }

export function parseDsn(value: string): ParsedDsn {
  let url: URL;
  try { url = new URL(value); } catch { throw new Error("Invalid lazy-observe DSN URL"); }
  const match = url.pathname.match(/^\/p\/([^/@]+)@([^/]+)\/?$/);
  if (!match || url.search || url.hash) throw new Error("DSN must match <origin>/p/<project>@<token>");
  if (url.protocol !== "http:" && url.protocol !== "https:") throw new Error("DSN must use http or https");
  let project: string;
  let token: string;
  try {
    project = decodeURIComponent(match[1]);
    token = decodeURIComponent(match[2]);
  } catch {
    throw new Error("DSN must match <origin>/p/<project>@<token>");
  }
  return {
    endpoint: url.origin,
    project,
    token,
  };
}
