import { useEffect, useMemo, useState, type MouseEvent } from "react";
import { useTranslation } from "react-i18next";
import uPlot from "uplot";
import type { TimeSeries } from "@/lib/api/types";
import dayjs from "@/lib/dayjs";
import {
  formatMetricsAxisValue,
  toggleMetricsSeriesSelection,
  type MetricsTableMode,
} from "@/lib/metrics-query";
import { MetricLegendTable } from "./metric-legend-table";
import { useChartTheme } from "./use-chart-theme";
import { UPlotChart, type ResponsiveUPlotOptions } from "./uplot-chart";
import { createResetZoomPlugin, createUPlotTooltip } from "./uplot-plugins";
import {
  createTimeSeriesTimeConfig,
  formatTimeSeriesTimestamp,
  type TimeSeriesTimeWindow,
} from "./time-series-time";

interface TimeSeriesChartProps {
  series: TimeSeries[];
  height?: number;
  useUTC?: boolean;
  stacked?: boolean;
  yAxisMin?: number | null;
  tableMode?: MetricsTableMode;
  timeWindow?: TimeSeriesTimeWindow;
}

type NumberSeries = Array<number | null>;
type ChartData = [number[], ...NumberSeries[]];

function alignSeries(series: TimeSeries[]): ChartData {
  const timestamps = [...new Set(series.flatMap((item) => item.points.map(([timestamp]) => timestamp)))]
    .sort((left, right) => left - right);
  const values = series.map((item) => {
    const byTimestamp = new Map(
      item.points.map(([timestamp, value]) => [
        timestamp,
        value !== null && Number.isFinite(value) ? value : null,
      ]),
    );
    return timestamps.map((timestamp) => byTimestamp.get(timestamp) ?? null);
  });
  return [timestamps, ...values];
}

function stackSeries(data: ChartData, visibleIndexes: Set<number>): {
  bands: uPlot.Band[];
  data: ChartData;
} {
  const accumulator = data[0].map(() => 0);
  const stacked: NumberSeries[] = data.slice(1).map((values, seriesIndex) => {
    if (!visibleIndexes.has(seriesIndex)) return [...values];
    return values.map((value, dataIndex) => {
      if (value === null || value === undefined) return null;
      accumulator[dataIndex] = (accumulator[dataIndex] ?? 0) + value;
      return accumulator[dataIndex] ?? value;
    });
  });
  const visibleDataIndexes = [...visibleIndexes]
    .sort((left, right) => left - right)
    .map((seriesIndex) => seriesIndex + 1);
  const bands = visibleDataIndexes.slice(1).map((upperIndex, index) => ({
    series: [upperIndex, visibleDataIndexes[index] ?? upperIndex],
  }) satisfies uPlot.Band);
  return { data: [data[0], ...stacked], bands };
}

function translucent(color: string): string {
  return `color-mix(in oklch, ${color} 22%, transparent)`;
}

export function TimeSeriesChart({
  series,
  height = 420,
  useUTC = false,
  stacked = false,
  yAxisMin = null,
  tableMode = "series",
  timeWindow,
}: TimeSeriesChartProps) {
  const [selectedIndexes, setSelectedIndexes] = useState<number[]>([]);
  const { i18n } = useTranslation();
  const theme = useChartTheme();
  const seriesKey = series.map((item) => item.name).join("\u0000");
  // Depend on the window's values, not the caller's object identity.
  const windowStartMs = timeWindow?.startMs;
  const windowEndMs = timeWindow?.endMs;
  const timeConfig = useMemo(
    () => createTimeSeriesTimeConfig(
      windowStartMs === undefined || windowEndMs === undefined
        ? undefined
        : { startMs: windowStartMs, endMs: windowEndMs },
    ),
    [windowStartMs, windowEndMs],
  );

  useEffect(() => {
    setSelectedIndexes([]);
  }, [seriesKey]);

  const rawData = useMemo(() => alignSeries(series), [series]);
  const visibleIndexes = useMemo(
    () => new Set(
      selectedIndexes.length === 0
        ? series.map((_, index) => index)
        : selectedIndexes,
    ),
    [selectedIndexes, series],
  );
  const stackedResult = useMemo(
    () => stacked ? stackSeries(rawData, visibleIndexes) : { data: rawData, bands: [] },
    [rawData, stacked, visibleIndexes],
  );
  const options = useMemo<ResponsiveUPlotOptions>(() => ({
    ms: timeConfig.ms,
    series: [
      { label: "Time" },
      ...series.map((item, index): uPlot.Series => ({
        show: visibleIndexes.has(index),
        label: item.name,
        stroke: theme.series[index % theme.series.length],
        fill: stacked ? translucent(theme.series[index % theme.series.length] ?? theme.foreground) : undefined,
        width: 1.5,
        spanGaps: false,
        points: { show: false },
      })),
    ],
    bands: stackedResult.bands,
    tzDate: useUTC
      ? (timestamp) => uPlot.tzDate(dayjs(timestamp).toDate(), "Etc/UTC")
      : undefined,
    scales: {
      ...(timeConfig.xRange
        ? { x: { range: timeConfig.xRange } }
        : {}),
      ...(yAxisMin === null
        ? {}
        : {
          y: {
            range: (_chart, _minimum, maximum) => {
              const upper = Number.isFinite(maximum) && maximum > yAxisMin
                ? maximum
                : yAxisMin + 1;
              const range = uPlot.rangeNum(yAxisMin, upper, 0.1, true);
              return [yAxisMin, range[1]];
            },
          },
        }),
    },
    axes: [
      {
        stroke: theme.mutedForeground,
        grid: { stroke: theme.border, width: 1, dash: [4, 4] },
        ticks: { stroke: theme.border },
        font: "11px sans-serif",
        space: 80,
      },
      {
        stroke: theme.mutedForeground,
        grid: { stroke: theme.border, width: 1, dash: [4, 4] },
        ticks: { stroke: theme.border },
        font: "11px sans-serif",
        size: 64,
        values: (_chart, splits) => splits.map(formatMetricsAxisValue),
      },
    ],
    cursor: {
      drag: { x: true, y: false, setScale: true },
      lock: true,
      points: { size: 5, width: 1 },
      y: false,
    },
    focus: { alpha: 1 },
    legend: { show: false },
    plugins: [
      createUPlotTooltip({
        data: rawData,
        formatTimestamp: (timestamp) =>
          formatTimeSeriesTimestamp(timestamp, useUTC, i18n.language),
        formatValue: formatMetricsAxisValue,
        series: series.flatMap((item, index) =>
          visibleIndexes.has(index)
            ? [{
              color: theme.series[index % theme.series.length] ?? theme.foreground,
              dataIndex: index + 1,
              label: item.name,
            }]
            : []
        ),
      }),
      createResetZoomPlugin(),
    ],
  }), [
    rawData,
    i18n.language,
    series,
    stacked,
    stackedResult.bands,
    theme,
    timeConfig.ms,
    timeConfig.xRange,
    useUTC,
    visibleIndexes,
    yAxisMin,
  ]);

  function toggleSeries(index: number, event: MouseEvent<HTMLElement>) {
    setSelectedIndexes((current) =>
      toggleMetricsSeriesSelection(
        current,
        index,
        series.length,
        event.ctrlKey || event.metaKey,
      )
    );
  }

  return (
    <div className="min-w-0">
      <UPlotChart data={stackedResult.data} height={height} options={options} />
      <MetricLegendTable
        series={series}
        colors={theme.series}
        selectedIndexes={selectedIndexes}
        tableMode={tableMode}
        onRowClick={toggleSeries}
      />
    </div>
  );
}
