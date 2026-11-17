export type LoginErrorTranslationKey =
  | "auth.invalidCredentials"
  | "auth.rateLimited"
  | "auth.serverError"
  | "auth.networkError"
  | "auth.signInError";

/** Map transport status to a stable, localized login-page message. */
export function loginErrorTranslationKey(error: unknown): LoginErrorTranslationKey {
  const status = errorStatus(error);
  if (status === 401) return "auth.invalidCredentials";
  if (status === 429) return "auth.rateLimited";
  if (status !== undefined && status >= 500) return "auth.serverError";
  if (status === 0) return "auth.networkError";
  return "auth.signInError";
}

function errorStatus(error: unknown): number | undefined {
  if (typeof error !== "object" || error === null || !("status" in error)) return undefined;
  return typeof error.status === "number" ? error.status : undefined;
}
