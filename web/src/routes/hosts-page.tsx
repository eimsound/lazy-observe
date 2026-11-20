import { useTranslation } from "react-i18next";
import { useNavigate } from "@tanstack/react-router";
import { Cpu, Database, Server } from "lucide-react";
import {
  listHosts,
  usageRating,
  loadRating,
  type Host,
  type UsageRating,
} from "@/lib/api/hosts";
import { IS_MOCK } from "@/lib/settings";
import { useProjectQuery } from "@/lib/project/use-project-query";
import { PageActions, PageContainer } from "@/components/layout/page";
import { EmptyState } from "@/components/common/empty-state";
import { QueryErrorState } from "@/components/common/query-error-state";
import { QueryStaleNotice } from "@/components/common/query-stale-notice";
import { Badge } from "@/components/ui/badge";
import { Card, CardContent } from "@/components/ui/card";
import { Skeleton } from "@/components/ui/skeleton";
import { cn } from "@/lib/utils";
import { queryViewState } from "./query-view-state";

const RATING_TEXT: Record<UsageRating, string> = {
  good: "text-vital-good",
  ni: "text-vital-ni",
  poor: "text-vital-poor",
};
const RATING_BAR: Record<UsageRating, string> = {
  good: "bg-vital-good",
  ni: "bg-vital-ni",
  poor: "bg-vital-poor",
};

export function HostsPage() {
  const { t } = useTranslation();
  const navigate = useNavigate();
  const hosts = useProjectQuery({
    queryKey: ["hosts"],
    queryFn: ({ signal, project }) => listHosts(signal, project),
    retry: false,
  });

  function openHost(instance: string) {
    void navigate({ to: "/infrastructure/hosts/$instance", params: { instance } });
  }

  const rows = hosts.data ?? [];
  const viewState = queryViewState({
    hasData: hosts.data !== undefined,
    isLoading: hosts.isLoading,
    isError: hosts.isError,
    isEmpty: rows.length === 0,
  });

  return (
    <PageContainer>
      {IS_MOCK && (
        <PageActions>
          <Badge variant="secondary" className="gap-1.5">
            <Database className="size-3" />
            {t("hosts.mockBadge")}
          </Badge>
        </PageActions>
      )}

      {viewState === "stale" && (
        <QueryStaleNotice
          isRetrying={hosts.isFetching}
          onRetry={() => void hosts.refetch()}
        />
      )}

      {viewState === "pending" ? (
        <div className="grid gap-3 sm:grid-cols-2 lg:grid-cols-3">
          {[0, 1, 2, 3, 4, 5].map((i) => (
            <Skeleton key={i} className="h-44 w-full" />
          ))}
        </div>
      ) : viewState === "error" ? (
        <Card>
          <CardContent>
            <QueryErrorState
              error={hosts.error}
              errorTitle={t("hosts.errorTitle")}
              onRetry={() => void hosts.refetch()}
            />
          </CardContent>
        </Card>
      ) : viewState === "empty" ? (
        <Card>
          <CardContent>
            <EmptyState
              icon={<Server className="size-6" />}
              title={t("hosts.empty")}
              desc={t("hosts.emptyDesc")}
            />
          </CardContent>
        </Card>
      ) : (
        <div className="grid gap-3 sm:grid-cols-2 lg:grid-cols-3">
          {rows.map((h) => (
            <HostCard key={h.instance} host={h} onOpen={() => openHost(h.instance)} />
          ))}
        </div>
      )}
    </PageContainer>
  );
}

function HostCard({ host, onOpen }: { host: Host; onOpen: () => void }) {
  const { t } = useTranslation();
  const load = loadRating(host.load1, host.cpu_cores);
  const statusLabel =
    host.up === true
      ? t("hosts.statusUp")
      : host.up === false
        ? t("hosts.statusDown")
        : t("hosts.statusUnknown");

  return (
    <button
      type="button"
      onClick={onOpen}
      className="rounded-lg border border-border bg-card p-4 text-left transition-colors hover:border-ring"
    >
      <div className="flex items-start justify-between gap-2">
        <div className="min-w-0">
          <div className="flex items-center gap-2">
            <span
              className={cn(
                "size-2.5 shrink-0 rounded-full",
                host.up === true
                  ? "bg-vital-good"
                  : host.up === false
                    ? "bg-vital-poor"
                    : "bg-muted-foreground",
              )}
              aria-hidden="true"
            />
            <span className="truncate font-medium">{host.hostname}</span>
          </div>
          <div className="mt-0.5 truncate font-mono text-xs text-muted-foreground">
            {host.instance}
          </div>
        </div>
        <Badge
          variant={
            host.up === true ? "outline" : host.up === false ? "destructive" : "secondary"
          }
          className="shrink-0 text-[11px]"
        >
          {statusLabel}
        </Badge>
      </div>

      <div className="mt-3 flex items-center gap-4 text-xs text-muted-foreground">
        <span className="inline-flex items-center gap-1">
          <Cpu className="size-3.5" />
          {host.cpu_cores == null ? "—" : t("hosts.cores", { count: host.cpu_cores })}
        </span>
        <span>
          {t("hosts.load1")}:{" "}
          <span
            className={cn(
              "font-medium tabular-nums",
              load ? RATING_TEXT[load] : "text-muted-foreground",
            )}
          >
            {host.load1 == null ? "—" : host.load1.toFixed(2)}
          </span>
        </span>
      </div>

      <div className="mt-3 space-y-2">
        <UsageBar label={t("hosts.memory")} percent={host.mem_used_percent} />
        <UsageBar label={t("hosts.disk")} percent={host.disk_used_percent_max} />
      </div>
    </button>
  );
}

function UsageBar({ label, percent }: { label: string; percent: number | null }) {
  const rating = percent == null ? null : usageRating(percent);
  const width = percent == null ? 0 : Math.max(0, Math.min(100, percent));
  return (
    <div>
      <div className="mb-1 flex items-center justify-between text-[11px]">
        <span className="text-muted-foreground">{label}</span>
        <span
          className={cn(
            "font-medium tabular-nums",
            rating ? RATING_TEXT[rating] : "text-muted-foreground",
          )}
        >
          {percent == null ? "—" : `${percent.toFixed(1)}%`}
        </span>
      </div>
      <div className="h-1.5 w-full overflow-hidden rounded-full bg-muted">
        <div
          className={cn(
            "h-full rounded-full transition-all",
            rating ? RATING_BAR[rating] : "bg-muted-foreground/30",
          )}
          style={{ width: `${width}%` }}
        />
      </div>
    </div>
  );
}
