import { useEffect, useRef, useState } from "react";
import { useTranslation } from "react-i18next";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { BellRing, Check, Loader2 } from "lucide-react";
import {
  getAlertConfig,
  putAlertConfig,
  type AlertConfig,
} from "@/lib/api/alert-config";
import { listChannels } from "@/lib/api/alerts";
import { useAuth } from "@/lib/auth/auth-provider";
import { hasCapability } from "@/lib/auth/permissions";
import { useProject } from "@/lib/project/project-provider";
import { isForbiddenError } from "@/lib/api/errors";
import { HelpTip } from "@/components/common/help-tip";
import { QueryErrorState } from "@/components/common/query-error-state";
import { QueryStaleNotice } from "@/components/common/query-stale-notice";
import {
  Card,
  CardContent,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Switch } from "@/components/ui/switch";
import { Skeleton } from "@/components/ui/skeleton";
import {
  Tooltip,
  TooltipContent,
  TooltipTrigger,
} from "@/components/ui/tooltip";
import { cn } from "@/lib/utils";
import { queryViewState } from "@/routes/query-view-state";

/**
 * Per-project alerting policy, embedded in the Settings page. Keyed by
 * `projectId` so switching projects remounts a clean form and can never carry
 * a prior project's draft into a newly selected project if loading fails.
 */
export function AlertConfigCard() {
  const { t } = useTranslation();
  const { current } = useProject();
  const projectId = current?.id ?? "";

  return (
    <Card>
      <CardHeader>
        <CardTitle className="flex items-center gap-2">
          <BellRing className="size-4" />
          {t("alertConfig.title")}
          {current && (
            <Badge variant="secondary" className="font-normal">
              {current.name}
            </Badge>
          )}
        </CardTitle>
      </CardHeader>
      <CardContent className="space-y-4">
        {!projectId ? (
          <p className="text-sm text-muted-foreground">{t("alertConfig.noProject")}</p>
        ) : (
          <AlertConfigForm key={projectId} projectId={projectId} />
        )}
      </CardContent>
    </Card>
  );
}

function AlertConfigForm({ projectId }: { projectId: string }) {
  const { t } = useTranslation();
  const { role } = useAuth();
  const canManage = hasCapability(role, "alerts:write");
  const canReadChannels = hasCapability(role, "channels:read");
  const queryClient = useQueryClient();

  const config = useQuery({
    queryKey: ["alert-config", projectId],
    queryFn: ({ signal }) => getAlertConfig(projectId, signal),
    enabled: !!projectId,
    refetchOnWindowFocus: false,
    retry: false,
  });
  const channels = useQuery({
    queryKey: ["channels"],
    queryFn: ({ signal }) => listChannels(signal),
    enabled: canReadChannels,
    refetchOnWindowFocus: false,
    retry: false,
  });

  const [editedDraft, setEditedDraft] = useState<AlertConfig | null>(null);
  const [saved, setSaved] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const savedTimerRef = useRef<ReturnType<typeof setTimeout> | null>(null);

  // Derive the effective form value from server data unless the user has unsaved edits.
  const draft = editedDraft ?? config.data ?? null;

  useEffect(() => {
    return () => {
      if (savedTimerRef.current !== null) {
        clearTimeout(savedTimerRef.current);
      }
    };
  }, []);

  const save = useMutation({
    mutationFn: (next: AlertConfig) => putAlertConfig(projectId, next),
    onSuccess: (next) => {
      queryClient.setQueryData(["alert-config", projectId], next);
      setEditedDraft(null);
      setSaved(true);
      setError(null);
      if (savedTimerRef.current !== null) {
        clearTimeout(savedTimerRef.current);
      }
      savedTimerRef.current = setTimeout(() => {
        savedTimerRef.current = null;
        setSaved(false);
      }, 1500);
    },
    onError: (e) => setError((e as Error).message || t("alertConfig.saveError")),
  });

  function patch(p: Partial<AlertConfig>) {
    if (!canManage) return;
    setEditedDraft((prev) => {
      const base = prev ?? config.data ?? null;
      return base ? { ...base, ...p } : null;
    });
    setSaved(false);
  }

  function toggleChannel(id: string) {
    if (!canManage) return;
    setEditedDraft((prev) => {
      const base = prev ?? config.data ?? null;
      if (!base) return null;
      const has = base.channel_ids.includes(id);
      return {
        ...base,
        channel_ids: has
          ? base.channel_ids.filter((c) => c !== id)
          : [...base.channel_ids, id],
      };
    });
    setSaved(false);
  }

  const configState = queryViewState({
    hasData: draft !== null,
    isLoading: config.isLoading,
    isError: config.isError,
    isEmpty: false,
  });

  if (configState === "pending") {
    return (
      <div className="space-y-3">
        <Skeleton className="h-10 w-full" />
        <Skeleton className="h-10 w-full" />
        <Skeleton className="h-10 w-64" />
      </div>
    );
  }

  if (configState === "error" || !draft) {
    return (
      <QueryErrorState
        error={config.error}
        onRetry={() => void config.refetch()}
      />
    );
  }

  const channelRows = channels.data ?? [];
  const channelsState = queryViewState({
    hasData: channels.data !== undefined,
    isLoading: channels.isLoading,
    isError: channels.isError,
    isEmpty: channelRows.length === 0,
  });

  return (
    <>
      {configState === "stale" && (
        <QueryStaleNotice
          isRetrying={config.isFetching}
          onRetry={() => void config.refetch()}
        />
      )}

      <div className="flex items-center justify-between rounded-lg border border-border px-3 py-2.5">
        <Label htmlFor="ac-new-issue" className={canManage ? "cursor-pointer" : undefined}>
          {t("alertConfig.newIssue")}
        </Label>
        <Switch
          id="ac-new-issue"
          checked={draft.new_issue}
          disabled={!canManage}
          onCheckedChange={(v) => patch({ new_issue: v })}
        />
      </div>

      <div className="flex items-center justify-between rounded-lg border border-border px-3 py-2.5">
        <div className="flex items-center gap-1.5">
          <Label htmlFor="ac-regression" className={canManage ? "cursor-pointer" : undefined}>
            {t("alertConfig.regression")}
          </Label>
          <HelpTip content={t("alertConfig.regressionHint")} />
        </div>
        <Switch
          id="ac-regression"
          checked={draft.regression}
          disabled={!canManage}
          onCheckedChange={(v) => patch({ regression: v })}
        />
      </div>

      <div className="space-y-1.5">
        <Label htmlFor="ac-threshold">{t("alertConfig.errorRateThreshold")}</Label>
        <div className="flex items-center gap-2">
          <Input
            id="ac-threshold"
            type="number"
            min={0}
            value={draft.error_rate_threshold_per_min}
            disabled={!canManage}
            onChange={(e) =>
              patch({ error_rate_threshold_per_min: Math.max(0, Number(e.target.value) || 0) })
            }
            className="w-32 font-mono text-sm"
          />
          <span className="text-sm text-muted-foreground">
            {t("alertConfig.perMinute")}
          </span>
        </div>
      </div>

      <div className="space-y-1.5">
        <Label>{t("alertConfig.channels")}</Label>
        {!canReadChannels ? (
          <p className="text-xs text-muted-foreground">
            {t("alertConfig.channelsReadOnlySummary", { count: draft.channel_ids.length })}
          </p>
        ) : (
          <>
            {channelsState === "stale" && (
              <QueryStaleNotice
                isRetrying={channels.isFetching}
                onRetry={() => void channels.refetch()}
              />
            )}
            {channelsState === "pending" ? (
              <Skeleton className="h-8 w-48" />
            ) : channelsState === "error" ? (
              <div className="flex flex-wrap items-center gap-2 text-xs text-destructive" role="alert">
                <span>
                  {isForbiddenError(channels.error)
                    ? t("alertConfig.channelsForbidden")
                    : t("alerts.channelsLoadError")}
                </span>
                {!isForbiddenError(channels.error) && (
                  <Button
                    type="button"
                    variant="outline"
                    size="sm"
                    className="h-6 px-2 text-xs"
                    onClick={() => void channels.refetch()}
                  >
                    {t("common.retry")}
                  </Button>
                )}
              </div>
            ) : channelsState === "empty" ? (
              <p className="text-xs text-muted-foreground">
                {t("alertConfig.noChannels")}
              </p>
            ) : (
              <div className="flex flex-wrap gap-2">
                {channelRows.map((ch) => {
                  const selected = draft.channel_ids.includes(ch.id);
                  return (
                    <button
                      key={ch.id}
                      type="button"
                      disabled={!canManage}
                      onClick={() => toggleChannel(ch.id)}
                      className={cn(
                        "rounded-md border px-2.5 py-1 text-xs font-medium transition-colors",
                        selected
                          ? "border-primary bg-primary/10 text-foreground"
                          : "border-border text-muted-foreground hover:bg-accent",
                      )}
                    >
                      {ch.name}
                    </button>
                  );
                })}
              </div>
            )}
          </>
        )}
      </div>

      {error && (
        <p className="text-sm text-destructive" role="alert">
          {error}
        </p>
      )}

      {canManage ? (
        <Button onClick={() => save.mutate(draft)} disabled={save.isPending || saved}>
          {save.isPending ? (
            <Loader2 className="size-4 animate-spin" />
          ) : saved ? (
            <Check className="size-4" />
          ) : null}
          {saved ? t("common.saved") : t("common.save")}
        </Button>
      ) : (
        <Tooltip>
          <TooltipTrigger asChild>
            <span className="inline-flex cursor-not-allowed">
              <Button disabled>{t("common.save")}</Button>
            </span>
          </TooltipTrigger>
          <TooltipContent>{t("alertConfig.readOnlyHint")}</TooltipContent>
        </Tooltip>
      )}
    </>
  );
}
