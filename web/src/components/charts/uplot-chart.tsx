import { useEffect, useRef } from "react";
import uPlot from "uplot";
import "uplot/dist/uPlot.min.css";
import "./uplot-chart.css";

export type ResponsiveUPlotOptions = Omit<uPlot.Options, "height" | "width">;

interface UPlotChartProps {
  data: uPlot.AlignedData;
  height: number;
  options: ResponsiveUPlotOptions;
}

/**
 * A deliberately small React lifecycle adapter around uPlot. Chart behavior
 * remains in the caller's options/plugins; this component only owns creation,
 * responsive sizing, and destruction.
 */
export function UPlotChart({ data, height, options }: UPlotChartProps) {
  const containerRef = useRef<HTMLDivElement>(null);

  useEffect(() => {
    const container = containerRef.current;
    if (!container) return;

    const chart = new uPlot(
      {
        ...options,
        width: Math.max(1, container.clientWidth),
        height,
      },
      data,
      container,
    );
    const resizeObserver = new ResizeObserver(() => {
      const width = Math.max(1, container.clientWidth);
      if (width !== chart.width || height !== chart.height) {
        chart.setSize({ width, height });
      }
    });
    resizeObserver.observe(container);

    return () => {
      resizeObserver.disconnect();
      chart.destroy();
    };
  }, [data, height, options]);

  return <div ref={containerRef} className="lazy-uplot" style={{ height }} />;
}
