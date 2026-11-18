import { onCLS, onFCP, onINP, onLCP, onTTFB, type Metric } from "web-vitals";
import type { Transport } from "./transport";
import type { ReplayVitalSample, VitalName, VitalRating } from "./types";

export function startVitals(
  transport: Transport,
  page: () => string,
  onReplayVital?: (sample: ReplayVitalSample) => void,
  onDeliveryFailure?: (error: unknown) => void,
): void {
  const report = (metric: Metric) => {
    const timestamp = Date.now();
    onReplayVital?.({
      timestamp,
      name: metric.name as VitalName,
      value: metric.value,
      rating: metric.rating as VitalRating,
    });
    void transport.vitals([{
      name: metric.name as VitalName,
      value: metric.value,
      page: page(),
      timestamp: new Date(timestamp).toISOString()
    }]).catch((error: unknown) => {
      // Vitals bypass the delivery queue and are sent once, so this is the only place a
      // failed POST is observable. The handler must never reach application code, and the
      // outer try/catch keeps a throwing reporter from becoming an unhandled rejection.
      try {
        onDeliveryFailure?.(error);
      } catch {
        /* never break the page */
      }
    });
  };
  onLCP(report); onCLS(report); onINP(report); onTTFB(report); onFCP(report);
}
