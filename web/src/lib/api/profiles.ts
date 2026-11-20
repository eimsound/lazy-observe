import { apiGet, apiGetResponse, ApiError, API_ROOT, IS_MOCK } from "./client";
import {
  normalizeFlameNode,
  normalizeProfileListResponse,
  normalizeProfileServicesResponse,
  type FlameNode,
  type ProfileListResponseDTO,
  type ProfileRecord,
  type ProfileServicesResponseDTO,
  type ProfileType,
} from "./profile-contract";
import dayjs from "../dayjs.ts";

// Mock data is only for VITE_MOCK builds; load it lazily so it never ships with real chunks.
const loadMockProfiles = () => import("./mock-profiles");

export { PROFILE_TYPES } from "./profile-contract";
export type {
  FlameNode,
  ProfileListResponseDTO,
  ProfileRecord,
  ProfileRecordDTO,
  ProfileServicesResponseDTO,
  ProfileType,
} from "./profile-contract";

/**
 * Continuous-profiling API (B9). The backend exposes pprof-derived profiles and
 * a pre-aggregated flamegraph tree. These endpoints live under /api (not
 * /api/v1) per the B9 contract. VITE_MOCK serves a deterministic offline
 * generator so the flamegraph page renders with no backend.
 *
 *   GET /api/profiles/services                                 -> { services: string[] }
 *   GET /api/profiles?service=&type=&start=&end=               -> { profiles: ProfileRecordDTO[] }
 *   GET /api/profiles/flamegraph?service=&type=&start=&end=    -> FlameNode (tree)
 *   GET /api/profiles/:id/raw                                  -> pprof download
 */

export interface ProfileQuery {
  service: string;
  type: ProfileType;
  /** Epoch seconds. */
  start: number;
  /** Epoch seconds. */
  end: number;
}

function profileQueryParams(q: ProfileQuery) {
  return {
    service: q.service,
    type: q.type,
    start: dayjs.unix(q.start).toISOString(),
    end: dayjs.unix(q.end).toISOString(),
  };
}

export async function getProfileServices(
  signal?: AbortSignal,
  project?: string,
): Promise<string[]> {
  if (IS_MOCK) return (await loadMockProfiles()).mockProfileServices();
  const response = await apiGet<ProfileServicesResponseDTO>("/profiles/services", {
    base: API_ROOT,
    signal,
    project,
  });
  return normalizeProfileServicesResponse(response);
}

export async function listProfiles(
  q: ProfileQuery,
  signal?: AbortSignal,
  project?: string,
): Promise<ProfileRecord[]> {
  if (IS_MOCK) return (await loadMockProfiles()).mockListProfiles(q);
  const response = await apiGet<ProfileListResponseDTO>("/profiles", {
    base: API_ROOT,
    params: profileQueryParams(q),
    signal,
    project,
  });
  return normalizeProfileListResponse(response);
}

export async function getFlamegraph(
  q: ProfileQuery,
  signal?: AbortSignal,
  project?: string,
): Promise<FlameNode | null> {
  if (IS_MOCK) return (await loadMockProfiles()).mockFlamegraph(q);
  try {
    const raw = await apiGet<unknown>("/profiles/flamegraph", {
      base: API_ROOT,
      params: profileQueryParams(q),
      signal,
      project,
    });
    return normalizeFlameNode(raw);
  } catch (err) {
    // Gateway returns 404 when no profiles exist in the queried window; map to
    // null so the UI transitions cleanly into the empty state instead of keeping
    // a stale flamegraph from a prior type/window.
    if (err instanceof ApiError && err.status === 404) {
      return null;
    }
    throw err;
  }
}

/**
 * Href for downloading a profile's raw pprof blob. In mock mode this is a
 * self-contained data: URL (there is no server); against a real backend it's
 * the `/api/profiles/:id/raw` endpoint (real downloads should use
 * `downloadRawProfile` so `X-LO-Project` is attached for non-default projects).
 */
export function profileRawHref(rec: ProfileRecord): string {
  if (IS_MOCK) {
    const body = `# mock pprof profile\nservice: ${rec.service}\ntype: ${rec.type}\nid: ${rec.id}\n` +
      "(This is a placeholder produced by VITE_MOCK; a real backend returns a gzipped pprof blob.)\n";
    return `data:text/plain;charset=utf-8,${encodeURIComponent(body)}`;
  }
  return `${API_ROOT}/profiles/${encodeURIComponent(rec.id)}/raw`;
}

/**
 * Fetches a raw pprof profile blob through the shared API client so the active
 * project scope (`X-LO-Project`) and session cookie are included on non-default
 * projects.
 */
export async function fetchRawProfileBlob(
  rec: ProfileRecord,
  project?: string,
  signal?: AbortSignal,
): Promise<Blob> {
  if (IS_MOCK) {
    const body = `# mock pprof profile\nservice: ${rec.service}\ntype: ${rec.type}\nid: ${rec.id}\n` +
      "(This is a placeholder produced by VITE_MOCK; a real backend returns a gzipped pprof blob.)\n";
    return new Blob([body], { type: "text/plain;charset=utf-8" });
  }
  const res = await apiGetResponse(`/profiles/${encodeURIComponent(rec.id)}/raw`, {
    base: API_ROOT,
    project,
    signal,
  });
  return res.blob();
}

export async function downloadRawProfile(
  rec: ProfileRecord,
  filename: string,
  project?: string,
): Promise<void> {
  const blob = await fetchRawProfileBlob(rec, project);
  const objectUrl = URL.createObjectURL(blob);
  try {
    const link = document.createElement("a");
    link.href = objectUrl;
    link.download = filename;
    document.body.appendChild(link);
    link.click();
    link.remove();
  } finally {
    URL.revokeObjectURL(objectUrl);
  }
}

/** Total sample count of a flamegraph tree (the root value). */
export function flameTotal(tree: FlameNode): number {
  return tree.value;
}

/** True when any node in the tree was clipped by the backend. */
export function hasTruncated(tree: FlameNode): boolean {
  if (tree.truncated) return true;
  return (tree.children ?? []).some(hasTruncated);
}

/** Total node count (for the header stat). */
export function countNodes(tree: FlameNode): number {
  return 1 + (tree.children ?? []).reduce((s, c) => s + countNodes(c), 0);
}
