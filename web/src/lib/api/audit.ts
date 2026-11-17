import { apiGet, API_ROOT } from "./client";

/**
 * Management audit trail API (admin-only). The backend persists allow-listed
 * management events; this client reads them newest-first with filters.
 */

export interface AuditEvent {
  id: number;
  action: string;
  actor_id?: number;
  actor_username: string;
  actor_role?: string;
  target_type: string;
  target_id?: number;
  target_name: string;
  target_role?: string;
  project_id?: number;
  project_slug?: string;
  changed_fields?: string;
  success: boolean;
  failure_reason?: string;
  http_method: string;
  http_path: string;
  created_at: string;
}

export interface AuditQuery {
  action?: string;
  actor?: string;
  target_type?: string;
  project_id?: number;
  before_id?: number;
  limit?: number;
}

export async function listAudit(query: AuditQuery = {}, signal?: AbortSignal): Promise<AuditEvent[]> {
  const params: Record<string, string | number | undefined> = {
    action: query.action || undefined,
    actor: query.actor || undefined,
    target_type: query.target_type || undefined,
    project_id: query.project_id,
    before_id: query.before_id,
    limit: query.limit ?? 100,
  };
  return apiGet<AuditEvent[]>("/audit", { base: API_ROOT, params, signal });
}
