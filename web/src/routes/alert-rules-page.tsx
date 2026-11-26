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
  DEFAULT_EVALUATION_INTERVAL,
  EVALUATION_INTERVAL_BOUNDS,
  REPEAT_INTERVAL_BOUNDS,
  SEVERITIES,
  evaluationIntervalProblem,
  repeatIntervalProblem,
  type AlertSignal,
  type AlertRule,
  type AlertRuleInput,
  type Severity,
} from "@/lib/api/alerts";
import { useAuth } from "@/lib/auth/auth-provider";
import { hasCapability } from "@/lib/auth/permissions";
import { useProject } from "@/lib/project/project-provider";
import { projectQueryKey } from "@/lib/project/project-scope";
import { useProjectQuery } from "@/lib/project/use-project-query";
import { useOptimisticList } from "@/hooks/use-optimistic-list";
import { PageActions, PageContainer } from "@/components/layout/page";
import { EmptyState } from "@/components/common/empty-state";
import { isForbiddenError } from "@/lib/api/errors";
import { QueryErrorState } from "@/components/common/query-error-state";
import { QueryStaleNotice } from "@/components/common/query-stale-notice";
import { ConfirmDialog } from "@/components/common/confirm-dialog";
import { HelpTip } from "@/components/common/help-tip";
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
  DialogFooter,
} from "@/components/ui/dialog";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import {
  ANNOTATION_EXAMPLES,
  LABEL_EXAMPLES,
  TEMPLATE_EXAMPLES,
  inheritedRepeatInterval,
  initialRepeatInterval,
  joinRuleAnnotations,
  splitRuleAnnotations,
} from "./alert-rule-form";
import { queryViewState } from "./query-view-state";

export function AlertRulesPage() {
  const { t } = useTranslation();
  const { role } = useAuth();
  const { currentSlug: project } = useProject();
  const canRead = hasCapability(role, "alerts:read");
  const canWrite = hasCapability(role, "alerts:write");
  const queryClient = useQueryClient();

  const rules = useProjectQuery({
    queryKey: ["alert-rules"],
    queryFn: ({ signal, project: activeProject }) => listRules(signal, activeProject),
    enabled: canRead,
    retry: false,
  });

  const [formOpen, setFormOpen] = useState(false);
  const [editing, setEditing] = useState<AlertRule | null>(null);
  const [deleting, setDeleting] = useState<AlertRule | null>(null);
  const [toggleError, setToggleError] = useState<string | null>(null);

  const rulesQueryKey = projectQueryKey(project, ["alert-rules"]);
  const toggle = useOptimisticList<AlertRule[], AlertRule, AlertRule>({
    queryKey: rulesQueryKey,
    mutationFn: (rule: AlertRule) => {
      const input: AlertRuleInput = {
        name: rule.name,
        signal: rule.signal,
        expr: rule.expr,
        for: rule.for,
        evaluation_interval: rule.evaluation_interval,
        repeat_interval: rule.repeat_interval,
        severity: rule.severity,
        labels: rule.labels,
        annotations: rule.annotations,
        channel_ids: rule.channel_ids,
        enabled: !rule.enabled,
      };
      return updateRule(rule.id, input, project);
    },
    optimisticUpdate: (previous, rule) =>
      previous.map((item) => (item.id === rule.id ? { ...item, enabled: !item.enabled } : item)),
    onErrorMessage: setToggleError,
    fallbackError: t("alerts.ruleSaveError"),
  });

  if (!canRead) {
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

  const rows = rules.data ?? [];
  const viewState = queryViewState({
    hasData: rules.data !== undefined,
    isLoading: rules.isLoading,
    isError: rules.isError,
    isEmpty: rows.length === 0,
  });

  return (
    <PageContainer>
      {canWrite && (
        <PageActions>
          <Button onClick={openNew} className="gap-1.5">
            <Plus className="size-4" />
            {t("alerts.newRule")}
          </Button>
        </PageActions>
      )}

      {toggleError && (
        <p className="mb-3 text-sm text-destructive" role="alert">
          {toggleError}
        </p>
      )}

      {viewState === "stale" && (
        <QueryStaleNotice
          isRetrying={rules.isFetching}
          onRetry={() => void rules.refetch()}
        />
      )}

      <Card>
        <CardContent className="p-0">
          {viewState === "pending" ? (
            <div className="space-y-2 p-4">
              {[0, 1, 2].map((i) => (
                <Skeleton key={i} className="h-14 w-full" />
              ))}
            </div>
          ) : viewState === "error" ? (
            <QueryErrorState
              error={rules.error}
              forbiddenDesc={t("alerts.rulesForbiddenDesc")}
              onRetry={() => void rules.refetch()}
            />
          ) : viewState === "empty" ? (
            <EmptyState
              icon={<ListChecks className="size-6" />}
              title={t("alerts.noRules")}
              desc={t("alerts.noRulesDesc")}
            />
          ) : (
            <div className="overflow-x-auto">
              <table className="w-full text-sm">
                <thead>
                  <tr className="border-b border-border text-left text-xs uppercase tracking-wider text-muted-foreground">
                    <th className="px-4 py-2.5 font-medium">{t("common.name")}</th>
                    <th className="px-4 py-2.5 font-medium">{t("alerts.colSeverity")}</th>
                    <th className="hidden px-4 py-2.5 font-medium md:table-cell">
                      {t("alerts.colInterval")}
                    </th>
                    <th className="hidden px-4 py-2.5 font-medium md:table-cell">
                      {t("alerts.colFor")}
                    </th>
                    <th className="hidden px-4 py-2.5 font-medium lg:table-cell">
                      {t("alerts.colRepeat")}
                    </th>
                    <th className="hidden px-4 py-2.5 font-medium lg:table-cell">
                      {t("alerts.colChannels")}
                    </th>
                    <th className="px-4 py-2.5 font-medium">{t("common.status")}</th>
                    <th className="px-4 py-2.5" />
                  </tr>
                </thead>
                <tbody>
                  {rows.map((r) => (
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
                      <td className="hidden whitespace-nowrap px-4 py-3 font-mono text-xs md:table-cell">
                        {r.evaluation_interval ? (
                          r.effective_evaluation_interval
                        ) : (
                          <Tooltip>
                            <TooltipTrigger asChild>
                              <span className="text-muted-foreground">
                                {r.effective_evaluation_interval}
                              </span>
                            </TooltipTrigger>
                            <TooltipContent>{t("alerts.intervalDefaultTooltip")}</TooltipContent>
                          </Tooltip>
                        )}
                      </td>
                      <td className="hidden px-4 py-3 font-mono text-xs text-muted-foreground md:table-cell">
                        {r.for}
                      </td>
                      <td className="hidden whitespace-nowrap px-4 py-3 font-mono text-xs lg:table-cell">
                        {r.repeat_interval ? (
                          r.effective_repeat_interval
                        ) : (
                          <Tooltip>
                            <TooltipTrigger asChild>
                              <span className="text-muted-foreground">
                                {r.effective_repeat_interval}
                              </span>
                            </TooltipTrigger>
                            <TooltipContent>{t("alerts.repeatDefaultTooltip")}</TooltipContent>
                          </Tooltip>
                        )}
                      </td>
                      <td className="hidden px-4 py-3 text-xs text-muted-foreground lg:table-cell">
                        {r.channel_ids.length}
                      </td>
                      <td className="px-4 py-3">
                        <Switch
                          checked={r.enabled}
                          disabled={!canWrite || toggle.isPending}
                          onCheckedChange={() => toggle.mutate(r)}
                          aria-label={t("alerts.enabledField")}
                        />
                      </td>
                      <td className="px-4 py-3">
                        {canWrite && (
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

      {deleting && (
        <ConfirmDialog
          key={deleting.id}
          open
          onOpenChange={(o) => !o && setDeleting(null)}
          title={t("alerts.deleteRuleConfirm", { name: deleting.name })}
          description={t("alerts.deleteRuleConfirmDesc")}
          confirmLabel={t("alerts.deleteRule")}
          onConfirm={async () => {
            await deleteRule(deleting.id, project);
            void queryClient.invalidateQueries({ queryKey: rulesQueryKey });
            setDeleting(null);
          }}
        />
      )}
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
  const { role } = useAuth();
  const { currentSlug: project } = useProject();
  const canReadChannels = hasCapability(role, "channels:read");
  const queryClient = useQueryClient();
  const channels = useQuery({
    queryKey: ["channels"],
    queryFn: ({ signal }) => listChannels(signal),
    enabled: canReadChannels,
    refetchOnWindowFocus: false,
    retry: false,
  });

  const [name, setName] = useState(editing?.name ?? "");
  const [signal, setSignal] = useState<AlertSignal>(editing?.signal ?? "metrics");
  const [expr, setExpr] = useState(editing?.expr ?? "");
  const [forDur, setForDur] = useState(editing?.for ?? "5m");
  // "" keeps the signal default; the server returns the effective value separately.
  const [evalInterval, setEvalInterval] = useState(editing?.evaluation_interval ?? "");
  const [severity, setSeverity] = useState<Severity>(editing?.severity ?? "warning");
  const [labels, setLabels] = useState<Pair[]>(recordToPairs(editing?.labels ?? {}));
  // annotations.summary is edited as the title template; the KV editor holds the rest.
  const [initialAnnotations] = useState(() => splitRuleAnnotations(editing?.annotations ?? {}));
  const [titleTemplate, setTitleTemplate] = useState(initialAnnotations.title);
  const [annotations, setAnnotations] = useState<Pair[]>(initialAnnotations.pairs);
  // New rules prefill the default; "" inherits the server's global repeat_interval.
  const [repeatInterval, setRepeatInterval] = useState(() => initialRepeatInterval(editing));
  const inheritedRepeat = inheritedRepeatInterval(editing);
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
      evaluation_interval: evalInterval.trim(),
      repeat_interval: repeatInterval.trim(),
      severity,
      labels: pairsToRecord(labels),
      annotations: joinRuleAnnotations(titleTemplate, annotations),
      enabled,
      channel_ids: channelIds,
    }),
    [
      name,
      signal,
      expr,
      forDur,
      evalInterval,
      repeatInterval,
      severity,
      labels,
      titleTemplate,
      annotations,
      enabled,
      channelIds,
    ],
  );

  const mutation = useMutation({
    mutationFn: () =>
      editing ? updateRule(editing.id, input, project) : createRule(input, project),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: projectQueryKey(project, ["alert-rules"]) });
      onOpenChange(false);
    },
    onError: (e) => setError((e as Error).message || t("alerts.ruleSaveError")),
  });

  function submit() {
    if (!input.name) return setError(t("alerts.nameRequired"));
    if (!input.expr) return setError(t("alerts.exprRequired"));
    const intervalProblem = evaluationIntervalProblem(input.signal, input.evaluation_interval);
    if (intervalProblem === "format") return setError(t("alerts.evaluationIntervalFormat"));
    if (intervalProblem === "range") {
      return setError(t("alerts.evaluationIntervalInvalid", EVALUATION_INTERVAL_BOUNDS[input.signal]));
    }
    const repeatProblem = repeatIntervalProblem(input.repeat_interval);
    if (repeatProblem === "format") return setError(t("alerts.repeatIntervalFormat"));
    if (repeatProblem === "range") return setError(t("alerts.repeatIntervalInvalid", REPEAT_INTERVAL_BOUNDS));
    setError(null);
    mutation.mutate();
  }

  const channelRows = channels.data ?? [];
  const channelsState = queryViewState({
    hasData: channels.data !== undefined,
    isLoading: channels.isLoading,
    isError: channels.isError,
    isEmpty: channelRows.length === 0,
  });

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="max-w-2xl" aria-describedby={undefined}>
        <DialogHeader>
          <DialogTitle>{editing ? t("alerts.editRule") : t("alerts.newRule")}</DialogTitle>
        </DialogHeader>

        <div className="space-y-4">
          {/* Row 1: identity and severity; row 2: how the rule is evaluated. */}
          <div className="grid gap-4 sm:grid-cols-3">
            <div className="space-y-1.5 sm:col-span-2">
              <Label htmlFor="r-name">{t("alerts.ruleName")}</Label>
              <Input
                id="r-name"
                value={name}
                onChange={(e) => setName(e.target.value)}
                placeholder={t("alerts.ruleNamePlaceholder")}
                autoFocus
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

          <div className="grid gap-4 sm:grid-cols-3">
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
              <Label htmlFor="r-interval">{t("alerts.evaluationInterval")}</Label>
              <Input
                id="r-interval"
                value={evalInterval}
                onChange={(e) => setEvalInterval(e.target.value)}
                placeholder={t("alerts.evaluationIntervalDefault", {
                  value: DEFAULT_EVALUATION_INTERVAL[signal],
                })}
                spellCheck={false}
                autoComplete="off"
                className="font-mono text-sm"
              />
            </div>
            <div className="space-y-1.5">
              <Label htmlFor="r-for">{t("alerts.forDuration")}</Label>
              <Input
                id="r-for"
                value={forDur}
                onChange={(e) => setForDur(e.target.value)}
                placeholder={t("alerts.forPlaceholder")}
                spellCheck={false}
                autoComplete="off"
                className="font-mono text-sm"
              />
            </div>
          </div>

          <div className="space-y-1.5">
            <div className="flex items-center gap-1.5">
              <Label>
                {t(signal === "logs" ? "alerts.exprLogs" : "alerts.exprMetrics")}
              </Label>
              <HelpTip
                content={t(signal === "logs" ? "alerts.exprLogsHint" : "alerts.exprMetricsHint")}
              />
            </div>
            {/* Query-language input uses the shared CodeMirror editors (AGENTS.md). */}
            {signal === "logs" ? (
              <LogsQLEditor
                key="logs"
                id="alert-rule-logsql"
                value={expr}
                placeholderText={t("alerts.exprLogsPlaceholder")}
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
                placeholderText={t("alerts.exprMetricsPlaceholder")}
                onChange={setExpr}
                onRun={() => undefined}
              />
            )}
          </div>

          {/* Notification: rendered title and how often a still-firing alert re-notifies. */}
          <div className="grid gap-4 sm:grid-cols-3">
            <div className="space-y-1.5 sm:col-span-2">
              <div className="flex items-center gap-1.5">
                <Label htmlFor="r-title">{t("alerts.titleTemplate")}</Label>
                <HelpTip
                  scopeTitle={t("alerts.titleTemplateHelpTitle")}
                  scope={[
                    { text: t("alerts.titleTemplateHelpLabels") },
                    { text: t("alerts.titleTemplateHelpValue") },
                    { text: t("alerts.titleTemplateHelpEmpty") },
                  ]}
                  examples={[TEMPLATE_EXAMPLES.instance, TEMPLATE_EXAMPLES.value]}
                />
              </div>
              <Input
                id="r-title"
                value={titleTemplate}
                onChange={(e) => setTitleTemplate(e.target.value)}
                placeholder={t("alerts.titleTemplatePlaceholder", TEMPLATE_EXAMPLES)}
                spellCheck={false}
                autoComplete="off"
              />
            </div>
            <div className="space-y-1.5">
              <div className="flex items-center gap-1.5">
                <Label htmlFor="r-repeat">{t("alerts.repeatInterval")}</Label>
                <HelpTip content={t("alerts.repeatIntervalHelp")} />
              </div>
              <Input
                id="r-repeat"
                value={repeatInterval}
                onChange={(e) => setRepeatInterval(e.target.value)}
                placeholder={
                  inheritedRepeat
                    ? t("alerts.repeatIntervalInherited", { value: inheritedRepeat })
                    : t("alerts.repeatIntervalInherit")
                }
                spellCheck={false}
                autoComplete="off"
                className="font-mono"
              />
            </div>
          </div>

          {/* Full-width rows: annotation values are templates and URLs that need room. */}
          <div className="space-y-4">
            <div className="space-y-1.5">
              <div className="flex items-center gap-1.5">
                <Label>{t("alerts.labelsField")}</Label>
                <HelpTip
                  scopeTitle={t("alerts.labelsHelpTitle")}
                  scope={[
                    { text: t("alerts.labelsHelpMatch") },
                    { text: t("alerts.labelsHelpLiteral") },
                    { text: t("alerts.labelsHelpTemplate") },
                  ]}
                  examples={[...LABEL_EXAMPLES]}
                />
              </div>
              <KvEditor pairs={labels} onChange={setLabels} />
            </div>
            <div className="space-y-1.5">
              <div className="flex items-center gap-1.5">
                <Label>{t("alerts.annotationsField")}</Label>
                <HelpTip
                  scopeTitle={t("alerts.annotationsHelpTitle")}
                  scope={[
                    { text: t("alerts.annotationsHelpTemplate") },
                    { text: t("alerts.annotationsHelpKeys") },
                  ]}
                  examples={[...ANNOTATION_EXAMPLES]}
                />
              </div>
              <KvEditor pairs={annotations} onChange={setAnnotations} />
            </div>
          </div>

          <div className="space-y-1.5">
            <Label>{t("alerts.channelsField")}</Label>
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
            ) : channelsState === "empty" ? (
              <p className="text-xs text-muted-foreground">{t("alerts.noChannelsAvailable")}</p>
            ) : (
              <div className="flex flex-wrap gap-2">
                {channelRows.map((ch) => {
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
