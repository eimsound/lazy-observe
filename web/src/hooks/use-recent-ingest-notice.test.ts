import assert from "node:assert/strict";
import { test } from "vitest";
import {
  MAX_RECENT_INGEST_TIMEOUT_MS,
  readRecentIngestVisible,
  subscribeToRecentIngestExpiry,
  type RecentIngestClock,
} from "./use-recent-ingest-notice.ts";

interface ScheduledTimer {
  id: number;
  delayMs: number;
  callback: () => void;
  cleared: boolean;
}

function createFakeClock(initialNowMs: number) {
  let nowMs = initialNowMs;
  let nextId = 1;
  const timers: ScheduledTimer[] = [];

  const clock: RecentIngestClock = {
    now: () => nowMs,
    setTimeout: (callback, delayMs) => {
      const entry: ScheduledTimer = {
        id: nextId++,
        delayMs,
        callback,
        cleared: false,
      };
      timers.push(entry);
      return entry.id as unknown as ReturnType<typeof setTimeout>;
    },
    clearTimeout: (handle) => {
      const target = timers.find((item) => item.id === (handle as unknown as number));
      if (target) target.cleared = true;
    },
  };

  return {
    clock,
    timers,
    setNow(value: number) {
      nowMs = value;
    },
    fireLatest() {
      const active = [...timers].reverse().find((item) => !item.cleared);
      if (!active) throw new Error("no active timer to fire");
      active.callback();
    },
  };
}

const BASE_NOW_MS = Date.UTC(2026, 10, 22, 10, 0, 0);

test("readRecentIngestVisible handles null and 30s window boundary", () => {
  assert.equal(readRecentIngestVisible(null, BASE_NOW_MS), false);
  assert.equal(readRecentIngestVisible(BASE_NOW_MS - 5_000, BASE_NOW_MS), true);
  assert.equal(readRecentIngestVisible(BASE_NOW_MS - 30_000, BASE_NOW_MS), false);
});

test("subscribeToRecentIngestExpiry schedules rounded-up remaining ms and notifies on expiry", () => {
  const fake = createFakeClock(BASE_NOW_MS);
  const targetMs = BASE_NOW_MS - 5_000.4; // remaining = 24_999.6ms -> ceil 25_000ms
  let notifications = 0;

  const unsubscribe = subscribeToRecentIngestExpiry(
    targetMs,
    () => {
      notifications += 1;
    },
    fake.clock,
  );

  assert.equal(fake.timers.length, 1);
  assert.equal(fake.timers[0].delayMs, 25_000);
  assert.equal(notifications, 0);

  // If the OS fires 1ms early, re-schedule rather than notifying prematurely.
  fake.setNow(targetMs + 29_999);
  fake.fireLatest();
  assert.equal(notifications, 0);
  assert.equal(fake.timers.length, 2);
  assert.equal(fake.timers[1].delayMs, 1);

  // Once the 30s boundary passes, notify once.
  fake.setNow(targetMs + 30_000);
  fake.fireLatest();
  assert.equal(notifications, 1);

  unsubscribe();
});

test("subscribeToRecentIngestExpiry clears active timer on unsubscribe and skips null/expired inputs", () => {
  const fake = createFakeClock(BASE_NOW_MS);
  let notifications = 0;

  const unsubNull = subscribeToRecentIngestExpiry(null, () => {
    notifications += 1;
  }, fake.clock);
  unsubNull();
  assert.equal(fake.timers.length, 0);

  const unsubExpired = subscribeToRecentIngestExpiry(BASE_NOW_MS - 30_000, () => {
    notifications += 1;
  }, fake.clock);
  unsubExpired();
  assert.equal(fake.timers.length, 0);

  const unsubActive = subscribeToRecentIngestExpiry(BASE_NOW_MS - 1_000, () => {
    notifications += 1;
  }, fake.clock);
  assert.equal(fake.timers.length, 1);
  assert.equal(fake.timers[0].cleared, false);

  unsubActive();
  assert.equal(fake.timers[0].cleared, true);

  // Even if callback fires after disposal, it must not notify.
  fake.setNow(BASE_NOW_MS + 40_000);
  fake.timers[0].callback();
  assert.equal(notifications, 0);
});

test("subscribeToRecentIngestExpiry caps single timer delay at MAX_RECENT_INGEST_TIMEOUT_MS", () => {
  const fake = createFakeClock(BASE_NOW_MS);
  const farFutureMs = BASE_NOW_MS + MAX_RECENT_INGEST_TIMEOUT_MS + 5_000;
  let notifications = 0;

  const unsubscribe = subscribeToRecentIngestExpiry(
    farFutureMs,
    () => {
      notifications += 1;
    },
    fake.clock,
  );

  assert.equal(fake.timers.length, 1);
  assert.equal(fake.timers[0].delayMs, MAX_RECENT_INGEST_TIMEOUT_MS);
  assert.equal(notifications, 0);

  unsubscribe();
});
