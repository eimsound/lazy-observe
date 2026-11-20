import type { AiSettings, AiSettingsUpdate } from "@/lib/api/ai";

export interface AiSettingsFormState {
  draft: AiSettings | null;
  apiKeyDraft: string;
  clearKey: boolean;
  dirty: boolean;
}

export const INITIAL_AI_SETTINGS_FORM: AiSettingsFormState = {
  draft: null,
  apiKeyDraft: "",
  clearKey: false,
  dirty: false,
};

/**
 * Syncs freshly fetched server settings into the form unless the user already
 * has unsaved local edits (`dirty === true`), preventing window-focus refetches
 * from clobbering in-progress edits.
 */
export function syncServerAiSettings(
  state: AiSettingsFormState,
  serverSettings: AiSettings,
): AiSettingsFormState {
  if (state.dirty && state.draft !== null) {
    return state;
  }
  return {
    ...state,
    draft: serverSettings,
    dirty: false,
  };
}

/** Updates a draft field and marks the form dirty. */
export function patchAiDraft(
  state: AiSettingsFormState,
  patch: Partial<AiSettings>,
): AiSettingsFormState {
  if (!state.draft) return state;
  return {
    ...state,
    draft: { ...state.draft, ...patch },
    dirty: true,
  };
}

/**
 * Updating the API key draft cancels a pending `clearKey` intent as soon as a
 * non-empty key is entered so the backend never discards the new key.
 */
export function setAiApiKeyDraft(
  state: AiSettingsFormState,
  value: string,
): AiSettingsFormState {
  return {
    ...state,
    apiKeyDraft: value,
    clearKey: value.trim() !== "" ? false : state.clearKey,
    dirty: true,
  };
}

/** Marks the stored API key for removal and clears any typed key draft. */
export function requestClearAiApiKey(state: AiSettingsFormState): AiSettingsFormState {
  return {
    ...state,
    apiKeyDraft: "",
    clearKey: true,
    dirty: true,
  };
}

/** Resets transient key flags and dirty tracking after a successful save. */
export function applySavedAiSettings(saved: AiSettings): AiSettingsFormState {
  return {
    draft: saved,
    apiKeyDraft: "",
    clearKey: false,
    dirty: false,
  };
}

/**
 * Builds the PUT /api/ai/settings payload, guaranteeing `api_key` and
 * `clear_api_key: true` are never sent together.
 */
export function buildAiSettingsUpdatePayload(
  state: AiSettingsFormState,
): AiSettingsUpdate {
  if (!state.draft) {
    throw new Error("no settings");
  }
  const trimmedKey = state.apiKeyDraft.trim();
  const hasNewKey = trimmedKey !== "";
  return {
    enabled: state.draft.enabled,
    provider: state.draft.provider,
    base_url: state.draft.base_url,
    model: state.draft.model,
    max_tokens: state.draft.max_tokens,
    api_key: hasNewKey ? trimmedKey : undefined,
    clear_api_key: hasNewKey ? false : state.clearKey,
    ...(state.draft.revision !== undefined
      ? { expected_revision: state.draft.revision }
      : {}),
  };
}
