import { ApiError, API_ROOT, apiGet } from "./client";
import {
  normalizeHealthDiagnostics,
  normalizeInboxDiagnostics,
  type HealthDiagnostics,
  type InboxDiagnostics,
} from "./system-contract";

export * from "./system-contract";

export async function getHealthDiagnostics(signal?: AbortSignal): Promise<HealthDiagnostics> {
  let response: Response;
  try {
    response = await fetch("/healthz", { credentials: "include", signal });
  } catch (error) {
    if (error instanceof DOMException && error.name === "AbortError") throw error;
    throw new ApiError("health diagnostics unavailable", 0);
  }
  let body: unknown;
  try {
    body = await response.json();
  } catch {
    throw new ApiError("health diagnostics unavailable", response.status);
  }
  return normalizeHealthDiagnostics(body);
}

export async function getInboxDiagnostics(signal?: AbortSignal): Promise<InboxDiagnostics> {
  const body = await apiGet<unknown>("/system/error-inbox", { base: API_ROOT, signal });
  return normalizeInboxDiagnostics(body);
}
