import { useMemo, type MouseEvent } from "react";
import { useTranslation } from "react-i18next";
import { DataTable, type DataTableColumnDef } from "@/components/ui/data-table";
import type { TimeSeries } from "@/lib/api/types";
import {
  metricsLabelColumns,
  type MetricsTableMode,
} from "@/lib/metrics-query";

interface MetricLegendRow {
  id: string;
  index: number;
  metric: string;
  metricName: string | undefined;
  labels: Record<string, string>;
  color: string;
  avg: number | undefined;
  max: number | undefined;
  min: number | undefined;
  last: number | undefined;
  sum: number | undefined;
}

interface MetricLegendTableProps {
  series: TimeSeries[];
  colors: string[];
  selectedIndexes: number[];
  tableMode: MetricsTableMode;
  onRowClick: (index: number, event: MouseEvent<HTMLElement>) => void;
}

const STAT_COLUMN_IDS = new Set(["avg", "max", "min", "last", "sum"]);

function calculateLegendRow(item: TimeSeries, index: number, color: string): MetricLegendRow {
  let count = 0;
  let sum = 0;
  let min = Number.POSITIVE_INFINITY;
  let max = Number.NEGATIVE_INFINITY;
  let last: number | undefined;
  for (const [, value] of item.points) {
    if (value === null || !Number.isFinite(value)) continue;
    count++;
    sum += value;
    min = Math.min(min, value);
    max = Math.max(max, value);
    last = value;
  }

  return {
    id: `${item.name}\u0000${index}`,
    index,
    metric: item.name,
    metricName: item.labels.__name__,
    labels: item.labels,
    color,
    avg: count > 0 ? sum / count : undefined,
    max: count > 0 ? max : undefined,
    min: count > 0 ? min : undefined,
    last,
    sum: count > 0 ? sum : undefined,
  };
}

function formatMetricValue(value: number | undefined, locale: string) {
  if (value === undefined) return "—";
  const absolute = Math.abs(value);
  if ((absolute > 0 && absolute < 0.001) || absolute >= 1_000_000_000) {
    return value.toExponential(3);
  }
  return new Intl.NumberFormat(locale, {
    maximumFractionDigits: 6,
    maximumSignificantDigits: 7,
  }).format(value);
}

export function MetricLegendTable({
  series,
  colors,
  selectedIndexes,
  tableMode,
  onRowClick,
}: MetricLegendTableProps) {
  const { t, i18n } = useTranslation();
  const data = useMemo(
    () => series.map((item, index) =>
      calculateLegendRow(item, index, colors[index % colors.length])
    ),
    [colors, series],
  );
  const labelNames = useMemo(() => metricsLabelColumns(series), [series]);
  const columns = useMemo<DataTableColumnDef<MetricLegendRow>[]>(() => {
    const statColumn = (
      key: "avg" | "max" | "min" | "last" | "sum",
      label: string,
    ): DataTableColumnDef<MetricLegendRow> => ({
      accessorKey: key,
      header: label,
      size: 96,
      sortUndefined: "last",
      cell: ({ getValue }) => (
        <span className="tabular-nums">
          {formatMetricValue(getValue<number | undefined>(), i18n.language)}
        </span>
      ),
    });

    const identityColumn: DataTableColumnDef<MetricLegendRow> = tableMode === "series"
      ? {
        accessorKey: "metric",
        header: t("metrics.fullMetric", { count: series.length }),
        size: 480,
        cell: ({ row }) => (
          <div className="flex min-w-0 items-start gap-2">
            <span
              className="mt-2 h-1 w-3.5 shrink-0 rounded-full"
              style={{ backgroundColor: row.original.color }}
            />
            <code className="min-w-0 select-text break-all leading-5">
              {row.original.metric}
            </code>
          </div>
        ),
      }
      : {
        accessorKey: "metricName",
        header: t("metrics.metricName"),
        size: 220,
        sortUndefined: "last",
        cell: ({ row }) => (
          <div className="flex min-w-0 items-start gap-2">
            <span
              className="mt-2 h-1 w-3.5 shrink-0 rounded-full"
              style={{ backgroundColor: row.original.color }}
            />
            {row.original.metricName === undefined
              ? <span className="text-muted-foreground">—</span>
              : <code className="min-w-0 select-text break-all leading-5">{row.original.metricName}</code>}
          </div>
        ),
      };
    const labelColumns: DataTableColumnDef<MetricLegendRow>[] = tableMode === "labels"
      ? labelNames.map((name) => ({
        id: `label:${name}`,
        accessorFn: (row) => row.labels[name],
        header: name,
        size: 160,
        sortUndefined: "last",
        cell: ({ getValue }) => {
          const value = getValue<string | undefined>();
          return value === undefined
            ? <span className="text-muted-foreground">—</span>
            : (
                <code
                  className="block min-w-0 break-all leading-5 select-text"
                  title={value}
                >
                  {value}
                </code>
              );
        },
      }))
      : [];

    return [
      identityColumn,
      ...labelColumns,
      statColumn("avg", t("metrics.average")),
      statColumn("max", t("metrics.maximum")),
      statColumn("min", t("metrics.minimum")),
      statColumn("last", t("metrics.latest")),
      statColumn("sum", t("metrics.total")),
    ];
  }, [i18n.language, labelNames, series.length, t, tableMode]);

  return (
    <div className="mt-3">
      <DataTable
        columns={columns}
        data={data}
        getRowId={(row) => row.id}
        getRowAriaSelected={(row) =>
          selectedIndexes.length === 0 || selectedIndexes.includes(row.original.index)}
        getRowStyle={(row) => ({
          opacity: selectedIndexes.length === 0 || selectedIndexes.includes(row.original.index)
            ? 1
            : 0.45,
        })}
        numericColumnIds={STAT_COLUMN_IDS}
        onRowClick={(row, event) => onRowClick(row.original.index, event)}
        resetSortingKey={tableMode}
        sortButtonLabel={t("metrics.sortColumn")}
        minWidth={960}
        className="max-h-80"
      />
    </div>
  );
}
