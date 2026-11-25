import { useNavigate, useSearch } from "@tanstack/react-router";
import { useTranslation } from "react-i18next";
import type { TFunction } from "i18next";
import { BellRing, X } from "lucide-react";
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
import { useResetOnProjectChange } from "@/hooks/use-reset-on-project-change";
import { queryViewState } from "./query-view-state";
import {
  alertEventsChip,
  filterLinkedEvents,
  type AlertEventsChip,
  type AlertEventsSearch,
} from "./alert-events-search";

type Filter = "all" | AlertStatus;

const FILTERS: { id: Filter; labelKey: string }[] = [
  { id: "all", labelKey: "alerts.filterAll" },
  { id: "firing", labelKey: "alerts.filterFiring" },
  { id: "resolved", labelKey: "alerts.filterResolved" },
];

const fmt = (iso?: string) => (iso ? formatDateTime(dayjs(iso).valueOf()) : "—");

function chipLabel(chip: AlertEventsChip, t: TFunction): string {
  switch (chip.kind) {
    case "event":
      return t("alerts.filterEvent", { rule: chip.rule });
    case "fingerprint":
      return t("alerts.filterFingerprint", { fingerprint: chip.fingerprint });
    case "rule":
      return t("alerts.filterRule", { rule: chip.rule });
  }
}

export function AlertEventsPage() {
  const { t } = useTranslation();
  const navigate = useNavigate();
  // Status, fingerprint and rule live in the URL so alert notifications can
  // deep-link to one alert instance (`?fingerprint=`) or one rule (`?rule=`).
  const search = useSearch({ strict: false }) as AlertEventsSearch;
  const filter: Filter = search.status ?? "all";

  // The fingerprint/rule filter belongs to the project it came from.
  useResetOnProjectChange(() => {
    void navigate({ to: "/alerts", search: {}, replace: true });
  });

  const setFilter = (next: Filter) => {
    void navigate({
      to: "/alerts",
      search: (prev: AlertEventsSearch) => ({ ...prev, status: next === "all" ? undefined : next }),
      replace: true,
    });
  };
  const clearLinkFilter = () => {
    void navigate({
      to: "/alerts",
      search: (prev: AlertEventsSearch) => ({ status: prev.status }),
      replace: true,
    });
  };

  const events = useProjectQuery({
    queryKey: ["alerts", "events"],
    queryFn: ({ signal, project }) => listAlerts(undefined, signal, project),
    refetchInterval: 30_000,
    retry: false,
  });

  const all = events.data ?? [];
  const firingCount = all.filter((e) => e.status === "firing").length;
  const chip = alertEventsChip(all, search);
  const linked = filterLinkedEvents(all, search);
  const rows = filter === "all" ? linked : linked.filter((e) => e.status === filter);
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

      {chip && (
        <div className="-mt-2 mb-4 flex flex-wrap items-center gap-2">
          <span className="inline-flex h-7 max-w-full items-center rounded-md border border-border bg-background text-xs shadow-sm">
            <span
              className={cn("max-w-64 truncate px-2", chip.kind === "fingerprint" && "font-mono")}
              title={chipLabel(chip, t)}
            >
              {chipLabel(chip, t)}
            </span>
            <button
              type="button"
              onClick={clearLinkFilter}
              aria-label={t("alerts.clearFilter")}
              title={t("alerts.clearFilter")}
              className="flex h-full items-center border-l border-border px-1.5 text-muted-foreground transition-colors hover:bg-accent hover:text-foreground focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
            >
              <X className="size-3" aria-hidden="true" />
            </button>
          </span>
        </div>
      )}

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
              action={chip ? (
                <Button variant="outline" size="sm" onClick={clearLinkFilter}>
                  {t("alerts.clearFilter")}
                </Button>
              ) : undefined}
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
                    <tr key={e.id} className="border-b border-border/60 last:border-0 align-top">
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
                        {chip && (
                          // The labels column is hidden below lg; a linked
                          // event still needs its labels to be identifiable.
                          <div className="mt-1.5 lg:hidden">
                            <LabelChips labels={e.labels} exclude={["alertname", "severity"]} max={4} />
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
