import { useEffect, useState } from "react";
import { useTranslation } from "react-i18next";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { BellRing, Check, Loader2 } from "lucide-react";
import {
  getAlertConfig,
  putAlertConfig,
  type AlertConfig,
} from "@/lib/api/alert-config";
import { listChannels } from "@/lib/api/alerts";
import { useProject } from "@/lib/project/project-provider";
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
 * Per-project alerting policy, embedded in the Settings page. Reads/writes the
 * project alert-config contract; channel selection reuses the F2 channel data
 * source. Scoped to the currently selected project (see project-provider).
 */
export function AlertConfigCard() {
  const { t } = useTranslation();
  const { current } = useProject();
  const projectId = current?.id ?? "";
  const queryClient = useQueryClient();

  const config = useQuery({
    queryKey: ["alert-config", projectId],
    queryFn: ({ signal }) => getAlertConfig(projectId, signal),
    enabled: !!projectId,
  });
  const channels = useQuery({
    queryKey: ["channels"],
    queryFn: ({ signal }) => listChannels(signal),
  });

  const [draft, setDraft] = useState<AlertConfig | null>(null);
  const [saved, setSaved] = useState(false);
  const [error, setError] = useState<string | null>(null);

  // Seed the editable draft once the config loads (or the project changes).
  useEffect(() => {
    if (config.data) setDraft(config.data);
  }, [config.data]);

  const save = useMutation({
    mutationFn: (next: AlertConfig) => putAlertConfig(projectId, next),
    onSuccess: (next) => {
      queryClient.setQueryData(["alert-config", projectId], next);
      setSaved(true);
      setError(null);
      setTimeout(() => setSaved(false), 1500);
    },
    onError: (e) => setError((e as Error).message || t("alertConfig.saveError")),
  });

  function patch(p: Partial<AlertConfig>) {
    setDraft((prev) => (prev ? { ...prev, ...p } : prev));
    setSaved(false);
  }

  function toggleChannel(id: string) {
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
    setSaved(false);
  }

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
        ) : config.isLoading || !draft ? (
          <div className="space-y-3">
            <Skeleton className="h-10 w-full" />
            <Skeleton className="h-10 w-full" />
            <Skeleton className="h-10 w-64" />
          </div>
        ) : (
          <>
            <div className="flex items-center justify-between rounded-lg border border-border px-3 py-2.5">
              <div>
                <Label htmlFor="ac-new-issue" className="cursor-pointer">
                  {t("alertConfig.newIssue")}
                </Label>
                <p className="text-xs text-muted-foreground">{t("alertConfig.newIssueHint")}</p>
              </div>
              <Switch
                id="ac-new-issue"
                checked={draft.new_issue}
                onCheckedChange={(v) => patch({ new_issue: v })}
              />
            </div>

            <div className="flex items-center justify-between rounded-lg border border-border px-3 py-2.5">
              <div>
                <Label htmlFor="ac-regression" className="cursor-pointer">
                  {t("alertConfig.regression")}
                </Label>
                <p className="text-xs text-muted-foreground">{t("alertConfig.regressionHint")}</p>
              </div>
              <Switch
                id="ac-regression"
                checked={draft.regression}
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
              {channels.data && channels.data.length > 0 ? (
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

            <Button onClick={() => draft && save.mutate(draft)} disabled={save.isPending || saved}>
              {save.isPending ? (
                <Loader2 className="size-4 animate-spin" />
              ) : saved ? (
                <Check className="size-4" />
              ) : null}
              {saved ? t("common.saved") : t("common.save")}
            </Button>
          </>
        )}
      </CardContent>
    </Card>
  );
}
