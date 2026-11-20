import { monitorEventLoopDelay } from "node:perf_hooks";
import type { Meter } from "@opentelemetry/api";

export type Dispose = () => void;

export function startRuntimeMetrics(meter: Meter, intervalMs: number): Dispose {
  const lag = monitorEventLoopDelay({ resolution: 20 });
  lag.enable();
  let values = { lag: 0, heapUsed: 0, heapTotal: 0, handles: 0 };
  let disposed = false;
  const collect = () => {
    const memory = process.memoryUsage();
    values = {
      lag: Number.isFinite(lag.mean) ? lag.mean / 1e9 : 0,
      heapUsed: memory.heapUsed,
      heapTotal: memory.heapTotal,
      handles: ((process as unknown as { _getActiveHandles?: () => unknown[] })._getActiveHandles?.() ?? []).length,
    };
    lag.reset();
  };
  collect();
  const timer = setInterval(collect, intervalMs);
  timer.unref();
  meter.createObservableGauge("nodejs.eventloop.delay.mean", { unit: "s" }).addCallback((o) => o.observe(values.lag));
  meter.createObservableGauge("process.runtime.nodejs.memory.heap.used", { unit: "By" }).addCallback((o) => o.observe(values.heapUsed));
  meter.createObservableGauge("process.runtime.nodejs.memory.heap.total", { unit: "By" }).addCallback((o) => o.observe(values.heapTotal));
  meter.createObservableGauge("process.runtime.nodejs.active_handles", { unit: "{handle}" }).addCallback((o) => o.observe(values.handles));
  return () => {
    if (disposed) return;
    disposed = true;
    clearInterval(timer);
    lag.disable();
  };
}

export const registerRuntimeMetrics: (meter: Meter, intervalMs: number) => Dispose = startRuntimeMetrics;
