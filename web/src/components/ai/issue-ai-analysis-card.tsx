import { useTranslation } from "react-i18next";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { AlertCircle, Loader2, RefreshCw, Sparkles } from "lucide-react";
import {
  getAiStatus,
  getIssueAiAnalysis,
  generateIssueAiAnalysis,
  type IssueAiAnalysis,
} from "@/lib/api/ai";
import { ApiError } from "@/lib/api/client";
import { useAuth } from "@/lib/auth/auth-provider";
import { canUseAi } from "@/lib/auth/permissions";
import { useProject } from "@/lib/project/project-provider";
import { projectQueryKey } from "@/lib/project/project-scope";
import { useProjectQuery } from "@/lib/project/use-project-query";
import { formatRelativeTime } from "@/lib/format";
import { ConfidenceBadge } from "@/components/ai/confidence-badge";
import { Card, CardContent } from "@/components/ui/card";
import { Button } from "@/components/ui/button";
import { Skeleton } from "@/components/ui/skeleton";
import {
  Tooltip,
  TooltipContent,
  TooltipTrigger,
} from "@/components/ui/tooltip";

/**
 * Minimal-footprint AI analysis card for the Issue detail page. When no analysis
 * exists it offers a generate action (member+; disabled with a tooltip when AI
 * is off); once generated it renders the summary, confidence-scored probable
 * causes and suggested actions, and surfaces a stale->refresh flow.
 */
export function IssueAiAnalysisCard({ issueId }: { issueId: string }) {
  const { t } = useTranslation();
  const { role } = useAuth();
  const { currentSlug: project } = useProject();
  const queryClient = useQueryClient();

  const statusQuery = useQuery({
    queryKey: ["ai-status"],
    queryFn: ({ signal }) => getAiStatus(signal),
    staleTime: 10_000,
  });

  const analysisQuery = useProjectQuery<IssueAiAnalysis | null, ApiError>({
    queryKey: ["issue-ai", issueId],
    queryFn: ({ signal, project: activeProject }) =>
      getIssueAiAnalysis(issueId, signal, activeProject),
    retry: false,
  });

  const generate = useMutation({
    mutationFn: (refresh: boolean) => generateIssueAiAnalysis(issueId, refresh, project),
    onSuccess: (data) => queryClient.setQueryData(projectQueryKey(project, ["issue-ai", issueId]), data),
  });

  const aiEnabled = statusQuery.data?.enabled ?? true;
  const allowed = canUseAi(role);
  const analysis = analysisQuery.data;

  return (
    <Card>
      <CardContent className="space-y-4 p-4">
        <div className="flex items-start justify-between gap-3">
          <div className="flex items-center gap-2">
            <div className="flex size-7 items-center justify-center rounded-md bg-primary/10 text-primary">
              <Sparkles className="size-4" />
            </div>
            <div>
              <h3 className="text-sm font-semibold">{t("aiAnalysis.title")}</h3>
              <p className="text-xs text-muted-foreground">{t("aiAnalysis.subtitle")}</p>
            </div>
          </div>
          {analysis && (
            <RefreshButton
              busy={generate.isPending}
              disabled={!aiEnabled || !allowed}
              onClick={() => generate.mutate(true)}
              enabledHint={aiEnabled ? (allowed ? undefined : t("aiAnalysis.permissionHint")) : t("aiAnalysis.disabledHint")}
            />
          )}
        </div>

        {/* Loading existing analysis */}
        {analysisQuery.isLoading ? (
          <div className="space-y-2">
            <Skeleton className="h-4 w-2/3" />
            <Skeleton className="h-16 w-full" />
          </div>
        ) : generate.isPending && !analysis ? (
          <div className="flex items-center gap-2 py-6 text-sm text-muted-foreground">
            <Loader2 className="size-4 animate-spin" />
            {t("aiAnalysis.generating")}
          </div>
        ) : !analysis ? (
          <NotGenerated
            aiEnabled={aiEnabled}
            allowed={allowed}
            busy={generate.isPending}
            error={generate.error}
            onGenerate={() => generate.mutate(false)}
          />
        ) : (
          <AnalysisBody analysis={analysis} stale={analysis.stale} />
        )}

        {/* Generation error (when an analysis already exists but refresh failed) */}
        {analysis && generate.isError && (
          <div className="flex items-center gap-2 text-xs text-destructive">
            <AlertCircle className="size-3.5" />
            {generate.error instanceof ApiError ? generate.error.message : t("aiAnalysis.error")}
          </div>
        )}
      </CardContent>
    </Card>
  );
}

function NotGenerated({
  aiEnabled,
  allowed,
  busy,
  error,
  onGenerate,
}: {
  aiEnabled: boolean;
  allowed: boolean;
  busy: boolean;
  error: unknown;
  onGenerate: () => void;
}) {
  const { t } = useTranslation();
  const disabled = !aiEnabled || !allowed || busy;
  const hint = !aiEnabled
    ? t("aiAnalysis.disabledHint")
    : !allowed
      ? t("aiAnalysis.permissionHint")
      : undefined;

  const button = (
    <Button size="sm" disabled={disabled} onClick={onGenerate}>
      {busy ? <Loader2 className="size-4 animate-spin" /> : <Sparkles className="size-4" />}
      {busy ? t("aiAnalysis.generating") : t("aiAnalysis.generate")}
    </Button>
  );

  return (
    <div className="flex flex-col items-center gap-3 py-6 text-center">
      <p className="max-w-sm text-sm text-muted-foreground">{t("aiAnalysis.emptyDesc")}</p>
      {hint ? (
        <Tooltip>
          <TooltipTrigger asChild>
            <span tabIndex={0}>{button}</span>
          </TooltipTrigger>
          <TooltipContent>{hint}</TooltipContent>
        </Tooltip>
      ) : (
        button
      )}
      {error instanceof ApiError && (
        <span className="flex items-center gap-1.5 text-xs text-destructive">
          <AlertCircle className="size-3.5" />
          {error.message}
        </span>
      )}
    </div>
  );
}

function RefreshButton({
  busy,
  disabled,
  onClick,
  enabledHint,
}: {
  busy: boolean;
  disabled: boolean;
  onClick: () => void;
  enabledHint?: string;
}) {
  const { t } = useTranslation();
  const button = (
    <Button variant="outline" size="sm" disabled={disabled || busy} onClick={onClick}>
      {busy ? <Loader2 className="size-4 animate-spin" /> : <RefreshCw className="size-4" />}
      {t("aiAnalysis.refresh")}
    </Button>
  );
  if (disabled && enabledHint) {
    return (
      <Tooltip>
        <TooltipTrigger asChild>
          <span tabIndex={0}>{button}</span>
        </TooltipTrigger>
        <TooltipContent>{enabledHint}</TooltipContent>
      </Tooltip>
    );
  }
  return button;
}

function AnalysisBody({ analysis, stale }: { analysis: IssueAiAnalysis; stale: boolean }) {
  const { t, i18n } = useTranslation();
  return (
    <div className="space-y-4">
      {stale && (
        <div className="flex items-start gap-2 rounded-md border border-vital-ni/30 bg-vital-ni/10 px-3 py-2 text-sm">
          <AlertCircle className="mt-0.5 size-4 shrink-0 text-vital-ni" />
          <div className="min-w-0">
            <div className="font-medium text-vital-ni">{t("aiAnalysis.staleTitle")}</div>
            <p className="text-xs text-muted-foreground">{t("aiAnalysis.staleDesc")}</p>
          </div>
        </div>
      )}

      {/* Summary */}
      <div className="space-y-1">
        <div className="text-xs font-medium uppercase tracking-wide text-muted-foreground">
          {t("aiAnalysis.summary")}
        </div>
        <p className="text-sm leading-relaxed text-foreground/90">{analysis.summary}</p>
      </div>

      {/* Probable causes */}
      <div className="space-y-2">
        <div className="text-xs font-medium uppercase tracking-wide text-muted-foreground">
          {t("aiAnalysis.probableCauses")}
        </div>
        <ul className="space-y-2">
          {analysis.probable_causes.map((c, i) => (
            <li key={i} className="rounded-md border border-border p-3">
              <div className="flex items-start justify-between gap-2">
                <span className="min-w-0 text-sm font-medium text-foreground">{c.cause}</span>
                <ConfidenceBadge confidence={c.confidence} />
              </div>
              <p className="mt-1 text-xs text-muted-foreground">
                <span className="font-medium">{t("aiAnalysis.evidence")}：</span>
                {c.evidence}
              </p>
            </li>
          ))}
        </ul>
      </div>

      {/* Suggested actions */}
      <div className="space-y-2">
        <div className="text-xs font-medium uppercase tracking-wide text-muted-foreground">
          {t("aiAnalysis.suggestedActions")}
        </div>
        <ul className="space-y-1.5">
          {analysis.suggested_actions.map((a, i) => (
            <li key={i} className="flex items-start gap-2 text-sm text-foreground/90">
              <span className="mt-1.5 size-1.5 shrink-0 rounded-full bg-primary" aria-hidden />
              <span className="min-w-0">{a}</span>
            </li>
          ))}
        </ul>
      </div>

      <div className="text-xs text-muted-foreground">
        {t("aiAnalysis.analyzedAt", { time: formatRelativeTime(analysis.ai_analysis_at, i18n.language) })}
      </div>
    </div>
  );
}
