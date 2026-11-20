import { useEffect, useMemo, useState } from "react";
import { useTranslation } from "react-i18next";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useNavigate, useParams, useSearch } from "@tanstack/react-router";
import {
  AlertCircle,
  ArrowLeft,
  Braces,
  CheckCircle2,
  ChevronLeft,
  ChevronRight,
  Loader2,
  PlayCircle,
  RotateCcw,
  VolumeX,
  Waypoints,
} from "lucide-react";
import {
  getIssue,
  getIssueEvent,
  updateIssueStatus,
  type ErrorEvent,
  type IssueDetail,
  type IssueStatus,
} from "@/lib/api/issues";
import { ApiError } from "@/lib/api/client";
import { useAuth } from "@/lib/auth/auth-provider";
import { canManageIssues } from "@/lib/auth/permissions";
import { formatErrorMessage } from "@/lib/api/errors";
import { formatDateTime, formatRelativeTime } from "@/lib/format";
import { PageContainer } from "@/components/layout/page";
import { StacktraceViewer } from "@/components/issues/stacktrace-viewer";
import { EventJsonDrawer } from "@/components/issues/event-json-drawer";
import { IssueLevelBadge } from "@/components/issues/issue-level-badge";
import { IssueAiAnalysisCard } from "@/components/ai/issue-ai-analysis-card";
import { Button } from "@/components/ui/button";
import { Card, CardContent } from "@/components/ui/card";
import { Skeleton } from "@/components/ui/skeleton";

export function IssueDetailPage() {
  const { t } = useTranslation();
  const { role } = useAuth();
  const canManage = canManageIssues(role);
  const navigate = useNavigate();
  const { issueId } = useParams({ strict: false }) as { issueId: string };
  const search = useSearch({ strict: false }) as { event?: string };
  const queryClient = useQueryClient();

  const [selectedEventId, setSelectedEventId] = useState<string | undefined>(search.event);
  const [jsonOpen, setJsonOpen] = useState(false);
  const [statusError, setStatusError] = useState<string | null>(null);

  const issueQuery = useQuery<IssueDetail, ApiError>({
    queryKey: ["issue", issueId],
    queryFn: ({ signal }) => getIssue(issueId, signal),
    retry: false,
  });

  const events = useMemo(() => issueQuery.data?.events ?? [], [issueQuery.data?.events]);

  // Default the event switcher to the deep-linked event, else the newest.
  useEffect(() => {
    if (events.length === 0) return;
    if (!selectedEventId || !events.some((e) => e.event_id === selectedEventId)) {
      setSelectedEventId(search.event && events.some((e) => e.event_id === search.event)
        ? search.event
        : events[0].event_id);
    }
  }, [events, selectedEventId, search.event]);

  const eventQuery = useQuery<ErrorEvent, ApiError>({
    queryKey: ["issue-event", issueId, selectedEventId],
    enabled: !!selectedEventId,
    queryFn: ({ signal }) => getIssueEvent(issueId, selectedEventId!, signal),
    retry: false,
  });

  const statusMutation = useMutation({
    mutationFn: (next: IssueStatus) => updateIssueStatus(issueId, next),
    onMutate: () => setStatusError(null),
    onSuccess: (updated) => {
      queryClient.setQueryData<IssueDetail>(["issue", issueId], (old) =>
        old ? { ...old, status: updated.status } : old,
      );
      void queryClient.invalidateQueries({ queryKey: ["issues"] });
    },
    onError: (err) => setStatusError(formatErrorMessage(err, t("issues.statusUpdateError"))),
  });

  const selectedIndex = useMemo(
    () => events.findIndex((e) => e.event_id === selectedEventId),
    [events, selectedEventId],
  );

  return (
    <PageContainer>
      <div className="mb-4">
        <Button variant="ghost" size="sm" onClick={() => navigate({ to: "/issues" })}>
          <ArrowLeft className="size-4" />
          {t("issues.backToList")}
        </Button>
      </div>

      {issueQuery.isLoading ? (
        <Card>
          <CardContent className="space-y-3 p-4">
            <div className="flex items-center gap-2 text-sm text-muted-foreground">
              <Loader2 className="size-4 animate-spin" />
              {t("issues.loading")}
            </div>
            <Skeleton className="h-24 w-full" />
            <Skeleton className="h-40 w-full" />
          </CardContent>
        </Card>
      ) : issueQuery.error || !issueQuery.data ? (
        <Card>
          <CardContent className="py-24">
            <div className="flex flex-col items-center gap-3 text-center">
              <div className="flex size-12 items-center justify-center rounded-full bg-muted text-destructive">
                <AlertCircle className="size-6" />
              </div>
              <div className="text-base font-medium">{t("issues.notFoundTitle")}</div>
              <p className="max-w-md text-sm text-muted-foreground">
                {issueQuery.error?.message ?? t("issues.notFoundDesc")}
              </p>
              <Button variant="outline" size="sm" onClick={() => navigate({ to: "/issues" })}>
                {t("issues.backToList")}
              </Button>
            </div>
          </CardContent>
        </Card>
      ) : (
        <IssueDetailBody
          issue={issueQuery.data}
          event={eventQuery.data}
          eventLoading={eventQuery.isLoading}
          eventError={eventQuery.error ?? null}
          selectedIndex={selectedIndex}
          canManage={canManage}
          statusError={statusError}
          onSelectEvent={setSelectedEventId}
          onStatus={(s) => statusMutation.mutate(s)}
          statusBusy={statusMutation.isPending}
          onOpenJson={() => setJsonOpen(true)}
          onViewTrace={(id) => navigate({ to: "/traces/$traceID", params: { traceID: id } })}
          onPlayReplay={(id) => navigate({ to: "/replays/$sessionId", params: { sessionId: id } })}
        />
      )}

      <EventJsonDrawer event={eventQuery.data ?? null} open={jsonOpen} onOpenChange={setJsonOpen} />
    </PageContainer>
  );
}

function IssueDetailBody({
  issue,
  event,
  eventLoading,
  eventError,
  selectedIndex,
  canManage,
  statusError,
  onSelectEvent,
  onStatus,
  statusBusy,
  onOpenJson,
  onViewTrace,
  onPlayReplay,
}: {
  issue: IssueDetail;
  event: ErrorEvent | undefined;
  eventLoading: boolean;
  eventError: ApiError | null;
  selectedIndex: number;
  canManage: boolean;
  statusError: string | null;
  onSelectEvent: (id: string) => void;
  onStatus: (next: IssueStatus) => void;
  statusBusy: boolean;
  onOpenJson: () => void;
  onViewTrace: (traceId: string) => void;
  onPlayReplay: (sessionId: string) => void;
}) {
  const { t, i18n } = useTranslation();
  const events = issue.events;
  const total = events.length;

  function step(delta: number) {
    const next = selectedIndex + delta;
    if (next >= 0 && next < total) onSelectEvent(events[next].event_id);
  }

  const traceId = event?.contexts.trace?.trace_id;
  const sessionId = event?.session_id;

  return (
    <div className="space-y-4">
      {/* Header */}
      <div className="space-y-3">
        <div className="flex flex-wrap items-start justify-between gap-3">
          <div className="min-w-0 space-y-1.5">
            <div className="flex items-center gap-2">
              <IssueLevelBadge level={issue.level} />
              <h1 className="min-w-0 break-words text-xl font-semibold tracking-tight">
                {issue.title}
              </h1>
            </div>
            <p className="font-mono text-xs text-muted-foreground">{issue.culprit}</p>
          </div>
          {canManage && (
            <div className="flex shrink-0 gap-2">
              {issue.status !== "resolved" && (
                <Button size="sm" variant="outline" disabled={statusBusy} onClick={() => onStatus("resolved")}>
                  <CheckCircle2 className="size-4" />
                  {t("issues.resolve")}
                </Button>
              )}
              {issue.status !== "ignored" && (
                <Button size="sm" variant="outline" disabled={statusBusy} onClick={() => onStatus("ignored")}>
                  <VolumeX className="size-4" />
                  {t("issues.ignore")}
                </Button>
              )}
              {issue.status !== "open" && (
                <Button size="sm" variant="outline" disabled={statusBusy} onClick={() => onStatus("open")}>
                  <RotateCcw className="size-4" />
                  {t("issues.reopen")}
                </Button>
              )}
            </div>
          )}
        </div>

        {statusError && (
          <div
            className="flex items-center gap-2 rounded-md border border-destructive/40 bg-destructive/10 px-3 py-2 text-sm text-destructive"
            role="alert"
          >
            <AlertCircle className="size-4 shrink-0" />
            <span>{statusError}</span>
          </div>
        )}

        {/* Count cards */}
        <div className="grid grid-cols-2 gap-3 sm:grid-cols-4">
          <StatCard label={t("issues.countCard")} value={issue.count.toLocaleString()} />
          <StatCard label={t("issues.usersCard")} value={issue.user_count.toLocaleString()} />
          <StatCard label={t("issues.firstSeenCard")} value={formatRelativeTime(issue.first_seen, i18n.language)} />
          <StatCard label={t("issues.lastSeenCard")} value={formatRelativeTime(issue.last_seen, i18n.language)} />
        </div>
      </div>

      {/* AI root-cause analysis (minimal-footprint card) */}
      <IssueAiAnalysisCard issueId={issue.id} />

      {/* Event switcher */}
      <Card>
        <CardContent className="flex flex-wrap items-center justify-between gap-3 p-3">
          <div className="flex items-center gap-2 text-sm">
            <Button
              size="icon"
              variant="outline"
              className="size-8"
              disabled={selectedIndex <= 0}
              onClick={() => step(-1)}
              aria-label={t("issues.prevEvent")}
            >
              <ChevronLeft className="size-4" />
            </Button>
            <span className="tabular-nums text-muted-foreground">
              {t("issues.eventOf", { index: selectedIndex + 1, total })}
            </span>
            <Button
              size="icon"
              variant="outline"
              className="size-8"
              disabled={selectedIndex >= total - 1}
              onClick={() => step(1)}
              aria-label={t("issues.nextEvent")}
            >
              <ChevronRight className="size-4" />
            </Button>
            {event && (
              <span className="ml-2 font-mono text-xs text-muted-foreground">
                {formatDateTime(event.timestamp)}
              </span>
            )}
          </div>
          <div className="flex flex-wrap gap-2">
            {traceId && (
              <Button size="sm" variant="outline" onClick={() => onViewTrace(traceId)}>
                <Waypoints className="size-4" />
                {t("issues.viewTrace")}
              </Button>
            )}
            {sessionId && (
              <Button size="sm" variant="outline" onClick={() => onPlayReplay(sessionId)}>
                <PlayCircle className="size-4" />
                {t("issues.playReplay")}
              </Button>
            )}
            <Button size="sm" variant="outline" onClick={onOpenJson} disabled={!event}>
              <Braces className="size-4" />
              {t("issues.viewRawJson")}
            </Button>
          </div>
        </CardContent>
      </Card>

      {/* Event body */}
      {eventLoading ? (
        <Card>
          <CardContent className="space-y-3 p-4">
            <div className="flex items-center gap-2 text-sm text-muted-foreground">
              <Loader2 className="size-4 animate-spin" />
              {t("issues.loadingEvent")}
            </div>
            <Skeleton className="h-48 w-full" />
          </CardContent>
        </Card>
      ) : eventError ? (
        <Card>
          <CardContent className="py-12">
            <div className="flex flex-col items-center gap-2 text-center">
              <AlertCircle className="size-6 text-destructive" />
              <div className="text-sm font-medium">{t("issues.eventLoadError")}</div>
              <p className="text-sm text-muted-foreground">{eventError.message}</p>
            </div>
          </CardContent>
        </Card>
      ) : event ? (
        <div className="grid grid-cols-1 gap-4 lg:grid-cols-3">
          <Card className="lg:col-span-2">
            <CardContent className="p-4">
              <div className="mb-3">
                <span className="font-mono text-sm font-semibold text-foreground">
                  {event.exception.type}
                </span>
                <span className="ml-2 font-mono text-sm text-muted-foreground">
                  {event.exception.value}
                </span>
              </div>
              <StacktraceViewer
                symbolicated={event.exception.stacktrace}
                raw={event.exception.raw_stacktrace}
              />
            </CardContent>
          </Card>

          <Card>
            <CardContent className="space-y-4 p-4">
              <h3 className="text-sm font-semibold">{t("issues.contextSection")}</h3>
              <ContextRow label={t("issues.contextBrowser")} value={
                event.contexts.browser
                  ? `${event.contexts.browser.name} ${event.contexts.browser.version}`
                  : undefined
              } />
              <ContextRow label={t("issues.contextOs")} value={
                event.contexts.os ? `${event.contexts.os.name} ${event.contexts.os.version}` : undefined
              } />
              <ContextRow label={t("issues.contextUrl")} value={event.url} mono />
              <ContextRow label={t("issues.contextUser")} value={event.user?.id} mono />

              <div className="space-y-1.5">
                <div className="text-xs text-muted-foreground">{t("issues.contextTags")}</div>
                <div className="flex flex-wrap gap-1.5">
                  {Object.entries(event.tags).map(([k, v]) => (
                    <span
                      key={k}
                      className="inline-flex items-center gap-1 rounded border border-border bg-muted/40 px-1.5 py-0.5 font-mono text-[11px]"
                    >
                      <span className="text-muted-foreground">{k}</span>
                      <span className="text-foreground">{v}</span>
                    </span>
                  ))}
                </div>
              </div>
            </CardContent>
          </Card>
        </div>
      ) : (
        <Card>
          <CardContent className="py-12 text-center text-sm text-muted-foreground">
            {t("issues.noEvents")}
          </CardContent>
        </Card>
      )}
    </div>
  );
}

function StatCard({ label, value }: { label: string; value: string }) {
  return (
    <div className="rounded-lg border border-border p-3">
      <div className="text-xs text-muted-foreground">{label}</div>
      <div className="mt-0.5 text-lg font-semibold tabular-nums">{value}</div>
    </div>
  );
}

function ContextRow({ label, value, mono }: { label: string; value?: string; mono?: boolean }) {
  if (!value) return null;
  return (
    <div className="space-y-0.5">
      <div className="text-xs text-muted-foreground">{label}</div>
      <div className={mono ? "break-all font-mono text-xs text-foreground" : "text-sm text-foreground"}>
        {value}
      </div>
    </div>
  );
}
