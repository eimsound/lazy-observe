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
import { canManageAlerts } from "@/lib/auth/permissions";
import { useProject } from "@/lib/project/project-provider";
import { isForbiddenError } from "@/lib/api/errors";
import { QueryErrorState } from "@/components/common/query-error-state";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Switch } from "@/components/ui/switch";
import { Skeleton } from "@/components/ui/skeleton";
import { cn } from "@/lib/utils";

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
        </CardTitle>
        <CardDescription>
          {current
            ? t("alertConfig.subtitle", { project: current.name })
            : t("alertConfig.noProject")}
        </CardDescription>
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
  const canManage = canManageAlerts(role);
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
    enabled: canManage,
    retry: false,
  });

  const [draft, setDraft] = useState<AlertConfig | null>(null);
  const [dirty, setDirty] = useState(false);
  const [saved, setSaved] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const savedTimerRef = useRef<ReturnType<typeof setTimeout> | null>(null);

  useEffect(() => {
    return () => {
      if (savedTimerRef.current !== null) {
        clearTimeout(savedTimerRef.current);
      }
    };
  }, []);

  // Seed the editable draft from server data unless the user has unsaved edits.
  useEffect(() => {
    if (config.data && !dirty) {
      setDraft(config.data);
    }
  }, [config.data, dirty]);

  const save = useMutation({
    mutationFn: (next: AlertConfig) => putAlertConfig(projectId, next),
    onSuccess: (next) => {
      queryClient.setQueryData(["alert-config", projectId], next);
      setDraft(next);
      setDirty(false);
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
    setDraft((prev) => (prev ? { ...prev, ...p } : prev));
    setDirty(true);
    setSaved(false);
  }

  function toggleChannel(id: string) {
    if (!canManage) return;
    setDraft((prev) => {
      if (!prev) return prev;
      const has = prev.channel_ids.includes(id);
      return {
        ...prev,
        channel_ids: has
          ? prev.channel_ids.filter((c) => c !== id)
          : [...prev.channel_ids, id],
      };
    });
    setDirty(true);
    setSaved(false);
  }

  if (config.isLoading) {
    return (
      <div className="space-y-3">
        <Skeleton className="h-10 w-full" />
        <Skeleton className="h-10 w-full" />
        <Skeleton className="h-10 w-64" />
      </div>
    );
  }

  if (config.isError || !draft) {
    return (
      <QueryErrorState
        error={config.error}
        onRetry={() => void config.refetch()}
      />
    );
  }

  return (
    <>
      <div className="flex items-center justify-between rounded-lg border border-border px-3 py-2.5">
        <div>
          <Label htmlFor="ac-new-issue" className={canManage ? "cursor-pointer" : undefined}>
            {t("alertConfig.newIssue")}
          </Label>
          <p className="text-xs text-muted-foreground">{t("alertConfig.newIssueHint")}</p>
        </div>
        <Switch
          id="ac-new-issue"
          checked={draft.new_issue}
          disabled={!canManage}
          onCheckedChange={(v) => patch({ new_issue: v })}
        />
      </div>

      <div className="flex items-center justify-between rounded-lg border border-border px-3 py-2.5">
        <div>
          <Label htmlFor="ac-regression" className={canManage ? "cursor-pointer" : undefined}>
            {t("alertConfig.regression")}
          </Label>
          <p className="text-xs text-muted-foreground">{t("alertConfig.regressionHint")}</p>
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
        {!canManage ? (
          <p className="text-xs text-muted-foreground">
            {t("alertConfig.channelsReadOnlySummary", { count: draft.channel_ids.length })}
          </p>
        ) : channels.isLoading ? (
          <Skeleton className="h-8 w-48" />
        ) : channels.isError ? (
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
        ) : channels.data && channels.data.length > 0 ? (
          <div className="flex flex-wrap gap-2">
            {channels.data.map((ch) => {
              const selected = draft.channel_ids.includes(ch.id);
              return (
                <button
                  key={ch.id}
                  type="button"
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
        ) : (
          <p className="text-xs text-muted-foreground">
            {t("alertConfig.noChannels")}
          </p>
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
        <p className="text-xs text-muted-foreground">{t("alertConfig.readOnlyHint")}</p>
      )}
    </>
  );
}
