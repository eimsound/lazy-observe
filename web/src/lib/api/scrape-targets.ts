import { apiGet, apiSend, IS_MOCK, API_ROOT } from "./client";
import {
  normalizeScrapeTargetListResponse,
  normalizeScrapeTargetResponse,
  normalizeTargetHealthResponse,
  type ScrapeTarget,
  type ScrapeTargetInput,
  type TargetHealth,
} from "./scrape-target-contract";

// Mock data is only for VITE_MOCK builds; load it lazily so it never ships with real chunks.
const loadMockInfra = () => import("./mock-infra");

export type {
  ScrapeTarget,
  ScrapeTargetInput,
  TargetHealth,
  TargetHealthStatus,
} from "./scrape-target-contract";

/**
 * vmagent scrape-target management API. Endpoints under /api:
 *   /api/scrape-targets                CRUD
 *   GET /api/scrape-targets/:id/health -> { status, last_scrape_at, scrape_job }
 * In VITE_MOCK mode an in-memory store backs every operation so the whole
 * surface is interactive offline.
 */

function delay<T>(value: T, ms = 200): Promise<T> {
  return new Promise((resolve) => setTimeout(() => resolve(value), ms));
}

export async function listScrapeTargets(signal?: AbortSignal): Promise<ScrapeTarget[]> {
  if (IS_MOCK) return delay([...(await loadMockInfra()).mockScrapeTargets]);
  return normalizeScrapeTargetListResponse(
    await apiGet<unknown>("/scrape-targets", { base: API_ROOT, signal }),
  );
}

export async function createScrapeTarget(input: ScrapeTargetInput): Promise<ScrapeTarget> {
  if (IS_MOCK) return delay((await loadMockInfra()).createMockTarget(input), 250);
  return normalizeScrapeTargetResponse(
    await apiSend<unknown>("POST", "/scrape-targets", input, { base: API_ROOT }),
  );
}

export async function updateScrapeTarget(
  id: string,
  input: ScrapeTargetInput,
): Promise<ScrapeTarget> {
  if (IS_MOCK) return delay((await loadMockInfra()).updateMockTarget(id, input), 250);
  return normalizeScrapeTargetResponse(
    await apiSend<unknown>("PATCH", `/scrape-targets/${id}`, input, { base: API_ROOT }),
  );
}

export async function deleteScrapeTarget(id: string): Promise<void> {
  if (IS_MOCK) {
    (await loadMockInfra()).deleteMockTarget(id);
    return delay(undefined);
  }
  await apiSend<void>("DELETE", `/scrape-targets/${id}`, undefined, { base: API_ROOT });
}

export async function getTargetHealth(
  id: string,
  signal?: AbortSignal,
): Promise<TargetHealth> {
  if (IS_MOCK) return delay((await loadMockInfra()).mockTargetHealth(id), 150);
  return normalizeTargetHealthResponse(
    await apiGet<unknown>(`/scrape-targets/${id}/health`, { base: API_ROOT, signal }),
  );
}
