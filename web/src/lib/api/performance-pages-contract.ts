/** Prometheus-compatible `/api/v1/series` response used for Web Vital page discovery. */
export interface VitalPagesSeriesResponseDTO {
  status: "success";
  data: Array<Record<string, string>>;
}

export type VitalPagesTransport = (signal?: AbortSignal) => Promise<unknown>;

/**
 * Execute page discovery through an injected transport. Keeping this boundary
 * explicit makes AbortSignal forwarding and response validation independently
 * testable from the browser client.
 */
export async function requestVitalPages(
  transport: VitalPagesTransport,
  signal?: AbortSignal,
): Promise<string[]> {
  return normalizeVitalPagesResponse(await transport(signal));
}

/** Validate the series envelope, then return sorted, unique, non-empty pages. */
export function normalizeVitalPagesResponse(value: unknown): string[] {
  const response = objectValue(value, "Web Vitals page response");
  if (response.status === "error") {
    const detail = typeof response.error === "string" && response.error.trim() !== ""
      ? `: ${response.error}`
      : "";
    throw new TypeError(`Web Vitals page query failed${detail}`);
  }
  if (response.status !== "success") {
    throw new TypeError("Web Vitals page response status must be success");
  }
  if (!Array.isArray(response.data)) {
    throw new TypeError("Web Vitals page response data must be an array");
  }

  const pages = new Set<string>();
  for (let index = 0; index < response.data.length; index += 1) {
    const labels = objectValue(response.data[index], `Web Vitals page response data[${index}]`);
    for (const [name, label] of Object.entries(labels)) {
      if (typeof label !== "string") {
        throw new TypeError(`Web Vitals page response label ${name} must be a string`);
      }
    }
    const page = labels.page;
    if (typeof page === "string" && page.trim() !== "") pages.add(page);
  }
  return [...pages].sort((a, b) => a.localeCompare(b));
}

function objectValue(value: unknown, path: string): Record<string, unknown> {
  if (typeof value !== "object" || value === null || Array.isArray(value)) {
    throw new TypeError(`${path} must be an object`);
  }
  return value as Record<string, unknown>;
}
