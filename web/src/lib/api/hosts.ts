import { apiGet, IS_MOCK } from "./client";
import { normalizeHostsResponse, type Host } from "./host-contract";

// Mock data is only for VITE_MOCK builds; load it lazily so it never ships with real chunks.
const loadMockInfra = () => import("./mock-infra");
export {
  loadRating,
  normalizeHostsResponse,
  usageRating,
  type Host,
  type UsageRating,
} from "./host-contract";

/**
 * Host monitoring API. Endpoint under /api:
 *   GET /api/hosts -> Host[]
 * A host row is a compact node_exporter roll-up used to render the hosts
 * overview; drilling into a host opens its metric charts.
 */

function delay<T>(value: T, ms = 200): Promise<T> {
  return new Promise((resolve) => setTimeout(() => resolve(value), ms));
}

export async function listHosts(signal?: AbortSignal): Promise<Host[]> {
  if (IS_MOCK) return delay([...(await loadMockInfra()).mockHosts]);
  return normalizeHostsResponse(await apiGet<unknown>("/hosts", { base: "/api", signal }));
}
