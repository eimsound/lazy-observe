import { onCLS, onFCP, onINP, onLCP, onTTFB, type Metric } from "web-vitals";
import type { Transport } from "./transport";
import type { Dispose, ReplayVitalSample, VitalName, VitalRating } from "./types";

export function startVitals(
  transport: Transport,
  page: () => string,
  onReplayVital?: (sample: ReplayVitalSample) => void,
  onDeliveryFailure?: (error: unknown) => void,
): Dispose {
  // Hold references in a mutable container cleared on dispose() so long-lived web-vitals
  // observers neither report after close() nor retain the closed Client and Replay buffer.
  let active: {
    transport: Transport;
    page: () => string;
    onReplayVital?: (sample: ReplayVitalSample) => void;
    onDeliveryFailure?: (error: unknown) => void;
  } | undefined = { transport, page, onReplayVital, onDeliveryFailure };

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
    void current.transport.vitals([{
      name: metric.name as VitalName,
      value: metric.value,
      page: current.page(),
      timestamp: new Date(timestamp).toISOString()
    }]).catch((error: unknown) => {
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
