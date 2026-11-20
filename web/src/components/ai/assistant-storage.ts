import type { AiSignal, AssistantAnswer } from "@/lib/api/ai-contract";

export interface AssistantTurn {
  id: string;
  question: string;
  /** The signal the user requested for this turn (auto/metrics/logs). */
  requestedSignal: AiSignal;
  status: "pending" | "done" | "error";
  answer?: AssistantAnswer;
  /** HTTP status of a failed request (429 -> rate limited). */
  errorStatus?: number;
  errorMessage?: string;
}

export interface AssistantStorageLike {
  getItem(key: string): string | null;
  setItem(key: string, value: string): void;
  removeItem(key: string): void;
}

export const ASSISTANT_HISTORY_LEGACY_KEY = "lo-assistant-history";
export const ASSISTANT_HISTORY_PREFIX = "lo-assistant-history:";
export const ASSISTANT_MAX_TURNS = 20;

function defaultStorage(): AssistantStorageLike | null {
  try {
    return globalThis.localStorage ?? null;
  } catch {
    return null;
  }
}

/**
 * Scope assistant history by both signed-in user and selected project so
 * switching projects or accounts never leaks prior query results.
 */
export function assistantHistoryKey(
  user: { id?: number | string; username?: string } | null | undefined,
  projectSlug: string,
): string | null {
  const userPart = user?.id !== undefined && user?.id !== null
    ? `id:${String(user.id)}`
    : user?.username?.trim()
      ? `u:${user.username.trim()}`
      : "";
  const projectPart = projectSlug.trim();
  if (!userPart || !projectPart) return null;
  return `${ASSISTANT_HISTORY_PREFIX}${encodeURIComponent(userPart)}:${encodeURIComponent(projectPart)}`;
}

/** Only completed/failed turns are worth persisting (drop in-flight ones). */
export function loadAssistantHistory(
  key: string | null,
  storage: AssistantStorageLike | null = defaultStorage(),
): AssistantTurn[] {
  if (!key || !storage) return [];
  try {
    const raw = storage.getItem(key);
    if (!raw) return [];
    const parsed = JSON.parse(raw) as AssistantTurn[];
    if (!Array.isArray(parsed)) return [];
    return parsed.filter((t) => t && typeof t === "object" && t.status !== "pending").slice(-ASSISTANT_MAX_TURNS);
  } catch {
    return [];
  }
}

export function saveAssistantHistory(
  key: string | null,
  turns: AssistantTurn[],
  storage: AssistantStorageLike | null = defaultStorage(),
): void {
  if (!key || !storage) return;
  try {
    const persistable = turns.filter((t) => t.status !== "pending").slice(-ASSISTANT_MAX_TURNS);
    if (persistable.length === 0) {
      storage.removeItem(key);
      return;
    }
    storage.setItem(key, JSON.stringify(persistable));
  } catch {
    /* ignore (private mode / disabled storage) */
  }
}

export function clearAssistantHistory(
  key: string | null,
  storage: AssistantStorageLike | null = defaultStorage(),
): void {
  if (!key || !storage) return;
  try {
    storage.removeItem(key);
  } catch {
    /* ignore */
  }
}
