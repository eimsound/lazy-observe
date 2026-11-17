import { expect, it } from "vitest";
import { MinuteRateLimiter } from "../src/rate-limit";

it("limits a minute and resets in the next window", () => {
  let now = 1_000;
  const limiter = new MinuteRateLimiter(2, () => now);
  expect(limiter.take()).toBe(true);
  expect(limiter.take()).toBe(true);
  expect(limiter.take()).toBe(false);
  now += 60_000;
  expect(limiter.take()).toBe(true);
});
