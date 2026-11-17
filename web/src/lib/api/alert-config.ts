import { apiGet, apiSend, IS_MOCK, API_ROOT } from "./client";
import { normalizeAlertConfigResponse, type AlertConfig } from "./alert-config-contract";

// Mock data is only for VITE_MOCK builds; load it lazily so it never ships with real chunks.
const loadMockInfra = () => import("./mock-infra");

export type { AlertConfig } from "./alert-config-contract";

/**
 * Per-project alerting configuration API. Endpoints under /api:
 *   GET /api/projects/:id/alert-config
 *   PUT /api/projects/:id/alert-config
 * Controls the project-level notification policy: whether new issues and
 * regressions notify, the error-rate threshold, and which channels receive
 * them (channel ids reuse the F2 channel data source).
 */

function delay<T>(value: T, ms = 200): Promise<T> {
  return new Promise((resolve) => setTimeout(() => resolve(value), ms));
}

export async function getAlertConfig(
  projectId: string,
  signal?: AbortSignal,
): Promise<AlertConfig> {
  if (IS_MOCK) return delay((await loadMockInfra()).getMockAlertConfig(projectId));
  return normalizeAlertConfigResponse(
    await apiGet<unknown>(`/projects/${projectId}/alert-config`, {
      base: API_ROOT,
      signal,
    }),
  );
}

export async function putAlertConfig(
  projectId: string,
  config: AlertConfig,
): Promise<AlertConfig> {
  if (IS_MOCK) return delay((await loadMockInfra()).setMockAlertConfig(projectId, config), 300);
  return normalizeAlertConfigResponse(
    await apiSend<unknown>("PUT", `/projects/${projectId}/alert-config`, config, {
      base: API_ROOT,
    }),
  );
}
