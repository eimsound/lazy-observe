import { useMemo, useState } from "react";
import uPlot from "uplot";
import { Eye, EyeOff, Filter } from "lucide-react";
import { useTranslation } from "react-i18next";
import type { LogsHistogram } from "@/lib/api/logs";
import { formatClock } from "@/lib/format";
import { Button } from "@/components/ui/button";
import { cn } from "@/lib/utils";
import { useChartTheme } from "./use-chart-theme";
import { UPlotChart, type ResponsiveUPlotOptions } from "./uplot-chart";
import { createUPlotTooltip } from "./uplot-plugins";

interface LogsHistogramChartProps {
  data: LogsHistogram;
  height?: number;
}

export interface LogsHitsChartSeries {
  label?: string;
  fields: Record<string, string>;
  timestamps: number[];
  values: number[];
  total: number;
  isOther?: boolean;
}

interface LogsHitsChartProps {
  series: LogsHitsChartSeries[];
  height?: number;
  stacked?: boolean;
  cumulative?: boolean;
  onSelectRange?: (startMs: number, endMs: number) => void;
  onFilterSeries?: (fields: Record<string, string>) => void;
}

export function LogsHistogramChart({ data, height = 132 }: LogsHistogramChartProps) {
  const theme = useChartTheme();
  const chartData = useMemo<uPlot.AlignedData>(() => [
    data.timestamps.map((timestamp) => timestamp / 1_000),
    data.values.map((value) => Number.isFinite(value) ? value : null),
  ], [data]);
  const options = useMemo<ResponsiveUPlotOptions>(() => ({
    series: [
      { label: "Time" },
      {
        label: "Logs",
        stroke: theme.series[0],
        fill: theme.series[0],
        paths: uPlot.paths.bars?.({
          size: [0.8, 18],
          radius: [0.2, 0],
        }),
        points: { show: false },
      },
    ],
    scales: {
      y: {
        range: (_chart, _minimum, maximum) => [
          0,
          Number.isFinite(maximum) && maximum > 0 ? maximum * 1.1 : 1,
        ],
      },
    },
    axes: [
      {
        stroke: theme.mutedForeground,
        grid: { show: false },
        ticks: { stroke: theme.border },
        font: "10px sans-serif",
        space: 70,
      },
      {
        stroke: theme.mutedForeground,
        grid: { stroke: theme.border, width: 1, dash: [4, 4] },
        ticks: { stroke: theme.border },
        font: "10px sans-serif",
        size: 44,
        splits: (_chart, _axisIndex, minimum, maximum) => {
          const upper = Math.max(1, maximum);
          const step = Math.max(1, Math.ceil((upper - Math.max(0, minimum)) / 2));
          return [0, step, step * 2].filter((value) => value <= upper);
        },
        values: (_chart, splits) => splits.map((value) => String(Math.round(value))),
      },
    ],
    cursor: {
      drag: { x: false, y: false },
      lock: true,
      points: { show: false },
      y: false,
    },
    legend: { show: false },
    plugins: [
      createUPlotTooltip({
        data: chartData,
        formatTimestamp: (timestamp) => formatClock(timestamp * 1_000),
        formatValue: (value) => new Intl.NumberFormat().format(value),
        series: [{
          color: theme.series[0] ?? theme.foreground,
          dataIndex: 1,
          label: "Logs",
        }],
      }),
    ],
  }), [chartData, theme]);

  return <UPlotChart data={chartData} height={height} options={options} />;
}

function seriesLabel(item: LogsHitsChartSeries): string {
  if (item.label) return item.label;
  const fields = Object.entries(item.fields);
  if (fields.length === 0) return item.isOther ? "Other" : "Logs";
  return fields.map(([key, value]) => `${key}=${JSON.stringify(value)}`).join(", ");
}

function alignHitsSeries(series: LogsHitsChartSeries[]) {
  const timestamps = [...new Set(series.flatMap((item) => item.timestamps))]
    .filter(Number.isFinite)
    .sort((left, right) => left - right);
  const rows = series.map((item) => {
    const values = new Map(item.timestamps.map((timestamp, index) => [
      timestamp,
      Number(item.values[index]) || 0,
    ]));
    return timestamps.map((timestamp) => values.get(timestamp) ?? 0);
  });
  return { timestamps, rows };
}

function cumulativeRows(rows: number[][]): number[][] {
  return rows.map((row) => {
    let total = 0;
    return row.map((value) => {
      total += value;
      return total;
    });
  });
}

function stackedRows(rows: number[][]): number[][] {
  const totals = new Array(rows[0]?.length ?? 0).fill(0);
  for (const row of rows) {
    row.forEach((value, index) => {
      totals[index] += value;
    });
  }
  // uPlot bars are filled from zero. Draw suffix totals from largest to
  // smallest so later series expose each preceding segment instead of the
  // final cumulative series covering the entire stack.
  return rows.map((row) => {
    const result = [...totals];
    row.forEach((value, index) => {
      totals[index] -= value;
    });
    return result;
  });
}

function createRangeSelectionPlugin(
  onSelectRange: ((startMs: number, endMs: number) => void) | undefined,
  timestamps: number[],
): uPlot.Plugin {
  let over: HTMLDivElement | null = null;
  let chart: uPlot | null = null;
  const selectBucket = () => {
    const index = chart?.cursor.idx;
    if (index === null || index === undefined || index < 0) return;
    const start = timestamps[index];
    if (!Number.isFinite(start)) return;
    const fallbackWidth = index > 0
      ? start - timestamps[index - 1]
      : timestamps[index + 1] !== undefined ? timestamps[index + 1] - start : 1_000;
    const end = timestamps[index + 1] ?? start + Math.max(1, fallbackWidth);
    onSelectRange?.(start, end);
  };

  return {
    hooks: {
      init: [(instance) => {
        chart = instance;
        over = instance.over;
        over.addEventListener("dblclick", selectBucket);
      }],
      setSelect: [(instance) => {
        if (!onSelectRange || instance.select.width < 4) return;
        const start = instance.posToVal(instance.select.left, "x") * 1_000;
        const end = instance.posToVal(instance.select.left + instance.select.width, "x") * 1_000;
        if (Number.isFinite(start) && Number.isFinite(end) && start < end) {
          onSelectRange(Math.floor(start), Math.ceil(end));
        }
      }],
      destroy: [() => {
        over?.removeEventListener("dblclick", selectBucket);
        over = null;
        chart = null;
      }],
    },
  };
}

const EMPTY_HIDDEN_INDEXES: number[] = [];

export function LogsHitsChart({
  series,
  height = 220,
  stacked = false,
  cumulative = false,
  onSelectRange,
  onFilterSeries,
}: LogsHitsChartProps) {
  const { t } = useTranslation();
  const theme = useChartTheme();
  const seriesIdentity = useMemo(
    () => series.map((item) => JSON.stringify([
      item.label,
      item.fields,
      item.isOther,
    ])).join("\u0000"),
    [series],
  );
  const [hiddenState, setHiddenState] = useState<{ identity: string; indexes: number[] }>({
    identity: seriesIdentity,
    indexes: EMPTY_HIDDEN_INDEXES,
  });
  const hidden = hiddenState.identity === seriesIdentity
    ? hiddenState.indexes
    : EMPTY_HIDDEN_INDEXES;
  const toggleHidden = (index: number) => {
    setHiddenState((current) => {
      const base = current.identity === seriesIdentity
        ? current.indexes
        : EMPTY_HIDDEN_INDEXES;
      const next = base.includes(index)
        ? base.filter((itemIndex) => itemIndex !== index)
        : [...base, index];
      return { identity: seriesIdentity, indexes: next };
    });
  };
  const seriesColors = useMemo(
    () => series.map((item, index) => (
      item.isOther
        ? theme.mutedForeground
        : theme.series[index % Math.max(1, theme.series.length)] ?? theme.foreground
    )),
    [series, theme],
  );
  const visibleEntries = useMemo(
    () => series
      .map((item, index) => ({ item, index }))
      .filter(({ index }) => !hidden.includes(index)),
    [hidden, series],
  );
  const visible = useMemo(
    () => visibleEntries.map(({ item }) => item),
    [visibleEntries],
  );
  const aligned = useMemo(() => alignHitsSeries(visible), [visible]);
  const rawData = useMemo<uPlot.AlignedData>(() => [
    aligned.timestamps.map((timestamp) => timestamp / 1_000),
    ...aligned.rows,
  ], [aligned]);
  const chartData = useMemo<uPlot.AlignedData>(() => {
    const cumulativeRowsValue = cumulative ? cumulativeRows(aligned.rows) : aligned.rows;
    const transformedRows = stacked ? stackedRows(cumulativeRowsValue) : cumulativeRowsValue;
    return [
      aligned.timestamps.map((timestamp) => timestamp / 1_000),
      ...transformedRows,
    ];
  }, [aligned, cumulative, stacked]);
  const options = useMemo<ResponsiveUPlotOptions>(() => {
    const colors = visibleEntries.map(({ index }) => seriesColors[index]);
    return {
      series: [
        { label: "Time" },
        ...visible.map((item, index) => ({
          label: seriesLabel(item),
          stroke: colors[index],
          fill: colors[index],
          paths: uPlot.paths.bars?.({
            align: 1,
            gap: 1,
            size: [0.9, 40],
            radius: [0.12, 0],
          }),
          points: { show: false },
        })),
      ],
      scales: {
        x: { time: true },
        y: {
          range: (_chart, _minimum, maximum) => [
            0,
            Number.isFinite(maximum) && maximum > 0 ? maximum * 1.08 : 1,
          ],
        },
      },
      axes: [
        {
          stroke: theme.mutedForeground,
          grid: { show: false },
          ticks: { stroke: theme.border },
          font: "10px sans-serif",
          space: 70,
        },
        {
          stroke: theme.mutedForeground,
          grid: { stroke: theme.border, width: 1, dash: [4, 4] },
          ticks: { stroke: theme.border },
          font: "10px sans-serif",
          size: 48,
          values: (_chart, splits) => splits.map((value) => (
            new Intl.NumberFormat(undefined, { notation: "compact" }).format(value)
          )),
        },
      ],
      cursor: {
        drag: { x: Boolean(onSelectRange), y: false, setScale: false },
        points: { show: false },
        y: false,
      },
      legend: { show: false },
      plugins: [
        createUPlotTooltip({
          data: rawData,
          formatTimestamp: (timestamp) => formatClock(timestamp * 1_000),
          formatValue: (value) => new Intl.NumberFormat().format(value),
          series: visible.map((item, index) => ({
            color: colors[index],
            dataIndex: index + 1,
            label: seriesLabel(item),
          })),
        }),
        createRangeSelectionPlugin(onSelectRange, aligned.timestamps),
      ],
    };
  }, [
    aligned.timestamps,
    onSelectRange,
    rawData,
    seriesColors,
    theme,
    visible,
    visibleEntries,
  ]);

  if (!series.length) return null;

  return (
    <div className="space-y-3">
      {aligned.timestamps.length ? (
        <UPlotChart data={chartData} height={height} options={options} />
      ) : (
        <div
          className="flex items-center justify-center rounded-md border border-dashed border-border text-sm text-muted-foreground"
          style={{ height }}
        >
          {t("logs.allSeriesHidden")}
        </div>
      )}
      {series.length > 1 && (
        <div className="grid gap-1 sm:grid-cols-2 xl:grid-cols-3">
          {series.map((item, index) => {
            const isHidden = hidden.includes(index);
            const color = seriesColors[index];
            return (
              <div
                key={`${seriesLabel(item)}-${index}`}
                className={cn(
                  "group flex min-w-0 items-center gap-2 rounded-md px-2 py-1 text-xs",
                  isHidden ? "text-muted-foreground opacity-60" : "hover:bg-muted",
                )}
              >
                <span className="h-1 w-3 shrink-0 rounded-full" style={{ backgroundColor: color }} />
                <button
                  type="button"
                  className="min-w-0 flex-1 truncate text-left"
                  title={seriesLabel(item)}
                  onClick={() => toggleHidden(index)}
                >
                  {seriesLabel(item)}
                </button>
                <span className="tabular-nums text-muted-foreground">
                  {new Intl.NumberFormat(undefined, { notation: "compact" }).format(item.total)}
                </span>
                <Button
                  type="button"
                  variant="ghost"
                  size="icon"
                  className="size-6"
                  onClick={() => toggleHidden(index)}
                  aria-label={isHidden ? t("logs.showSeries") : t("logs.hideSeries")}
                >
                  {isHidden ? <EyeOff /> : <Eye />}
                </Button>
                {onFilterSeries && Object.keys(item.fields).length > 0 && (
                  <Button
                    type="button"
                    variant="ghost"
                    size="icon"
                    className="size-6"
                    onClick={() => onFilterSeries(item.fields)}
                    aria-label={t("logs.filterSeries")}
                  >
                    <Filter />
                  </Button>
                )}
              </div>
            );
          })}
        </div>
      )}
    </div>
  );
}
