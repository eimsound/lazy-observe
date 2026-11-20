export function randomHex(bytes: number): string {
  const data = new Uint8Array(bytes);
  globalThis.crypto?.getRandomValues?.(data);
  if (!data.some(Boolean)) for (let i = 0; i < data.length; i++) data[i] = Math.floor(Math.random() * 256);
  return Array.from(data, (value) => value.toString(16).padStart(2, "0")).join("");
}

export function eventId(): string {
  return globalThis.crypto?.randomUUID?.() ?? `${randomHex(4)}-${randomHex(2)}-4${randomHex(2).slice(1)}-a${randomHex(2).slice(1)}-${randomHex(6)}`;
}

export function normalizePage(value: string): string {
  try {
    const url = new URL(value, typeof location === "undefined" ? "http://localhost" : location.href);
    return url.pathname || "/";
  } catch {
    return value.split(/[?#]/, 1)[0] || "/";
  }
}

export { sanitizeUrl } from "./sanitize";

export function safeJsonStringify(value: unknown, maxLength = 2_048): string {
  const seen = new WeakSet<object>();
  try {
    const serialized = JSON.stringify(value, (_key, current: unknown) => {
      if (typeof current === "bigint") return current.toString();
      if (current instanceof Error) return { name: current.name, message: current.message };
      if (typeof current === "object" && current !== null) {
        if (seen.has(current)) return "[Circular]";
        seen.add(current);
      }
      return current;
    });
    if (serialized === undefined) return String(value);
    return serialized.length > maxLength ? `${serialized.slice(0, maxLength)}...` : serialized;
  } catch {
    return String(value);
  }
}

export function formatNonError(value: unknown): string {
  if (typeof value === "string") return value;
  if (value === null || value === undefined) return String(value);
  if (typeof value === "object") {
    const record = value as Record<string, unknown>;
    if (typeof record.message === "string" && record.message.trim()) return record.message;
    const json = safeJsonStringify(value, 1_024);
    if (json && json !== "{}") return json;
  }
  return String(value);
}

export function safeSessionStorage(): Storage | undefined {
  try {
    return typeof sessionStorage === "undefined" ? undefined : sessionStorage;
  } catch {
    return undefined;
  }
}

export function getSessionId(storage: Storage | undefined): string {
  const key = "lazy-observe:session-id";
  try {
    const existing = storage?.getItem(key);
    if (existing) return existing;
    const id = eventId();
    storage?.setItem(key, id);
    return id;
  } catch { return eventId(); }
}
