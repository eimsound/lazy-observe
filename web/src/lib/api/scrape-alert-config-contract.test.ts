import assert from "node:assert/strict";
import test from "node:test";

import {
  normalizeScrapeTargetListResponse,
  normalizeScrapeTargetResponse,
  normalizeTargetHealthResponse,
} from "./scrape-target-contract.ts";
import { normalizeAlertConfigResponse } from "./alert-config-contract.ts";

test("normalizes the explicit scrape target DTO without storage fields", () => {
  const target = normalizeScrapeTargetResponse({
    id: "42",
    name: "node",
    url: "http://node:9100/metrics",
    interval: "15s",
    labels: { env: "prod" },
    enabled: true,
  });
  assert.deepEqual(target, {
    id: "42",
    name: "node",
    url: "http://node:9100/metrics",
    interval: "15s",
    labels: { env: "prod" },
    enabled: true,
  });
  assert.throws(
    () => normalizeScrapeTargetResponse({ ...target, id: 42 }),
    /id must be a non-empty string/,
  );
  assert.deepEqual(normalizeScrapeTargetListResponse([target]), [target]);
});

test("uses last_scrape_at as the single health timestamp field", () => {
  assert.deepEqual(
    normalizeTargetHealthResponse({
      status: "up",
      last_scrape_at: "2025-07-17T13:00:00.25Z",
      scrape_job: "scrape-42",
    }),
    {
      status: "up",
      last_scrape_at: "2025-07-17T13:00:00.25Z",
      scrape_job: "scrape-42",
    },
  );
  assert.deepEqual(
    normalizeTargetHealthResponse({ status: "unknown", last_scrape_at: null, scrape_job: "scrape-42" }),
    { status: "unknown", last_scrape_at: null, scrape_job: "scrape-42" },
  );
  assert.throws(
    () => normalizeTargetHealthResponse({ status: "up", last_scrape: "2025-07-17T13:00:00Z" }),
    /last_scrape_at/,
  );
});

test("normalizes project alert-config channel ids as strings only", () => {
  const config = normalizeAlertConfigResponse({
    new_issue: true,
    regression: false,
    error_rate_threshold_per_min: 3,
    channel_ids: ["7", "11"],
  });
  assert.deepEqual(config.channel_ids, ["7", "11"]);
  assert.throws(
    () => normalizeAlertConfigResponse({ ...config, channel_ids: [7] }),
    /channel_ids must contain strings/,
  );
});
