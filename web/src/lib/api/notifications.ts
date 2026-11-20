import { apiGet, apiSend, API_ROOT } from "./client";

/**
 * Notification delivery history (Outbox) API. Scoped to the selected project;
 * requires member or admin role.
 */

export type NotificationStatus = "pending" | "processing" | "delivered" | "dead";

export interface NotificationItem {
  id: number;
  project_id: number;
  rule_id: number | null;
  channel_id: number;
  channel_name: string;
  channel_type: string;
  priority: number;
  status: NotificationStatus;
  attempts: number;
  last_error: string;
  available_at: string;
  created_at: string;
  delivered_at: string | null;
}

export interface NotificationQuery {
  status?: NotificationStatus;
  channel_id?: number;
  before_id?: number;
  limit?: number;
}

export async function listNotifications(
  query: NotificationQuery = {},
  signal?: AbortSignal,
  project?: string,
): Promise<NotificationItem[]> {
  const params: Record<string, string | number | undefined> = {
    status: query.status,
    channel_id: query.channel_id,
    before_id: query.before_id,
    limit: query.limit ?? 100,
  };
  return apiGet<NotificationItem[]>("/notifications", { base: API_ROOT, params, signal, project });
}

export async function retryNotification(id: number, project?: string): Promise<void> {
  await apiSend<void>("POST", `/notifications/${id}/retry`, undefined, { base: API_ROOT, project });
}
