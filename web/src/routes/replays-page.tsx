import { useEffect, useRef, useState } from "react";
import { useTranslation } from "react-i18next";
import { useNavigate } from "@tanstack/react-router";
import { AlertCircle, Database, Loader2, MonitorPlay, TriangleAlert } from "lucide-react";
import {
  listReplaySessions,
  type ReplayListResult,
} from "@/lib/api/replay";
import { IS_MOCK } from "@/lib/settings";
import { ApiError } from "@/lib/api/client";
import { useProject } from "@/lib/project/project-provider";
import { useProjectQuery } from "@/lib/project/use-project-query";
import { formatBytes, formatClockDuration, formatDateTime } from "@/lib/format";
import { PageActions, PageContainer } from "@/components/layout/page";
import { EmptyState } from "@/components/common/empty-state";
import { QueryStaleNotice } from "@/components/common/query-stale-notice";
import { Button } from "@/components/ui/button";
import { Badge } from "@/components/ui/badge";
import { Card, CardContent } from "@/components/ui/card";
import { Skeleton } from "@/components/ui/skeleton";
import { Switch } from "@/components/ui/switch";
import { Label } from "@/components/ui/label";
import { queryViewState } from "./query-view-state";

export function ReplaysPage() {
  const { t } = useTranslation();
  const navigate = useNavigate();
  const { currentSlug } = useProject();
  const [errorsOnly, setErrorsOnly] = useState(false);
  const [page, setPage] = useState(1);
  const prevProjectRef = useRef(currentSlug);

  useEffect(() => {
    if (prevProjectRef.current !== currentSlug) {
      prevProjectRef.current = currentSlug;
      setPage(1);
    }
  }, [currentSlug]);

  const query = useProjectQuery<ReplayListResult, ApiError>({
    queryKey: ["replays", errorsOnly, page],
    queryFn: ({ signal, project }) =>
      listReplaySessions({ has_error: errorsOnly ? true : undefined, page }, signal, project),
    retry: false,
    placeholderData: (prev) => prev,
  });

  const sessions = query.data?.sessions ?? [];
  const viewState = queryViewState({
    hasData: query.data !== undefined,
    isLoading: query.isLoading,
    isError: query.isError,
    isEmpty: sessions.length === 0,
  });

  return (
    <PageContainer>
      {IS_MOCK && (
        <PageActions>
          <Badge variant="secondary" className="gap-1.5">
            <Database className="size-3" />
            {t("replays.mockBadge")}
          </Badge>
        </PageActions>
      )}

      <Card className="mb-4">
        <CardContent className="flex items-center gap-3 p-3">
          <Switch
            id="errors-only"
            checked={errorsOnly}
            onCheckedChange={(v) => {
              setErrorsOnly(v);
              setPage(1);
            }}
          />
          <Label htmlFor="errors-only" className="cursor-pointer">
            {t("replays.filterErrorsOnly")}
          </Label>
        </CardContent>
      </Card>

      {viewState === "pending" ? (
        <Card>
          <CardContent className="space-y-2 p-4">
            <div className="flex items-center gap-2 text-sm text-muted-foreground">
              <Loader2 className="size-4 animate-spin" />
              {t("replays.loading")}
            </div>
            {Array.from({ length: 6 }).map((_, i) => (
              <Skeleton key={i} className="h-16 w-full" />
            ))}
          </CardContent>
        </Card>
      ) : viewState === "error" ? (
        <Card>
          <CardContent className="py-16">
            <EmptyState
              icon={<AlertCircle className="size-6 text-destructive" />}
              title={t("replays.errorTitle")}
              desc={t("common.queryErrorDesc")}
              action={
                <Button variant="outline" size="sm" onClick={() => query.refetch()}>
                  {t("common.retry")}
                </Button>
              }
            />
          </CardContent>
        </Card>
      ) : viewState === "empty" ? (
        <Card>
          <CardContent className="py-16">
            <EmptyState
              icon={<MonitorPlay className="size-6" />}
              title={t("replays.emptyTitle")}
              desc={t("replays.emptyDesc")}
            />
          </CardContent>
        </Card>
      ) : (
        <>
          {viewState === "stale" && (
            <QueryStaleNotice
              isRetrying={query.isFetching}
              onRetry={() => void query.refetch()}
            />
          )}
          <div className="mb-2 text-xs text-muted-foreground">
            {t("replays.resultsCount", { count: query.data?.total ?? sessions.length })}
          </div>
          <div className="space-y-2">
            {sessions.map((s) => (
              <Card
                key={s.session_id}
                role="button"
                tabIndex={0}
                onClick={() =>
                  navigate({ to: "/replays/$sessionId", params: { sessionId: s.session_id } })
                }
                onKeyDown={(e) => {
                  if (e.key === "Enter")
                    void navigate({ to: "/replays/$sessionId", params: { sessionId: s.session_id } });
                }}
                className="cursor-pointer transition-colors hover:border-ring hover:bg-muted/30"
              >
                <CardContent className="flex items-center gap-4 p-3">
                  <MonitorPlay className="size-5 shrink-0 text-muted-foreground" />
                  <div className="min-w-0 flex-1">
                    <div className="flex items-center gap-2">
                      <span className="truncate text-sm font-medium">{s.page_url}</span>
                      {s.has_error && (
                        <Badge variant="destructive" className="shrink-0 gap-1">
                          <TriangleAlert className="size-3" />
                          {t("replays.hasError")}
                        </Badge>
                      )}
                    </div>
                    <div className="mt-1 flex flex-wrap items-center gap-x-4 gap-y-0.5 text-xs text-muted-foreground">
                      <span>{formatDateTime(s.started_at)}</span>
                      <span>{t("replays.segments", { count: s.segment_count })}</span>
                      <span>{formatBytes(s.total_bytes)}</span>
                      <span className="truncate font-mono">{s.session_id.slice(0, 20)}</span>
                    </div>
                  </div>
                  <div className="shrink-0 text-right">
                    <div className="text-sm font-semibold tabular-nums">
                      {formatClockDuration(s.last_seen_at - s.started_at)}
                    </div>
                    <div className="text-[11px] text-muted-foreground">{t("replays.colDuration")}</div>
                  </div>
                </CardContent>
              </Card>
            ))}
          </div>

          {query.data && query.data.total > query.data.page_size && (
            <div className="mt-4 flex items-center justify-end gap-2">
              <Button size="sm" variant="outline" disabled={page <= 1} onClick={() => setPage((p) => p - 1)}>
                {t("issues.pagePrev")}
              </Button>
              <Button
                size="sm"
                variant="outline"
                disabled={page * query.data.page_size >= query.data.total}
                onClick={() => setPage((p) => p + 1)}
              >
                {t("issues.pageNext")}
              </Button>
            </div>
          )}
        </>
      )}
    </PageContainer>
  );
}
