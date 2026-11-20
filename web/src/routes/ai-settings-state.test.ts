import assert from "node:assert/strict";
import { test } from "vitest";

import type { AiSettings } from "../lib/api/ai.ts";
import {
  INITIAL_AI_SETTINGS_FORM,
  applySavedAiSettings,
  buildAiSettingsUpdatePayload,
  patchAiDraft,
  requestClearAiApiKey,
  setAiApiKeyDraft,
  syncServerAiSettings,
} from "./ai-settings-state.ts";

const SERVER_SETTINGS: AiSettings = {
  enabled: true,
  provider: "openai",
  base_url: "https://api.openai.com",
  model: "gpt-4o-mini",
  max_tokens: 1024,
  has_api_key: true,
  source: "database",
  updated_by: "admin",
};

test("typing a new API key after clicking clear resets clear_api_key", () => {
  let state = syncServerAiSettings(INITIAL_AI_SETTINGS_FORM, SERVER_SETTINGS);
  state = requestClearAiApiKey(state);
  assert.equal(state.clearKey, true);
  assert.equal(state.apiKeyDraft, "");

  state = setAiApiKeyDraft(state, "sk-new-secret-key");
  assert.equal(state.clearKey, false);

  const payload = buildAiSettingsUpdatePayload(state);
  assert.equal(payload.api_key, "sk-new-secret-key");
  assert.equal(payload.clear_api_key, false);
});

test("server refetch does not clobber dirty unsaved draft edits", () => {
  let state = syncServerAiSettings(INITIAL_AI_SETTINGS_FORM, SERVER_SETTINGS);
  state = patchAiDraft(state, { model: "gpt-4.1" });
  assert.equal(state.dirty, true);

  const afterRefetch = syncServerAiSettings(state, {
    ...SERVER_SETTINGS,
    model: "gpt-4o-mini",
  });
  assert.equal(afterRefetch.draft?.model, "gpt-4.1");
  assert.equal(afterRefetch.dirty, true);

  const afterSave = applySavedAiSettings({
    ...SERVER_SETTINGS,
    model: "gpt-4.1",
  });
  assert.equal(afterSave.dirty, false);
  assert.equal(afterSave.clearKey, false);
  assert.equal(afterSave.apiKeyDraft, "");
});

test("clicking clear API key clears any previously typed key draft", () => {
  let state = syncServerAiSettings(INITIAL_AI_SETTINGS_FORM, SERVER_SETTINGS);
  state = setAiApiKeyDraft(state, "sk-temp");
  state = requestClearAiApiKey(state);

  const payload = buildAiSettingsUpdatePayload(state);
  assert.equal(payload.api_key, undefined);
  assert.equal(payload.clear_api_key, true);
});

test("buildAiSettingsUpdatePayload propagates expected_revision when server supplied revision", () => {
  const state = syncServerAiSettings(INITIAL_AI_SETTINGS_FORM, {
    ...SERVER_SETTINGS,
    revision: 7,
  });
  const payload = buildAiSettingsUpdatePayload(state);
  assert.equal(payload.expected_revision, 7);
});
