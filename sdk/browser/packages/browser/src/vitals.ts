import { onCLS, onFCP, onINP, onLCP, onTTFB, type Metric } from "web-vitals";
import type { VitalPayload } from "./transport";
import type { Dispose, ReplayVitalSample, VitalDimensions, VitalName, VitalRating } from "./types";
import { collectVitalDimensions, normalizeCountryCode } from "./vitals-dimensions";

export const DEFAULT_VITAL_QUEUE_CAPACITY = 50;
export const DEFAULT_VITAL_BATCH_SIZE = 10;
export const DEFAULT_VITAL_FLUSH_INTERVAL_MS = 1_500;
export const MAX_VITALS_DISTINCT_PAGES_PER_BATCH = 32;

export function vitalDedupeKey(vital: VitalPayload): string {
  return `${vital.name}\0${vital.page}`;
}

export function vitalPageKey(vital: VitalPayload): string {
  return vital.page;
}

export function startVitals(
  enqueue: (payload: VitalPayload) => void,
  page: () => string,
  onReplayVital?: (sample: ReplayVitalSample) => void,
  dimensions: () => VitalDimensions = () => collectVitalDimensions(),
): Dispose {
  // Hold references in a mutable container cleared on dispose() so long-lived web-vitals
  // observers neither report after close() nor retain the closed Client and Replay buffer.
  let active: {
    enqueue: (payload: VitalPayload) => void;
    page: () => string;
    onReplayVital?: (sample: ReplayVitalSample) => void;
    dimensions: () => VitalDimensions;
  } | undefined = { enqueue, page, onReplayVital, dimensions };

  const report = (metric: Metric) => {
    const current = active;
    if (!current) return;
    const timestamp = Date.now();
    try {
      current.onReplayVital?.({
        timestamp,
        name: metric.name as VitalName,
        value: metric.value,
        rating: metric.rating as VitalRating,
      });
    } catch {
      /* never break the page */
    }
    const dims = safeReadDimensions(current.dimensions);
    const payload: VitalPayload = {
      name: metric.name as VitalName,
      value: metric.value,
      page: safeReadPage(current.page),
      timestamp: new Date(timestamp).toISOString(),
      ...(dims.device_type ? { device_type: dims.device_type } : {}),
      ...(dims.browser_family ? { browser_family: dims.browser_family } : {}),
      ...(dims.os_family ? { os_family: dims.os_family } : {}),
      ...(dims.country ? { country: dims.country } : {}),
    };
    try {
      current.enqueue(payload);
    } catch {
      /* never break the page */
    }
  };
  onLCP(report); onCLS(report); onINP(report); onTTFB(report); onFCP(report);
  return () => { active = undefined; };
}

function safeReadPage(reader: () => string): string {
  try {
    return reader() || "/";
  } catch {
    return "/";
  }
}

function safeReadDimensions(reader: () => VitalDimensions): VitalDimensions {
  try {
    const dims = reader();
    if (!dims || typeof dims !== "object") return {};
    const country = normalizeCountryCode(dims.country);
    return {
      device_type: dims.device_type,
      browser_family: dims.browser_family,
      os_family: dims.os_family,
      ...(country ? { country } : {}),
    };
  } catch {
    return {};
  }
}
