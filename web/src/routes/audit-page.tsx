import { useState } from "react";
import { useTranslation } from "react-i18next";
import { keepPreviousData, useQuery } from "@tanstack/react-query";
import { ChevronLeft, ChevronRight, History, ShieldAlert, ShieldCheck } from "lucide-react";
import { listAudit, type AuditEvent } from "@/lib/api/audit";
import { useAuth } from "@/lib/auth/auth-provider";
import { hasCapability } from "@/lib/auth/permissions";
import {
  INITIAL_CURSOR_PAGINATION,
  canGoBackCursor,
  popCursorPage,
  pushCursorPage,
  type CursorPaginationState,
} from "@/lib/cursor-pagination";
import { formatDateTime } from "@/lib/format";
import dayjs from "@/lib/dayjs";
import { PageContainer } from "@/components/layout/page";
import { EmptyState } from "@/components/common/empty-state";
import { QueryErrorState } from "@/components/common/query-error-state";
import { QueryStaleNotice } from "@/components/common/query-stale-notice";
import { Button } from "@/components/ui/button";
import { Card, CardContent } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Skeleton } from "@/components/ui/skeleton";
import { queryViewState } from "./query-view-state";

const PAGE_SIZE = 100;
const fmt = (iso: string) => formatDateTime(dayjs(iso).valueOf());

export function AuditPage() {
  const { t } = useTranslation();
  const { role } = useAuth();
  const canRead = hasCapability(role, "audit:read");
  const [action, setAction] = useState("");
  const [actor, setActor] = useState("");
  const [pagination, setPagination] = useState<CursorPaginationState>(INITIAL_CURSOR_PAGINATION);

  const audit = useQuery({
    queryKey: [
      "audit",
      {
        action: action.trim() || undefined,
        actor: actor.trim() || undefined,
        before: pagination.before,
      },
    ],
    queryFn: ({ signal }) =>
      listAudit(
        {
          action: action.trim() || undefined,
          actor: actor.trim() || undefined,
          before_id: pagination.before,
          limit: PAGE_SIZE,
        },
        signal,
      ),
    placeholderData: keepPreviousData,
    enabled: canRead,
    retry: false,
  });

  if (!canRead) {
    return (
      <PageContainer>
        <Card>
          <CardContent>
            <EmptyState
              icon={<ShieldAlert className="size-6" />}
              title={t("audit.forbiddenTitle")}
              desc={t("audit.forbiddenDesc")}
            />
          </CardContent>
        </Card>
      </PageContainer>
    );
  }

  const events: AuditEvent[] = audit.data ?? [];
  const hasMore = events.length === PAGE_SIZE;
  const viewState = queryViewState({
    hasData: audit.data !== undefined,
    isLoading: audit.isLoading,
    isError: audit.isError,
    isEmpty: events.length === 0,
  });

  const goBack = () => {
    setPagination((prev) => popCursorPage(prev));
  };
  const goForward = () => {
    const oldest = events[events.length - 1];
    if (!oldest) return;
    setPagination((prev) => pushCursorPage(prev, oldest.id));
  };

  return (
    <PageContainer>
      <div className="mb-4 flex flex-wrap gap-2">
        <Input
          className="w-56"
          value={action}
          onChange={(event) => {
            setAction(event.target.value);
            setPagination(INITIAL_CURSOR_PAGINATION);
          }}
          placeholder={t("audit.filterAction")}
        />
        <Input
          className="w-56"
          value={actor}
          onChange={(event) => {
            setActor(event.target.value);
            setPagination(INITIAL_CURSOR_PAGINATION);
          }}
          placeholder={t("audit.filterActor")}
        />
      </div>
      {viewState === "stale" && (
        <QueryStaleNotice
          isRetrying={audit.isFetching}
          onRetry={() => void audit.refetch()}
        />
      )}
      <Card>
        <CardContent className="p-0">
          {viewState === "pending" ? (
            <div className="space-y-2 p-4">
              {[0, 1, 2, 3].map((i) => (
                <Skeleton key={i} className="h-10 w-full" />
              ))}
            </div>
          ) : viewState === "error" ? (
            <QueryErrorState
              error={audit.error}
              forbiddenTitle={t("audit.forbiddenTitle")}
              forbiddenDesc={t("audit.forbiddenDesc")}
              onRetry={() => void audit.refetch()}
            />
          ) : viewState === "empty" ? (
            <EmptyState icon={<History className="size-6" />} title={t("audit.empty")} />
          ) : (
            <div className="overflow-x-auto">
              <table className="w-full text-sm">
                <thead>
                  <tr className="border-b border-border text-left text-xs uppercase tracking-wider text-muted-foreground">
                    <th className="px-4 py-2.5 font-medium">{t("audit.colWhen")}</th>
                    <th className="px-4 py-2.5 font-medium">{t("audit.colActor")}</th>
                    <th className="px-4 py-2.5 font-medium">{t("audit.colAction")}</th>
                    <th className="px-4 py-2.5 font-medium">{t("audit.colTarget")}</th>
                    <th className="hidden px-4 py-2.5 font-medium md:table-cell">{t("audit.colStatus")}</th>
                  </tr>
                </thead>
                <tbody>
                  {events.map((event) => (
                    <tr key={event.id} className="border-b border-border/60 last:border-0">
                      <td className="whitespace-nowrap px-4 py-2.5 text-muted-foreground">
                        {fmt(event.created_at)}
                      </td>
                      <td className="px-4 py-2.5">
                        <span className="font-medium">{event.actor_username}</span>
                        {event.actor_role && (
                          <span className="ml-1.5 text-xs text-muted-foreground">{event.actor_role}</span>
                        )}
                      </td>
                      <td className="px-4 py-2.5 font-mono text-xs">{event.action}</td>
                      <td className="px-4 py-2.5">
                        {event.target_name || event.target_type}
                        {event.project_slug && (
                          <span className="ml-1.5 text-xs text-muted-foreground">
                            {event.project_slug}
                          </span>
                        )}
                      </td>
                      <td className="hidden px-4 py-2.5 md:table-cell">
                        {event.success ? (
                          <ShieldCheck className="size-4 text-primary" />
                        ) : (
                          <span className="text-xs text-destructive">
                            {event.failure_reason || "failed"}
                          </span>
                        )}
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}
        </CardContent>
      </Card>
      <div className="mt-4 flex items-center justify-end gap-2">
        <Button
          variant="outline"
          size="sm"
          onClick={goBack}
          disabled={!canGoBackCursor(pagination) || audit.isLoading}
        >
          <ChevronLeft className="size-4" />
          {t("audit.newer")}
        </Button>
        <Button
          variant="outline"
          size="sm"
          onClick={goForward}
          disabled={!hasMore || audit.isLoading || audit.isError}
        >
          {t("audit.older")}
          <ChevronRight className="size-4" />
        </Button>
      </div>
    </PageContainer>
  );
}
