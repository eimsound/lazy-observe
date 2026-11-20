import { useMemo, useState } from "react";
import { useTranslation } from "react-i18next";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Loader2, Pencil, Plus, RefreshCw, Target, Trash2 } from "lucide-react";
import {
  listScrapeTargets,
  createScrapeTarget,
  updateScrapeTarget,
  deleteScrapeTarget,
  getTargetHealth,
  type ScrapeTarget,
  type ScrapeTargetInput,
  type TargetHealthStatus,
} from "@/lib/api/scrape-targets";
import { useAuth } from "@/lib/auth/auth-provider";
import { canManageAlerts } from "@/lib/auth/permissions";
import { formatRelativeTime } from "@/lib/format";
import dayjs from "@/lib/dayjs";
import { PageActions, PageContainer } from "@/components/layout/page";
import { EmptyState } from "@/components/common/empty-state";
import { formatErrorMessage } from "@/lib/api/errors";
import { QueryErrorState } from "@/components/common/query-error-state";
import { ConfirmDialog } from "@/components/common/confirm-dialog";
import { KvEditor } from "@/components/common/kv-editor";
import { pairsToRecord, recordToPairs, type Pair } from "@/components/common/kv-pairs";
import { Card, CardContent } from "@/components/ui/card";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Switch } from "@/components/ui/switch";
import { Skeleton } from "@/components/ui/skeleton";
import { cn } from "@/lib/utils";
import {
  Dialog,
  DialogContent,
  DialogHeader,
  DialogTitle,
  DialogFooter,
} from "@/components/ui/dialog";

const STATUS_DOT: Record<TargetHealthStatus, string> = {
  up: "bg-vital-good",
  down: "bg-vital-poor",
  unknown: "bg-muted-foreground/50",
};

function isValidUrl(url: string): boolean {
  try {
    const u = new URL(url);
    return u.protocol === "http:" || u.protocol === "https:";
  } catch {
    return false;
  }
}

export function ScrapeTargetsPage() {
  const { t } = useTranslation();
  const { role } = useAuth();
  const canManage = canManageAlerts(role);
  const queryClient = useQueryClient();

  const targets = useQuery({
    queryKey: ["scrape-targets"],
    queryFn: ({ signal }) => listScrapeTargets(signal),
    retry: false,
  });

  const [formOpen, setFormOpen] = useState(false);
  const [editing, setEditing] = useState<ScrapeTarget | null>(null);
  const [deleting, setDeleting] = useState<ScrapeTarget | null>(null);
  const [toggleError, setToggleError] = useState<string | null>(null);

  const toggle = useMutation({
    mutationFn: (target: ScrapeTarget) => {
      const { id, ...rest } = target;
      return updateScrapeTarget(id, { ...rest, enabled: !target.enabled });
    },
    onMutate: () => setToggleError(null),
    onSuccess: () => queryClient.invalidateQueries({ queryKey: ["scrape-targets"] }),
    onError: (err) => setToggleError(formatErrorMessage(err, t("scrapeTargets.saveError"))),
  });

  function refreshAll() {
    void queryClient.invalidateQueries({ queryKey: ["scrape-targets"] });
    void queryClient.invalidateQueries({ queryKey: ["target-health"] });
  }

  return (
    <PageContainer>
      <PageActions>
        <Button variant="outline" size="sm" onClick={refreshAll} disabled={targets.isFetching}>
          {targets.isFetching ? <Loader2 className="size-3.5 animate-spin" /> : <RefreshCw className="size-3.5" />}
          {t("common.refresh")}
        </Button>
        {canManage && (
          <Button
            onClick={() => {
              setEditing(null);
              setFormOpen(true);
            }}
            className="gap-1.5"
          >
            <Plus className="size-4" />
            {t("scrapeTargets.newTarget")}
          </Button>
        )}
      </PageActions>

      {toggleError && (
        <p className="mb-3 text-sm text-destructive" role="alert">
          {toggleError}
        </p>
      )}

      <Card>
        <CardContent className="p-0">
          {targets.isLoading ? (
            <div className="space-y-2 p-4">
              {[0, 1, 2].map((i) => (
                <Skeleton key={i} className="h-12 w-full" />
              ))}
            </div>
          ) : targets.isError ? (
            <QueryErrorState error={targets.error} onRetry={() => void targets.refetch()} />
          ) : targets.data && targets.data.length > 0 ? (
            <div className="overflow-x-auto">
              <table className="w-full text-sm">
                <thead>
                  <tr className="border-b border-border text-left text-xs uppercase tracking-wider text-muted-foreground">
                    <th className="px-4 py-2.5 font-medium">{t("scrapeTargets.colName")}</th>
                    <th className="px-4 py-2.5 font-medium">{t("scrapeTargets.colHealth")}</th>
                    <th className="hidden px-4 py-2.5 font-medium md:table-cell">
                      {t("scrapeTargets.colInterval")}
                    </th>
                    <th className="px-4 py-2.5 font-medium">{t("common.enabled")}</th>
                    <th className="px-4 py-2.5" />
                  </tr>
                </thead>
                <tbody>
                  {targets.data.map((target) => (
                    <tr
                      key={target.id}
                      className="border-b border-border/60 align-top last:border-0"
                    >
                      <td className="px-4 py-3">
                        <div className="font-medium">{target.name}</div>
                        <div className="mt-0.5 max-w-md truncate font-mono text-xs text-muted-foreground">
                          {target.url}
                        </div>
                        {Object.keys(target.labels).length > 0 && (
                          <div className="mt-1 flex flex-wrap gap-1">
                            {Object.entries(target.labels).map(([k, v]) => (
                              <Badge key={k} variant="outline" className="text-[10px]">
                                {k}={v}
                              </Badge>
                            ))}
                          </div>
                        )}
                      </td>
                      <td className="px-4 py-3">
                        <HealthCell target={target} />
                      </td>
                      <td className="hidden px-4 py-3 font-mono text-xs text-muted-foreground md:table-cell">
                        {target.interval}
                      </td>
                      <td className="px-4 py-3">
                        <Switch
                          checked={target.enabled}
                          disabled={!canManage || toggle.isPending}
                          onCheckedChange={() => toggle.mutate(target)}
                          aria-label={t("common.enabled")}
                        />
                      </td>
                      <td className="px-4 py-3">
                        {canManage && (
                          <div className="flex items-center justify-end gap-1">
                            <Button
                              variant="ghost"
                              size="icon"
                              aria-label={t("common.edit")}
                              onClick={() => {
                                setEditing(target);
                                setFormOpen(true);
                              }}
                            >
                              <Pencil className="size-4" />
                            </Button>
                            <Button
                              variant="ghost"
                              size="icon"
                              aria-label={t("scrapeTargets.deleteTarget")}
                              onClick={() => setDeleting(target)}
                            >
                              <Trash2 className="size-4 text-destructive" />
                            </Button>
                          </div>
                        )}
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          ) : (
            <EmptyState
              icon={<Target className="size-6" />}
              title={t("scrapeTargets.empty")}
              desc={canManage ? t("scrapeTargets.emptyDesc") : undefined}
            />
          )}
        </CardContent>
      </Card>

      {!canManage && (
        <p className="mt-3 text-xs text-muted-foreground">{t("scrapeTargets.readOnlyHint")}</p>
      )}

      {formOpen && (
        <TargetFormDialog
          key={editing?.id ?? "new"}
          open={formOpen}
          onOpenChange={setFormOpen}
          editing={editing}
        />
      )}
      <ConfirmDialog
        open={!!deleting}
        onOpenChange={(o) => !o && setDeleting(null)}
        title={t("scrapeTargets.deleteConfirm", { name: deleting?.name ?? "" })}
        description={t("scrapeTargets.deleteConfirmDesc")}
        confirmLabel={t("scrapeTargets.deleteTarget")}
        onConfirm={async () => {
          if (!deleting) return;
          await deleteScrapeTarget(deleting.id);
          void queryClient.invalidateQueries({ queryKey: ["scrape-targets"] });
          setDeleting(null);
        }}
      />
    </PageContainer>
  );
}

function HealthCell({ target }: { target: ScrapeTarget }) {
  const { t, i18n } = useTranslation();
  const health = useQuery({
    queryKey: ["target-health", target.id, target.enabled, target.url],
    queryFn: ({ signal }) => getTargetHealth(target.id, signal),
    enabled: target.enabled,
    staleTime: 60_000,
    refetchOnWindowFocus: false,
    retry: false,
  });

  const status: TargetHealthStatus = target.enabled ? (health.data?.status ?? "unknown") : "unknown";
  const lastScrape = target.enabled ? health.data?.last_scrape_at : undefined;

  return (
    <div className="flex items-center gap-2">
      <span className={cn("size-2.5 shrink-0 rounded-full", STATUS_DOT[status])} aria-hidden="true" />
      <div className="flex flex-col">
        <span className="text-xs font-medium">{t(`scrapeTargets.status_${status}`)}</span>
        {lastScrape && (
          <span className="text-[11px] text-muted-foreground">
            {formatRelativeTime(dayjs(lastScrape).valueOf(), i18n.language)}
          </span>
        )}
      </div>
    </div>
  );
}

function TargetFormDialog({
  open,
  onOpenChange,
  editing,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  editing: ScrapeTarget | null;
}) {
  const { t } = useTranslation();
  const queryClient = useQueryClient();

  const [name, setName] = useState(editing?.name ?? "");
  const [url, setUrl] = useState(editing?.url ?? "");
  const [interval, setInterval] = useState(editing?.interval ?? "15s");
  const [enabled, setEnabled] = useState<boolean>(editing?.enabled ?? true);
  const [labels, setLabels] = useState<Pair[]>(recordToPairs(editing?.labels ?? {}));
  const [error, setError] = useState<string | null>(null);

  const urlInvalid = url.trim().length > 0 && !isValidUrl(url.trim());

  const input = useMemo<ScrapeTargetInput>(
    () => ({
      name: name.trim(),
      url: url.trim(),
      interval: interval.trim() || "15s",
      labels: pairsToRecord(labels),
      enabled,
    }),
    [name, url, interval, labels, enabled],
  );

  const mutation = useMutation({
    mutationFn: () =>
      editing ? updateScrapeTarget(editing.id, input) : createScrapeTarget(input),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ["scrape-targets"] });
      onOpenChange(false);
    },
    onError: (e) => setError((e as Error).message || t("scrapeTargets.saveError")),
  });

  function submit() {
    if (!input.name) return setError(t("scrapeTargets.nameRequired"));
    if (!input.url) return setError(t("scrapeTargets.urlRequired"));
    if (!isValidUrl(input.url)) return setError(t("scrapeTargets.urlInvalid"));
    setError(null);
    mutation.mutate();
  }

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="max-w-lg">
        <DialogHeader>
          <DialogTitle>
            {editing ? t("scrapeTargets.editTarget") : t("scrapeTargets.newTarget")}
          </DialogTitle>
        </DialogHeader>

        <div className="space-y-4">
          <div className="space-y-1.5">
            <Label htmlFor="tgt-name">{t("scrapeTargets.name")}</Label>
            <Input
              id="tgt-name"
              value={name}
              onChange={(e) => setName(e.target.value)}
              placeholder={t("scrapeTargets.namePlaceholder")}
              autoFocus
            />
          </div>

          <div className="space-y-1.5">
            <Label htmlFor="tgt-url">{t("scrapeTargets.url")}</Label>
            <Input
              id="tgt-url"
              value={url}
              onChange={(e) => setUrl(e.target.value)}
              placeholder={t("scrapeTargets.urlPlaceholder")}
              className={cn("font-mono text-sm", urlInvalid && "border-destructive")}
              spellCheck={false}
              autoComplete="off"
            />
            {urlInvalid && (
              <p className="text-xs text-destructive">{t("scrapeTargets.urlInvalid")}</p>
            )}
          </div>

          <div className="space-y-1.5">
            <Label htmlFor="tgt-interval">{t("scrapeTargets.interval")}</Label>
            <Input
              id="tgt-interval"
              value={interval}
              onChange={(e) => setInterval(e.target.value)}
              placeholder="15s"
              className="w-32 font-mono text-sm"
            />
          </div>

          <div className="space-y-1.5">
            <Label>{t("scrapeTargets.labels")}</Label>
            <KvEditor pairs={labels} onChange={setLabels} />
          </div>

          <div className="flex items-center justify-between rounded-lg border border-border px-3 py-2">
            <Label htmlFor="tgt-enabled" className="cursor-pointer">
              {t("common.enabled")}
            </Label>
            <Switch id="tgt-enabled" checked={enabled} onCheckedChange={setEnabled} />
          </div>

          {error && (
            <p className="text-sm text-destructive" role="alert">
              {error}
            </p>
          )}
        </div>

        <DialogFooter>
          <Button variant="outline" onClick={() => onOpenChange(false)} disabled={mutation.isPending}>
            {t("common.cancel")}
          </Button>
          <Button onClick={submit} disabled={mutation.isPending}>
            {mutation.isPending && <Loader2 className="size-4 animate-spin" />}
            {editing ? t("common.saveChanges") : t("common.create")}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
