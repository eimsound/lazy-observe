import { useEffect, useMemo, useState } from "react";
import { useTranslation } from "react-i18next";
import { useNavigate, useParams } from "@tanstack/react-router";
import { AlertCircle, ArrowLeft, BarChart3, Flame, Loader2 } from "lucide-react";
import {
  getTrace,
  summarizeTrace,
  type JaegerSpan,
  type JaegerTrace,
} from "@/lib/api/traces";
import { buildTree, maxDepth } from "@/lib/trace-tree";
import { ApiError } from "@/lib/api/client";
import { useProject } from "@/lib/project/project-provider";
import { useProjectQuery } from "@/lib/project/use-project-query";
import { formatDurationMicros } from "@/lib/format";
import { PageContainer } from "@/components/layout/page";
import { TraceWaterfall } from "@/components/traces/trace-waterfall";
import { SpanDetailDrawer } from "@/components/traces/span-detail-drawer";
import { Button } from "@/components/ui/button";
import { Badge } from "@/components/ui/badge";
import { Card, CardContent } from "@/components/ui/card";
import { Skeleton } from "@/components/ui/skeleton";
import { buildTraceEvidenceLinks } from "./trace-evidence-links";

export function TraceDetailPage() {
  const { t } = useTranslation();
  const navigate = useNavigate();
  const { traceID } = useParams({ strict: false }) as { traceID: string };
  const { currentSlug: project } = useProject();

  const [selected, setSelected] = useState<JaegerSpan | null>(null);
  const [drawerOpen, setDrawerOpen] = useState(false);

  useEffect(() => {
    setSelected(null);
    setDrawerOpen(false);
  }, [project, traceID]);

  const traceQuery = useProjectQuery<JaegerTrace, ApiError>({
    queryKey: ["trace", traceID],
    queryFn: ({ signal, project: activeProject }) => getTrace(traceID, signal, activeProject),
    retry: false,
  });

  function selectSpan(span: JaegerSpan) {
    setSelected(span);
    setDrawerOpen(true);
  }

  function viewLogs(trace: JaegerTrace) {
    const link = buildTraceEvidenceLinks(trace).logs;
    void navigate({ to: link.to, search: link.search });
  }

  function viewProfiling(trace: JaegerTrace) {
    const link = buildTraceEvidenceLinks(trace).profiling;
    void navigate({ to: link.to, search: link.search });
  }

  function viewMetrics(trace: JaegerTrace) {
    const link = buildTraceEvidenceLinks(trace).metrics;
    void navigate({ to: link.to, search: link.search });
  }

  return (
    <PageContainer>
      <div className="mb-4 flex items-center gap-3">
        <Button variant="ghost" size="sm" onClick={() => navigate({ to: "/traces" })}>
          <ArrowLeft className="size-4" />
          {t("traces.backToSearch")}
        </Button>
      </div>

      {traceQuery.isLoading ? (
        <Card>
          <CardContent className="space-y-3 p-4">
            <div className="flex items-center gap-2 text-sm text-muted-foreground">
              <Loader2 className="size-4 animate-spin" />
              {t("traces.loadingTrace")}
            </div>
            <Skeleton className="h-40 w-full" />
          </CardContent>
        </Card>
      ) : traceQuery.error || !traceQuery.data ? (
        <Card>
          <CardContent className="flex flex-col items-center justify-center gap-3 py-24 text-center">
            <div className="flex size-12 items-center justify-center rounded-full bg-muted text-destructive">
              <AlertCircle className="size-6" />
            </div>
            <div className="text-base font-medium">{t("traces.notFoundTitle")}</div>
            <p className="max-w-md text-sm text-muted-foreground">
              {traceQuery.error?.message ?? t("traces.notFoundDesc")}
            </p>
            <Button variant="outline" size="sm" onClick={() => navigate({ to: "/traces" })}>
              {t("traces.backToSearch")}
            </Button>
          </CardContent>
        </Card>
      ) : (
        <TraceDetailBody
          key={`${project}:${traceID}`}
          trace={traceQuery.data}
          selected={selected}
          drawerOpen={drawerOpen}
          onOpenChange={setDrawerOpen}
          onSelectSpan={selectSpan}
          onViewLogs={viewLogs}
          onViewProfiling={viewProfiling}
          onViewMetrics={viewMetrics}
        />
      )}
    </PageContainer>
  );
}

function TraceDetailBody({
  trace,
  selected,
  drawerOpen,
  onOpenChange,
  onSelectSpan,
  onViewLogs,
  onViewProfiling,
  onViewMetrics,
}: {
  trace: JaegerTrace;
  selected: JaegerSpan | null;
  drawerOpen: boolean;
  onOpenChange: (open: boolean) => void;
  onSelectSpan: (span: JaegerSpan) => void;
  onViewLogs: (trace: JaegerTrace) => void;
  onViewProfiling: (trace: JaegerTrace) => void;
  onViewMetrics: (trace: JaegerTrace) => void;
}) {
  const { t } = useTranslation();
  const summary = useMemo(() => summarizeTrace(trace), [trace]);
  const depth = useMemo(() => maxDepth(buildTree(trace)), [trace]);

  return (
    <>
      {/* Header */}
      <div className="mb-4 space-y-2">
        <div className="flex flex-wrap items-center gap-2">
          <h1 className="text-xl font-semibold tracking-tight">{summary.rootName}</h1>
          <span className="text-sm text-muted-foreground">{summary.rootService}</span>
          {summary.errorCount > 0 && (
            <Badge variant="destructive">
              {t("traces.errors", { count: summary.errorCount })}
            </Badge>
          )}
          <div className="ml-auto flex flex-wrap items-center gap-2">
            <Badge variant="secondary">{t("traces.contextualEvidence")}</Badge>
            <Button variant="outline" size="sm" onClick={() => onViewProfiling(trace)}>
              <Flame className="size-4" />
              {t("traces.viewProfiling")}
            </Button>
            <Button variant="outline" size="sm" onClick={() => onViewMetrics(trace)}>
              <BarChart3 className="size-4" />
              {t("traces.viewMetrics")}
            </Button>
          </div>
        </div>
        <p className="text-xs text-muted-foreground">{t("traces.contextualEvidenceHint")}</p>
        <div className="flex flex-wrap gap-x-6 gap-y-1 text-xs text-muted-foreground">
          <Stat label={t("traces.traceId")} value={trace.traceID} mono />
          <Stat label={t("traces.totalDuration")} value={formatDurationMicros(summary.duration)} />
          <Stat label={t("traces.spanCount")} value={String(summary.spanCount)} />
          <Stat label={t("traces.serviceCount")} value={String(summary.serviceCount)} />
          <Stat label={t("traces.depth")} value={String(depth)} />
        </div>
      </div>

      <TraceWaterfall
        trace={trace}
        selectedSpanId={selected?.spanID}
        onSelectSpan={onSelectSpan}
      />

      <SpanDetailDrawer
        span={selected}
        trace={trace}
        open={drawerOpen}
        onOpenChange={onOpenChange}
        onViewLogs={() => onViewLogs(trace)}
      />
    </>
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
