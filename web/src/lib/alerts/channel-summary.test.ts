import assert from "node:assert/strict";
import { test } from "vitest";

import {
  REDACTED_SENTINEL,
  buildChannelConfigPayload,
  formatChannelSummary,
  initChannelFormConfig,
  sanitizeDisplayUrl,
} from "./channel-summary.ts";

test("channel card summary never leaks signing secrets even when secret sorts before url", () => {
  const summary = formatChannelSummary({
    type: "webhook",
    config: {
      secret: "whsec_live_super_secret",
      url: "https://hooks.example.com/notify?access_token=abc123&env=prod",
    },
  });

  assert.equal(summary.includes("whsec_live_super_secret"), false);
  assert.equal(summary.includes("abc123"), false);
  assert.match(summary, /^https:\/\/hooks\.example\.com\/notify/);
  assert.equal(summary.includes("env=prod"), true);
});

test("dingtalk channel summary ignores secret and redacts access_token query param", () => {
  const summary = formatChannelSummary({
    type: "dingtalk",
    config: {
      secret: "SEC_very_secret_signing_key",
      url: "https://oapi.dingtalk.com/robot/send?access_token=secret-token-value",
    },
  });

  assert.equal(summary.includes("SEC_very_secret_signing_key"), false);
  assert.equal(summary.includes("secret-token-value"), false);
  assert.equal(summary.includes("oapi.dingtalk.com/robot/send"), true);
});

test("smtp channel summary shows recipient and host:port without exposing password", () => {
  const summary = formatChannelSummary({
    type: "smtp",
    config: {
      password: "smtp-plaintext-password",
      username: "mailer",
      to: "oncall@example.com",
      host: "smtp.example.com",
      port: "587",
    },
  });

  assert.equal(summary, "oncall@example.com (smtp.example.com:587)");
  assert.equal(summary.includes("smtp-plaintext-password"), false);
});

test("sanitizeDisplayUrl strips embedded credentials and sensitive query params", () => {
  const sanitized = sanitizeDisplayUrl("https://user:pass@hooks.example.com/hook?api_key=k1&ok=1");
  assert.equal(sanitized.includes("user"), false);
  assert.equal(sanitized.includes("pass"), false);
  assert.equal(sanitized.includes("k1"), false);
  assert.equal(sanitized.includes("ok=1"), true);
});

test("editing a channel keeps secret input empty and preserves stored secret when left blank", () => {
  const initial = initChannelFormConfig({
    type: "webhook",
    config: {
      url: "https://hooks.example.com/lo",
      secret: REDACTED_SENTINEL,
    },
  });

  assert.equal(initial.values.url, "https://hooks.example.com/lo");
  assert.equal(initial.values.secret, "");
  assert.equal(initial.storedSecrets.secret, true);

  const unchangedPayload = buildChannelConfigPayload(
    "webhook",
    initial.values,
    initial.storedSecrets,
    true,
  );
  assert.deepEqual(unchangedPayload, {
    url: "https://hooks.example.com/lo",
    secret: REDACTED_SENTINEL,
  });

  const rotatedPayload = buildChannelConfigPayload(
    "webhook",
    { ...initial.values, secret: "new-secret-value" },
    initial.storedSecrets,
    true,
  );
  assert.deepEqual(rotatedPayload, {
    url: "https://hooks.example.com/lo",
    secret: "new-secret-value",
  });
});
