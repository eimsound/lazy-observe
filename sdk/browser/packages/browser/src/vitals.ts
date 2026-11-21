import { onCLS, onFCP, onINP, onLCP, onTTFB, type Metric } from "web-vitals";
import type { Transport, VitalPayload } from "./transport";
import type { Dispose, ReplayVitalSample, VitalDimensions, VitalName, VitalRating } from "./types";
import { collectVitalDimensions, normalizeCountryCode } from "./vitals-dimensions";

export function startVitals(
  transport: Transport,
  page: () => string,
  onReplayVital?: (sample: ReplayVitalSample) => void,
  onDeliveryFailure?: (error: unknown) => void,
  dimensions: () => VitalDimensions = () => collectVitalDimensions(),
): Dispose {
  // Hold references in a mutable container cleared on dispose() so long-lived web-vitals
  // observers neither report after close() nor retain the closed Client and Replay buffer.
  let active: {
    transport: Transport;
    page: () => string;
    onReplayVital?: (sample: ReplayVitalSample) => void;
    onDeliveryFailure?: (error: unknown) => void;
    dimensions: () => VitalDimensions;
  } | undefined = { transport, page, onReplayVital, onDeliveryFailure, dimensions };

  const report = (metric: Metric) => {
    const current = active;
    if (!current) return;
    const timestamp = Date.now();
    current.onReplayVital?.({
      timestamp,
      name: metric.name as VitalName,
      value: metric.value,
      rating: metric.rating as VitalRating,
    });
    const dims = safeReadDimensions(current.dimensions);
    const payload: VitalPayload = {
      name: metric.name as VitalName,
      value: metric.value,
      page: current.page(),
      timestamp: new Date(timestamp).toISOString(),
      ...(dims.device_type ? { device_type: dims.device_type } : {}),
      ...(dims.browser_family ? { browser_family: dims.browser_family } : {}),
      ...(dims.os_family ? { os_family: dims.os_family } : {}),
      ...(dims.country ? { country: dims.country } : {}),
    };
    void current.transport.vitals([payload]).catch((error: unknown) => {
      // Vitals bypass the delivery queue and are sent once, so this is the only place a
      // failed POST is observable. The handler must never reach application code, and the
      // outer try/catch keeps a throwing reporter from becoming an unhandled rejection.
      const handler = active?.onDeliveryFailure;
      if (!handler) return;
      try {
        handler(error);
      } catch {
        /* never break the page */
      }
    });
  };
  onLCP(report); onCLS(report); onINP(report); onTTFB(report); onFCP(report);
  return () => { active = undefined; };
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
