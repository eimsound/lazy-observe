export interface ParsedDSN {
  endpoint: string;
  project: string;
  token: string;
}

const expected = "expected http(s)://host/p/<slug>@<token>";

export function parseDSN(raw: string): ParsedDSN {
  let url: URL;
  try {
    url = new URL(raw);
  } catch (error) {
    throw new Error(`parse DSN: ${expected}`, { cause: error });
  }
  if ((url.protocol !== "http:" && url.protocol !== "https:") || !url.host || url.username || url.password || url.search || url.hash) {
    throw new Error(`parse DSN: ${expected}`);
  }
  const match = /^\/p\/([^/@]+)@([^/@]+)$/.exec(url.pathname);
  if (!match) throw new Error(`parse DSN: ${expected}`);
  let project: string;
  let token: string;
  try {
    project = decodeURIComponent(match[1]!);
    token = decodeURIComponent(match[2]!);
  } catch (error) {
    throw new Error("parse DSN: invalid project slug or token", { cause: error });
  }
  if (!project || !token.trim()) throw new Error(`parse DSN: ${expected}`);
  return { endpoint: url.origin, project, token };
}
