import { useMemo, useState } from "react";
import { useTranslation } from "react-i18next";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { ListChecks, Loader2, Pencil, Plus, ShieldAlert, Trash2 } from "lucide-react";
import {
  listRules,
  createRule,
  updateRule,
  deleteRule,
  listChannels,
  ALERT_SIGNALS,
  SEVERITIES,
  type AlertSignal,
  type AlertRule,
  type AlertRuleInput,
  type Severity,
} from "@/lib/api/alerts";
import { useAuth } from "@/lib/auth/auth-provider";
import { canManageAlerts } from "@/lib/auth/permissions";
import { PageActions, PageContainer } from "@/components/layout/page";
import { EmptyState } from "@/components/common/empty-state";
import { isForbiddenError } from "@/lib/api/errors";
import { QueryErrorState } from "@/components/common/query-error-state";
import { ConfirmDialog } from "@/components/common/confirm-dialog";
import { SeverityBadge } from "@/components/alerts/severity";
import { KvEditor } from "@/components/common/kv-editor";
import { pairsToRecord, recordToPairs, type Pair } from "@/components/common/kv-pairs";
import { Card, CardContent } from "@/components/ui/card";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { PromQLEditor } from "@/components/metrics/promql-editor";
import { LogsQLEditor } from "@/components/logs/logsql-editor";
import { resolveMetricsRange } from "@/lib/metrics-query";
import { Switch } from "@/components/ui/switch";
import { Skeleton } from "@/components/ui/skeleton";
import { cn } from "@/lib/utils";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import {
  Dialog,
  DialogContent,
  DialogHeader,
  DialogTitle,
  DialogDescription,
  DialogFooter,
} from "@/components/ui/dialog";

export function AlertRulesPage() {
  const { t } = useTranslation();
  const { role } = useAuth();
  const canManage = canManageAlerts(role);
  const queryClient = useQueryClient();

  const rules = useQuery({
    queryKey: ["alert-rules"],
    queryFn: ({ signal }) => listRules(signal),
    enabled: canManage,
    retry: false,
  });

  const [formOpen, setFormOpen] = useState(false);
  const [editing, setEditing] = useState<AlertRule | null>(null);
  const [deleting, setDeleting] = useState<AlertRule | null>(null);

  const toggle = useMutation({
    mutationFn: (rule: AlertRule) => {
      const input: AlertRuleInput = {
        name: rule.name,
        signal: rule.signal,
        expr: rule.expr,
        for: rule.for,
        severity: rule.severity,
        labels: rule.labels,
        annotations: rule.annotations,
        channel_ids: rule.channel_ids,
        enabled: !rule.enabled,
      };
      return updateRule(rule.id, input);
    },
    onSuccess: () => queryClient.invalidateQueries({ queryKey: ["alert-rules"] }),
  });

  if (!canManage) {
    return (
      <PageContainer>
        <Card>
          <CardContent>
            <EmptyState
              icon={<ShieldAlert className="size-6" />}
              title={t("common.forbiddenTitle")}
              desc={t("alerts.rulesForbiddenDesc")}
            />
          </CardContent>
        </Card>
      </PageContainer>
    );
  }

  function openNew() {
    setEditing(null);
    setFormOpen(true);
  }
  function openEdit(rule: AlertRule) {
    setEditing(rule);
    setFormOpen(true);
  }

  return (
    <PageContainer>
      <PageActions>
        <Button onClick={openNew} className="gap-1.5">
          <Plus className="size-4" />
          {t("alerts.newRule")}
        </Button>
      </PageActions>

      <Card>
        <CardContent className="p-0">
          {rules.isLoading ? (
            <div className="space-y-2 p-4">
              {[0, 1, 2].map((i) => (
                <Skeleton key={i} className="h-14 w-full" />
              ))}
            </div>
          ) : rules.isError ? (
            <QueryErrorState
              error={rules.error}
              forbiddenDesc={t("alerts.rulesForbiddenDesc")}
              onRetry={() => void rules.refetch()}
            />
          ) : rules.data && rules.data.length > 0 ? (
            <div className="overflow-x-auto">
              <table className="w-full text-sm">
                <thead>
                  <tr className="border-b border-border text-left text-xs uppercase tracking-wider text-muted-foreground">
                    <th className="px-4 py-2.5 font-medium">{t("common.name")}</th>
                    <th className="px-4 py-2.5 font-medium">{t("alerts.colSeverity")}</th>
                    <th className="hidden px-4 py-2.5 font-medium md:table-cell">
                      {t("alerts.colFor")}
                    </th>
                    <th className="hidden px-4 py-2.5 font-medium lg:table-cell">
                      {t("alerts.colChannels")}
                    </th>
                    <th className="px-4 py-2.5 font-medium">{t("common.status")}</th>
                    <th className="px-4 py-2.5" />
                  </tr>
                </thead>
                <tbody>
                  {rules.data.map((r) => (
                    <tr key={r.id} className="border-b border-border/60 last:border-0 align-top">
                      <td className="px-4 py-3">
                        <div className="flex items-center gap-2 font-medium">
                          <span>{r.name}</span>
                          <span className="rounded bg-muted px-1.5 py-0.5 text-[10px] font-medium uppercase text-muted-foreground">
                            {t(r.signal === "logs" ? "alerts.signalLogs" : "alerts.signalMetrics")}
                          </span>
                        </div>
                        <div className="mt-0.5 max-w-md truncate font-mono text-xs text-muted-foreground">
                          {r.expr}
                        </div>
                      </td>
                      <td className="px-4 py-3">
                        <SeverityBadge severity={r.severity} />
                      </td>
                      <td className="hidden px-4 py-3 font-mono text-xs text-muted-foreground md:table-cell">
                        {r.for}
                      </td>
                      <td className="hidden px-4 py-3 text-xs text-muted-foreground lg:table-cell">
                        {r.channel_ids.length}
                      </td>
                      <td className="px-4 py-3">
                        <Switch
                          checked={r.enabled}
                          disabled={!canManage || toggle.isPending}
                          onCheckedChange={() => toggle.mutate(r)}
                          aria-label={t("alerts.enabledField")}
                        />
                      </td>
                      <td className="px-4 py-3">
                        {canManage && (
                          <div className="flex items-center justify-end gap-1">
                            <Button
                              variant="ghost"
                              size="icon"
                              aria-label={t("common.edit")}
                              onClick={() => openEdit(r)}
                            >
                              <Pencil className="size-4" />
                            </Button>
                            <Button
                              variant="ghost"
                              size="icon"
                              aria-label={t("alerts.deleteRule")}
                              onClick={() => setDeleting(r)}
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
              icon={<ListChecks className="size-6" />}
              title={t("alerts.noRules")}
              desc={t("alerts.noRulesDesc")}
            />
          )}
        </CardContent>
      </Card>

      {formOpen && (
        <RuleFormDialog
          key={editing?.id ?? "new"}
          open={formOpen}
          onOpenChange={setFormOpen}
          editing={editing}
        />
      )}

      <ConfirmDialog
        open={!!deleting}
        onOpenChange={(o) => !o && setDeleting(null)}
        title={t("alerts.deleteRuleConfirm", { name: deleting?.name ?? "" })}
        description={t("alerts.deleteRuleConfirmDesc")}
        confirmLabel={t("alerts.deleteRule")}
        onConfirm={async () => {
          if (!deleting) return;
          await deleteRule(deleting.id);
          void queryClient.invalidateQueries({ queryKey: ["alert-rules"] });
          setDeleting(null);
        }}
      />
    </PageContainer>
  );
}

function RuleFormDialog({
  open,
  onOpenChange,
  editing,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  editing: AlertRule | null;
}) {
  const { t } = useTranslation();
  const queryClient = useQueryClient();
  const channels = useQuery({
    queryKey: ["channels"],
    queryFn: ({ signal }) => listChannels(signal),
    retry: false,
  });

  const [name, setName] = useState(editing?.name ?? "");
  const [signal, setSignal] = useState<AlertSignal>(editing?.signal ?? "metrics");
  const [expr, setExpr] = useState(editing?.expr ?? "");
  const [forDur, setForDur] = useState(editing?.for ?? "5m");
  const [severity, setSeverity] = useState<Severity>(editing?.severity ?? "warning");
  const [labels, setLabels] = useState<Pair[]>(recordToPairs(editing?.labels ?? {}));
  const [annotations, setAnnotations] = useState<Pair[]>(
    recordToPairs(editing?.annotations ?? {}),
  );
  const [channelIds, setChannelIds] = useState<string[]>(editing?.channel_ids ?? []);
  const [enabled, setEnabled] = useState<boolean>(editing?.enabled ?? true);
  const [error, setError] = useState<string | null>(null);
  // Metric-name autocomplete looks at the last hour, like the metrics explorer default.
  const [autocompleteRange] = useState(() =>
    resolveMetricsRange({ mode: "relative", presetId: "1h", seconds: 3600 }),
  );

  const input = useMemo<AlertRuleInput>(
    () => ({
      name: name.trim(),
      signal,
      expr: expr.trim(),
      for: forDur.trim() || "0m",
      severity,
      labels: pairsToRecord(labels),
      annotations: pairsToRecord(annotations),
      enabled,
      channel_ids: channelIds,
    }),
    [name, signal, expr, forDur, severity, labels, annotations, enabled, channelIds],
  );

  const mutation = useMutation({
    mutationFn: () => (editing ? updateRule(editing.id, input) : createRule(input)),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ["alert-rules"] });
      onOpenChange(false);
    },
    onError: (e) => setError((e as Error).message || t("alerts.ruleSaveError")),
  });

  function submit() {
    if (!input.name) return setError(t("alerts.nameRequired"));
    if (!input.expr) return setError(t("alerts.exprRequired"));
    setError(null);
    mutation.mutate();
  }

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="max-w-2xl">
        <DialogHeader>
          <DialogTitle>{editing ? t("alerts.editRule") : t("alerts.newRule")}</DialogTitle>
        </DialogHeader>

        <div className="space-y-4">
          <div className="grid gap-4 sm:grid-cols-2">
            <div className="space-y-1.5">
              <Label htmlFor="r-name">{t("alerts.ruleName")}</Label>
              <Input
                id="r-name"
                value={name}
                onChange={(e) => setName(e.target.value)}
                placeholder={t("alerts.ruleNamePlaceholder")}
                autoFocus
              />
            </div>
            <div className="grid grid-cols-2 gap-4">
              <div className="space-y-1.5">
                <Label htmlFor="r-for">{t("alerts.forDuration")}</Label>
                <Input
                  id="r-for"
                  value={forDur}
                  onChange={(e) => setForDur(e.target.value)}
                  placeholder={t("alerts.forPlaceholder")}
                  className="font-mono text-sm"
                />
              </div>
              <div className="space-y-1.5">
                <Label>{t("alerts.severity")}</Label>
                <Select value={severity} onValueChange={(v) => setSeverity(v as Severity)}>
                  <SelectTrigger>
                    <SelectValue />
                  </SelectTrigger>
                  <SelectContent>
                    {SEVERITIES.map((s) => (
                      <SelectItem key={s} value={s}>
                        {t(`alerts.sev${s.charAt(0).toUpperCase()}${s.slice(1)}`)}
                      </SelectItem>
                    ))}
                  </SelectContent>
                </Select>
              </div>
            </div>
          </div>

          <div className="space-y-1.5">
            <Label>{t("alerts.signal")}</Label>
            <Select value={signal} onValueChange={(value) => setSignal(value as AlertSignal)}>
              <SelectTrigger>
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                {ALERT_SIGNALS.map((value) => (
                  <SelectItem key={value} value={value}>
                    {t(value === "logs" ? "alerts.signalLogs" : "alerts.signalMetrics")}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
          </div>

          <div className="space-y-1.5">
            <Label>
              {t(signal === "logs" ? "alerts.exprLogs" : "alerts.exprMetrics")}
            </Label>
            {/* Query-language input uses the shared CodeMirror editors (AGENTS.md). */}
            {signal === "logs" ? (
              <LogsQLEditor
                key="logs"
                id="alert-rule-logsql"
                value={expr}
                onChange={setExpr}
                onRun={() => undefined}
                autocompleteEnabled={false}
              />
            ) : (
              <PromQLEditor
                key="metrics"
                id="alert-rule-promql"
                value={expr}
                range={autocompleteRange}
                onChange={setExpr}
                onRun={() => undefined}
              />
            )}
            <p className="font-mono text-xs text-muted-foreground">
              {t(signal === "logs" ? "alerts.exprLogsPlaceholder" : "alerts.exprMetricsPlaceholder")}
            </p>
            <p className="text-xs text-muted-foreground">
              {t(signal === "logs" ? "alerts.exprLogsHint" : "alerts.exprMetricsHint")}
            </p>
          </div>

          <div className="grid gap-4 sm:grid-cols-2">
            <div className="space-y-1.5">
              <Label>{t("alerts.labelsField")}</Label>
              <KvEditor pairs={labels} onChange={setLabels} />
            </div>
            <div className="space-y-1.5">
              <Label>{t("alerts.annotationsField")}</Label>
              <KvEditor pairs={annotations} onChange={setAnnotations} />
            </div>
          </div>

          <div className="space-y-1.5">
            <Label>{t("alerts.channelsField")}</Label>
            {channels.isLoading ? (
              <Skeleton className="h-8 w-48" />
            ) : channels.isError ? (
              <div className="flex flex-wrap items-center gap-2 text-xs text-destructive" role="alert">
                <span>
                  {isForbiddenError(channels.error)
                    ? t("alerts.channelsLoadForbidden")
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
                  const selected = channelIds.includes(ch.id);
                  return (
                    <button
                      key={ch.id}
                      type="button"
                      onClick={() =>
                        setChannelIds((prev) =>
                          selected ? prev.filter((id) => id !== ch.id) : [...prev, ch.id],
                        )
                      }
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
              <p className="text-xs text-muted-foreground">{t("alerts.noChannelsAvailable")}</p>
            )}
          </div>

          <div className="flex items-center justify-between rounded-lg border border-border px-3 py-2">
            <Label htmlFor="r-enabled" className="cursor-pointer">
              {t("alerts.enabledField")}
            </Label>
            <Switch id="r-enabled" checked={enabled} onCheckedChange={setEnabled} />
          </div>

          {error && (
            <p className="text-sm text-destructive" role="alert">
              {error}
            </p>
          )}
        </div>

        <DialogDescription className="sr-only">{t("alerts.rulesSubtitle")}</DialogDescription>
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
