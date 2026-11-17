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
