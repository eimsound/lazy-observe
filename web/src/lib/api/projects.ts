import { apiGet, apiSend, IS_MOCK, API_ROOT } from "./client";

// Mock data is only for VITE_MOCK builds; load it lazily so it never ships with real chunks.
const loadMockAdmin = () => import("./mock-admin");

/**
 * Project & API-token management API (B1). Endpoints live under /api (not
 * /api/v1). A token's secret value is returned exactly once, on creation.
 */

export interface Project {
  id: string;
  name: string;
  slug: string;
  created_at: string;
}

export interface ProjectAPIToken {
  id: string;
  name: string;
  /** Non-secret display prefix, safe to show in the list. */
  prefix: string;
  created_at: string;
  last_used_at: string | null;
  revoked: boolean;
}

/** Returned only from the create call — carries the full secret once. */
export interface IssuedProjectAPIToken extends ProjectAPIToken {
  value: string;
}

function delay<T>(value: T, ms = 200): Promise<T> {
  return new Promise((resolve) => setTimeout(() => resolve(value), ms));
}

export async function listProjects(signal?: AbortSignal): Promise<Project[]> {
  if (IS_MOCK) return delay([...(await loadMockAdmin()).mockProjects]);
  return apiGet<Project[]>("/projects", { base: API_ROOT, signal });
}

export async function createProject(name: string, slug: string): Promise<Project> {
  if (IS_MOCK) return delay((await loadMockAdmin()).createMockProject(name, slug), 250);
  return apiSend<Project>("POST", "/projects", { name, slug }, { base: API_ROOT });
}

export async function updateProject(
  projectId: string,
  name: string,
  slug: string,
): Promise<Project> {
  if (IS_MOCK) return delay((await loadMockAdmin()).updateMockProject(projectId, name, slug), 250);
  return apiSend<Project>("PATCH", `/projects/${projectId}`, { name, slug }, { base: API_ROOT });
}

export async function deleteProject(projectId: string): Promise<void> {
  if (IS_MOCK) {
    (await loadMockAdmin()).deleteMockProject(projectId);
    return delay(undefined, 200);
  }
  await apiSend<void>("DELETE", `/projects/${projectId}`, undefined, { base: API_ROOT });
}

export async function listProjectAPITokens(
  projectId: string,
  signal?: AbortSignal,
): Promise<ProjectAPIToken[]> {
  if (IS_MOCK) return delay([...((await loadMockAdmin()).mockTokens[projectId] ?? [])]);
  return apiGet<ProjectAPIToken[]>(`/projects/${projectId}/tokens`, {
    base: API_ROOT,
    signal,
  });
}

export async function createProjectAPIToken(
  projectId: string,
  name: string,
): Promise<IssuedProjectAPIToken> {
  if (IS_MOCK) return delay((await loadMockAdmin()).createMockProjectAPIToken(projectId, name), 250);
  return apiSend<IssuedProjectAPIToken>(
    "POST",
    `/projects/${projectId}/tokens`,
    { name },
    { base: API_ROOT },
  );
}

export async function revokeProjectAPIToken(projectId: string, tokenId: string): Promise<void> {
  if (IS_MOCK) {
    (await loadMockAdmin()).revokeMockProjectAPIToken(projectId, tokenId);
    return delay(undefined, 200);
  }
  await apiSend<void>("DELETE", `/projects/${projectId}/tokens/${tokenId}`, undefined, {
    base: API_ROOT,
  });
}
