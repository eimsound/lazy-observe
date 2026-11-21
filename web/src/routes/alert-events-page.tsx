import { useState } from "react";
import { useTranslation } from "react-i18next";
import { BellRing } from "lucide-react";
import {
  listAlerts,
  eventSeverity,
  type AlertStatus,
  type AlertEvent,
} from "@/lib/api/alerts";
import { useProjectQuery } from "@/lib/project/use-project-query";
import { formatDateTime } from "@/lib/format";
import dayjs from "@/lib/dayjs";
import { PageActions, PageContainer } from "@/components/layout/page";
import { EmptyState } from "@/components/common/empty-state";
import { QueryErrorState } from "@/components/common/query-error-state";
import { QueryStaleNotice } from "@/components/common/query-stale-notice";
import {
  SeverityBadge,
  StatusBadge,
  InhibitedBadge,
  LabelChips,
} from "@/components/alerts/severity";
import { Card, CardContent } from "@/components/ui/card";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Skeleton } from "@/components/ui/skeleton";
import { cn } from "@/lib/utils";
import { queryViewState } from "./query-view-state";

type Filter = "all" | AlertStatus;

const FILTERS: { id: Filter; labelKey: string }[] = [
  { id: "all", labelKey: "alerts.filterAll" },
  { id: "firing", labelKey: "alerts.filterFiring" },
  { id: "resolved", labelKey: "alerts.filterResolved" },
];

const fmt = (iso?: string) => (iso ? formatDateTime(dayjs(iso).valueOf()) : "—");

export function AlertEventsPage() {
  const { t } = useTranslation();
  const [filter, setFilter] = useState<Filter>("all");

  const events = useProjectQuery({
    queryKey: ["alerts", "events"],
    queryFn: ({ signal, project }) => listAlerts(undefined, signal, project),
    refetchInterval: 30_000,
    retry: false,
  });

  const all = events.data ?? [];
  const firingCount = all.filter((e) => e.status === "firing").length;
  const rows = filter === "all" ? all : all.filter((e) => e.status === filter);
  const viewState = queryViewState({
    hasData: events.data !== undefined,
    isLoading: events.isLoading,
    isError: events.isError,
    isEmpty: rows.length === 0,
  });

  return (
    <PageContainer>
      {firingCount > 0 && (
        <PageActions>
          <Badge variant="destructive" className="gap-1.5">
            <BellRing className="size-3" />
            {t("alerts.activeCount", { count: firingCount })}
          </Badge>
        </PageActions>
      )}

      <div className="mb-4 inline-flex rounded-lg border border-border bg-muted/40 p-1">
        {FILTERS.map((f) => (
          <Button
            key={f.id}
            variant="ghost"
            size="sm"
            onClick={() => setFilter(f.id)}
            className={cn(
              "h-7 px-3",
              filter === f.id && "bg-background shadow-sm",
            )}
          >
            {t(f.labelKey)}
          </Button>
        ))}
      </div>

      {viewState === "stale" && (
        <QueryStaleNotice
          isRetrying={events.isFetching}
          onRetry={() => void events.refetch()}
        />
      )}

      <Card>
        <CardContent className="p-0">
          {viewState === "pending" ? (
            <div className="space-y-2 p-4">
              {[0, 1, 2].map((i) => (
                <Skeleton key={i} className="h-14 w-full" />
              ))}
            </div>
          ) : viewState === "error" ? (
            <QueryErrorState error={events.error} onRetry={() => void events.refetch()} />
          ) : viewState === "empty" ? (
            <EmptyState
              icon={<BellRing className="size-6" />}
              title={t("alerts.noEvents")}
              desc={t("alerts.noEventsDesc")}
            />
          ) : (
            <div className="overflow-x-auto">
              <table className="w-full text-sm">
                <thead>
                  <tr className="border-b border-border text-left text-xs uppercase tracking-wider text-muted-foreground">
                    <th className="px-4 py-2.5 font-medium">{t("alerts.colSeverity")}</th>
                    <th className="px-4 py-2.5 font-medium">{t("alerts.colRule")}</th>
                    <th className="hidden px-4 py-2.5 font-medium lg:table-cell">
                      {t("alerts.colLabels")}
                    </th>
                    <th className="px-4 py-2.5 font-medium">{t("alerts.colStarted")}</th>
                    <th className="hidden px-4 py-2.5 font-medium md:table-cell">
                      {t("alerts.colEnded")}
                    </th>
                    <th className="px-4 py-2.5 font-medium">{t("alerts.colStatus")}</th>
                  </tr>
                </thead>
                <tbody>
                  {rows.map((e: AlertEvent) => (
                    <tr key={e.fingerprint} className="border-b border-border/60 last:border-0 align-top">
                      <td className="px-4 py-3">
                        <SeverityBadge severity={eventSeverity(e)} />
                      </td>
                      <td className="px-4 py-3">
                        <div className="font-medium">{e.rule}</div>
                        {e.annotations.summary && (
                          <div className="mt-0.5 max-w-xs truncate text-xs text-muted-foreground">
                            {e.annotations.summary}
                          </div>
                        )}
                      </td>
                      <td className="hidden px-4 py-3 lg:table-cell">
                        <LabelChips labels={e.labels} exclude={["alertname", "severity"]} max={4} />
                      </td>
                      <td className="px-4 py-3 text-xs text-muted-foreground">{fmt(e.starts_at)}</td>
                      <td className="hidden px-4 py-3 text-xs text-muted-foreground md:table-cell">
                        {e.status === "firing" ? (
                          <span className="italic">{t("alerts.ongoing")}</span>
                        ) : (
                          fmt(e.ends_at)
                        )}
                      </td>
                      <td className="px-4 py-3">
                        <div className="flex flex-wrap items-center gap-1.5">
                          <StatusBadge status={e.status} />
                          {e.inhibited && <InhibitedBadge />}
                        </div>
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}
        </CardContent>
      </Card>
    </PageContainer>
  );
}
