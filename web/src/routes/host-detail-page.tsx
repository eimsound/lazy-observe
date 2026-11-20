import { useMemo, useState } from "react";
import { useTranslation } from "react-i18next";
import { useQueries, useQuery } from "@tanstack/react-query";
import { Link, useParams } from "@tanstack/react-router";
import { ArrowLeft, RefreshCw, Server } from "lucide-react";
import { listHosts } from "@/lib/api/hosts";
import { queryRange } from "@/lib/api/metrics";
import { PageActions, PageContainer } from "@/components/layout/page";
import { TimeSeriesChart } from "@/components/charts/time-series-chart";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent } from "@/components/ui/card";
import { Skeleton } from "@/components/ui/skeleton";

const WINDOW_SECONDS = 3600;
const STEP_SECONDS = 60;

function promString(value: string): string {
  return `"${value.replace(/\\/g, "\\\\").replace(/"/g, '\\"')}"`;
}

interface MetricPanelDefinition {
  titleKey: string;
  queries: string[];
}

function hostMetricPanels(instance: string): MetricPanelDefinition[] {
  const selector = `instance=${promString(instance)}`;
  return [
    {
      titleKey: "hosts.cpu",
      queries: [
        `100 * (1 - avg by(instance) (rate(node_cpu_seconds_total{mode="idle",${selector}}[5m])))`,
      ],
    },
    {
      titleKey: "hosts.memory",
      queries: [
        `100 * (1 - node_memory_MemAvailable_bytes{${selector}} / node_memory_MemTotal_bytes{${selector}})`,
      ],
    },
    {
      titleKey: "hosts.disk",
      queries: [
        `100 * (1 - node_filesystem_avail_bytes{${selector},fstype!~"tmpfs|overlay"} / node_filesystem_size_bytes{${selector},fstype!~"tmpfs|overlay"})`,
      ],
    },
    {
      titleKey: "hosts.network",
      queries: [
        `rate(node_network_receive_bytes_total{${selector},device!="lo"}[5m])`,
        `rate(node_network_transmit_bytes_total{${selector},device!="lo"}[5m])`,
      ],
    },
    {
      titleKey: "hosts.load1",
      queries: [
        `node_load1{${selector}}`,
        `node_load5{${selector}}`,
        `node_load15{${selector}}`,
      ],
    },
  ];
}

interface HostMetricWindow {
  start: number;
  end: number;
  step: number;
}

function MetricPanel({
  instance,
  titleKey,
  queries,
  window,
}: MetricPanelDefinition & { instance: string; window: HostMetricWindow }) {
  const { t } = useTranslation();
  const results = useQueries({
    queries: queries.map((query) => ({
      queryKey: ["host-metric", instance, query, window.start, window.end, window.step],
      queryFn: ({ signal }) => queryRange({ query, ...window }, signal),
      retry: false,
      staleTime: 30_000,
    })),
  });
  const series = results.flatMap((result) => result.data ?? []);
  const isLoading = results.some((result) => result.isLoading);
  const isError = results.some((result) => result.isError);

  return (
    <Card>
      <CardContent className="p-4">
        <h2 className="mb-2 text-sm font-medium">{t(titleKey)}</h2>
        {isLoading ? (
          <Skeleton className="h-[260px] w-full" />
        ) : isError ? (
          <p className="text-sm text-destructive">{t("hosts.metricsError")}</p>
        ) : series.length === 0 ? (
          <p className="text-sm text-muted-foreground">{t("hosts.metricsEmpty")}</p>
        ) : (
          <TimeSeriesChart
            series={series}
            height={260}
            timeWindow={{ startMs: window.start * 1000, endMs: window.end * 1000 }}
          />
        )}
      </CardContent>
    </Card>
  );
}

export function HostDetailPage() {
  const { t } = useTranslation();
  const { instance } = useParams({ from: "/app/infrastructure/hosts/$instance" });
  const [nowMs, setNowMs] = useState(() => Date.now());

  const window = useMemo<HostMetricWindow>(() => {
    const end = Math.floor(nowMs / 1000);
    return { start: end - WINDOW_SECONDS, end, step: STEP_SECONDS };
  }, [nowMs]);

  const hosts = useQuery({ queryKey: ["hosts"], queryFn: ({ signal }) => listHosts(signal) });
  const host = hosts.data?.find((h) => h.instance === instance);
  const panels = useMemo(() => hostMetricPanels(instance), [instance]);

  function refresh() {
    setNowMs((prev) => Math.max(prev + 1000, Date.now()));
    void hosts.refetch();
  }

  return (
    <PageContainer>
      <PageActions>
        <Button variant="outline" size="sm" onClick={refresh}>
          <RefreshCw className="size-3.5" />
          {t("common.refresh")}
        </Button>
      </PageActions>
      <div className="mb-4">
        <Button asChild variant="ghost" size="sm" className="mb-2 gap-1.5 text-muted-foreground">
          <Link to="/infrastructure/hosts">
            <ArrowLeft className="size-4" />
            {t("hosts.backToList")}
          </Link>
        </Button>
        <div className="flex flex-wrap items-center gap-3">
          <div className="flex size-9 items-center justify-center rounded-md bg-primary text-primary-foreground">
            <Server className="size-5" />
          </div>
          <div>
            <h1 className="flex items-center gap-2 text-2xl font-semibold tracking-tight">
              {host?.hostname ?? instance}
              {host && (
                <Badge
                  variant={
                    host.up === true
                      ? "outline"
                      : host.up === false
                        ? "destructive"
                        : "secondary"
                  }
                  className="text-[11px]"
                >
                  {host.up === true
                    ? t("hosts.statusUp")
                    : host.up === false
                      ? t("hosts.statusDown")
                      : t("hosts.statusUnknown")}
                </Badge>
              )}
            </h1>
            <p className="font-mono text-xs text-muted-foreground">{instance}</p>
          </div>
        </div>
      </div>

      <div className="grid gap-3 lg:grid-cols-2">
        {panels.map((panel) => (
          <MetricPanel key={panel.titleKey} instance={instance} window={window} {...panel} />
        ))}
      </div>
    </PageContainer>
  );
}
