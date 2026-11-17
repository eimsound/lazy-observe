import { onCLS, onFCP, onINP, onLCP, onTTFB, type Metric } from "web-vitals";
import type { Transport } from "./transport";
import type { ReplayVitalSample, VitalName, VitalRating } from "./types";

export function startVitals(
  transport: Transport,
  page: () => string,
  onReplayVital?: (sample: ReplayVitalSample) => void,
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
    }]).catch(() => undefined);
  };
  onLCP(report); onCLS(report); onINP(report); onTTFB(report); onFCP(report);
}
