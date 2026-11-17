/**
 * Client-side settings persistence.
 *
 * The browser holds an httpOnly session cookie. Data reads are scoped by the
 * selected project, sent as the `X-LO-Project: <slug>` header. The selection
 * is per tab; the last choice persists only as the default for new tabs.
 */

import { createProjectScope, type SlugStorage } from "@/lib/project/project-scope";

const scope = createProjectScope(browserStorage());

function browserStorage(): SlugStorage | null {
  try {
    return globalThis.localStorage ?? null;
  } catch {
    return null;
  }
}

/** This tab's selected project slug, sent as `X-LO-Project`. */
export function getProjectSlug(): string {
  return scope.get();
}

export function setProjectSlug(slug: string): void {
  scope.set(slug);
}

/** Whether the app is running against the built-in mock backend. */
export const IS_MOCK = import.meta.env.VITE_MOCK === "1";
