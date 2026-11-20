import { useTranslation } from "react-i18next";
import { useNavigate, useParams } from "@tanstack/react-router";
import { AlertCircle, ArrowLeft, Loader2, TriangleAlert } from "lucide-react";
import {
  getReplaySegment,
  getReplaySession,
  type ReplaySessionDetail,
  type RRWebEvent,
} from "@/lib/api/replay";
import { ApiError } from "@/lib/api/client";
import { useProjectQuery } from "@/lib/project/use-project-query";
import { formatBytes, formatClockDuration, formatDateTime } from "@/lib/format";
import { PageContainer } from "@/components/layout/page";
import { QueryStaleNotice } from "@/components/common/query-stale-notice";
import { ReplayPlayer } from "@/components/replay/replay-player";
import { Button } from "@/components/ui/button";
import { Badge } from "@/components/ui/badge";
import { Card, CardContent } from "@/components/ui/card";
import { Skeleton } from "@/components/ui/skeleton";
import { queryViewState } from "./query-view-state";

export function ReplayDetailPage() {
  const { t } = useTranslation();
  const navigate = useNavigate();
  const { sessionId } = useParams({ strict: false }) as { sessionId: string };

  const detailQuery = useProjectQuery<ReplaySessionDetail, ApiError>({
    queryKey: ["replay", sessionId],
    queryFn: ({ signal, project }) => getReplaySession(sessionId, signal, project),
    retry: false,
    placeholderData: (previous) => previous,
  });

  // Load every segment in order, then concatenate into one event stream.
  const segmentsQuery = useProjectQuery<RRWebEvent[], ApiError>({
    queryKey: ["replay-segments", sessionId, detailQuery.data?.segment_count],
    enabled: !!detailQuery.data,
    retry: false,
    placeholderData: (previous) => previous,
    queryFn: async ({ signal, project }) => {
      const detail = detailQuery.data!;
      const all: RRWebEvent[] = [];
      for (const seg of detail.segments) {
        const events = await getReplaySegment(sessionId, seg.seq, signal, project);
        all.push(...events);
      }
      return all;
    },
  });
  const detailState = queryViewState({
    hasData: detailQuery.data !== undefined,
    isLoading: detailQuery.isLoading,
    isError: detailQuery.isError,
    isEmpty: false,
  });
  const segmentsState = queryViewState({
    hasData: segmentsQuery.data !== undefined,
    isLoading: segmentsQuery.isLoading,
    isError: segmentsQuery.isError,
    isEmpty: segmentsQuery.data?.length === 0,
  });

  return (
    <PageContainer>
      <div className="mb-4">
        <Button variant="ghost" size="sm" onClick={() => navigate({ to: "/replays" })}>
          <ArrowLeft className="size-4" />
          {t("replays.backToList")}
        </Button>
      </div>

      {detailState === "pending" ? (
        <Card>
          <CardContent className="space-y-3 p-4">
            <div className="flex items-center gap-2 text-sm text-muted-foreground">
              <Loader2 className="size-4 animate-spin" />
              {t("replays.loadingSession")}
            </div>
            <Skeleton className="h-64 w-full" />
          </CardContent>
        </Card>
      ) : detailState === "error" || !detailQuery.data ? (
        <Card>
          <CardContent className="py-24">
            <div className="flex flex-col items-center gap-3 text-center">
              <div className="flex size-12 items-center justify-center rounded-full bg-muted text-destructive">
                <AlertCircle className="size-6" />
              </div>
              <div className="text-base font-medium">
                {detailState === "error" ? t("replays.errorTitle") : t("replays.notFoundTitle")}
              </div>
              <p className="max-w-md text-sm text-muted-foreground">
                {detailState === "error" ? t("common.queryErrorDesc") : t("replays.notFoundDesc")}
              </p>
              <div className="flex gap-2">
                {detailState === "error" && (
                  <Button variant="outline" size="sm" onClick={() => void detailQuery.refetch()}>
                    {t("common.retry")}
                  </Button>
                )}
                <Button variant="outline" size="sm" onClick={() => navigate({ to: "/replays" })}>
                  {t("replays.backToList")}
                </Button>
              </div>
            </div>
          </CardContent>
        </Card>
      ) : (
        <div className="space-y-4">
          {detailState === "stale" && (
            <QueryStaleNotice
              isRetrying={detailQuery.isFetching}
              onRetry={() => void detailQuery.refetch()}
            />
          )}
          <MetadataBar detail={detailQuery.data} />

          <Card>
            <CardContent className="p-4">
              {segmentsState === "pending" ? (
                <div className="space-y-3">
                  <div className="flex items-center gap-2 text-sm text-muted-foreground">
                    <Loader2 className="size-4 animate-spin" />
                    {t("replays.loadingPlayer")}
                  </div>
                  <Skeleton className="aspect-video w-full" />
                </div>
              ) : segmentsState === "error" || !segmentsQuery.data ? (
                <div className="flex flex-col items-center gap-2 py-12 text-center">
                  <AlertCircle className="size-6 text-destructive" />
                  <div className="text-sm font-medium">{t("replays.playerError")}</div>
                  <p className="text-sm text-muted-foreground">
                    {t("common.queryErrorDesc")}
                  </p>
                  <Button variant="outline" size="sm" onClick={() => void segmentsQuery.refetch()}>
                    {t("common.retry")}
                  </Button>
                </div>
              ) : segmentsState === "empty" ? (
                <div className="flex min-h-40 items-center justify-center text-sm text-muted-foreground">
                  {t("replays.playerEmpty")}
                </div>
              ) : (
                <>
                  {segmentsState === "stale" && (
                    <QueryStaleNotice
                      isRetrying={segmentsQuery.isFetching}
                      onRetry={() => void segmentsQuery.refetch()}
                    />
                  )}
                  <ReplayPlayer
                    events={segmentsQuery.data}
                    startedAt={detailQuery.data.started_at}
                    durationMs={detailQuery.data.last_seen_at - detailQuery.data.started_at}
                    markers={detailQuery.data.markers}
                    onOpenIssue={(issueId, eventId) =>
                      navigate({
                        to: "/issues/$issueId",
                        params: { issueId },
                        search: { event: eventId },
                      })
                    }
                  />
                </>
              )}
            </CardContent>
          </Card>
        </div>
      )}
    </PageContainer>
  );
}

function MetadataBar({ detail }: { detail: ReplaySessionDetail }) {
  const { t } = useTranslation();
  return (
    <div className="space-y-2">
      <div className="flex flex-wrap items-center gap-2">
        <h1 className="text-lg font-semibold tracking-tight">{detail.page_url}</h1>
        {detail.has_error && (
          <Badge variant="destructive" className="gap-1">
            <TriangleAlert className="size-3" />
            {t("replays.hasError")}
          </Badge>
        )}
      </div>
      <div className="flex flex-wrap gap-x-6 gap-y-1 text-xs text-muted-foreground">
        <Stat label={t("replays.sessionId")} value={detail.session_id} mono />
        <Stat label={t("replays.startedAt")} value={formatDateTime(detail.started_at)} />
        <Stat
          label={t("replays.duration")}
          value={formatClockDuration(detail.last_seen_at - detail.started_at)}
        />
        <Stat label={t("replays.segmentCount")} value={String(detail.segment_count)} />
        <Stat label={t("replays.totalSize")} value={formatBytes(detail.total_bytes)} />
      </div>
    </div>
  );
}

function Stat({ label, value, mono }: { label: string; value: string; mono?: boolean }) {
  return (
    <span className="flex items-center gap-1.5">
      <span>{label}:</span>
      <span className={mono ? "font-mono text-foreground" : "font-medium text-foreground"}>
        {value}
      </span>
    </span>
  );
}
