import { useMemo, useState, type KeyboardEvent } from "react";
import { useTranslation } from "react-i18next";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import {
  ArrowRight,
  Loader2,
  Pencil,
  Plus,
  ShieldAlert,
  ShieldBan,
  Trash2,
  X,
} from "lucide-react";
import {
  listInhibitionRules,
  createInhibitionRule,
  updateInhibitionRule,
  deleteInhibitionRule,
  type InhibitionRule,
  type InhibitionRuleInput,
  type Matcher,
} from "@/lib/api/alerts";
import { formatErrorMessage } from "@/lib/api/errors";
import { useAuth } from "@/lib/auth/auth-provider";
import { useProject } from "@/lib/project/project-provider";
import { projectQueryKey } from "@/lib/project/project-scope";
import { useProjectQuery } from "@/lib/project/use-project-query";
import { useOptimisticList } from "@/hooks/use-optimistic-list";
import { formatDateTime } from "@/lib/format";
import dayjs from "@/lib/dayjs";
import { PageActions, PageContainer } from "@/components/layout/page";
import { EmptyState } from "@/components/common/empty-state";
import { QueryErrorState } from "@/components/common/query-error-state";
import { QueryStaleNotice } from "@/components/common/query-stale-notice";
import { ConfirmDialog } from "@/components/common/confirm-dialog";
import { HelpTip } from "@/components/common/help-tip";
import { Card, CardContent } from "@/components/ui/card";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Textarea } from "@/components/ui/textarea";
import { Switch } from "@/components/ui/switch";
import { Skeleton } from "@/components/ui/skeleton";
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
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { queryViewState } from "./query-view-state";
import {
  INHIBITION_MATCHER_OPS,
  MAX_INHIBITION_MATCHERS,
  MAX_MATCHER_REGEX_BYTES,
  addMatcherRow,
  commitEqualLabelDraft,
  createInhibitionFormDraft,
  partitionInhibitionRules,
  removeEqualLabel,
  removeMatcherRow,
  resolveInhibitionAccess,
  updateMatcherRow,
  validateInhibitionDraft,
  validateMatcherPattern,
  type InhibitionValidationErrorCode,
} from "./inhibition-rules-state";

const fmt = (iso?: string) => (iso ? formatDateTime(dayjs(iso).valueOf()) : "—");

function validationErrorKey(code: InhibitionValidationErrorCode): string {
  switch (code) {
    case "sourceMatchersRequired":
      return "alerts.inhibitionSourceRequired";
    case "targetMatchersRequired":
      return "alerts.inhibitionTargetRequired";
    case "sourceMatcherIncomplete":
      return "alerts.inhibitionSourceIncomplete";
    case "targetMatcherIncomplete":
      return "alerts.inhibitionTargetIncomplete";
    case "sourceMatchersTooMany":
      return "alerts.inhibitionSourceTooMany";
    case "targetMatchersTooMany":
      return "alerts.inhibitionTargetTooMany";
    case "sourceRegexTooLong":
      return "alerts.inhibitionSourceRegexTooLong";
    case "targetRegexTooLong":
      return "alerts.inhibitionTargetRegexTooLong";
    case "sourceRegexInvalid":
      return "alerts.inhibitionSourceRegexInvalid";
    case "targetRegexInvalid":
      return "alerts.inhibitionTargetRegexInvalid";
  }
}

export function InhibitionRulesPage() {
  const { t } = useTranslation();
  const { role } = useAuth();
  const { currentSlug: project } = useProject();
  const { canRead, canWrite } = resolveInhibitionAccess(role);
  const queryClient = useQueryClient();

  const rules = useProjectQuery({
    queryKey: ["inhibition-rules"],
    queryFn: ({ signal, project: activeProject }) =>
      listInhibitionRules(signal, activeProject),
    enabled: canRead,
    retry: false,
  });

  const [formOpen, setFormOpen] = useState(false);
  const [editing, setEditing] = useState<InhibitionRule | null>(null);
  const [deleting, setDeleting] = useState<InhibitionRule | null>(null);
  const [toggleError, setToggleError] = useState<string | null>(null);

  const rulesQueryKey = projectQueryKey(project, ["inhibition-rules"]);
  const toggle = useOptimisticList<InhibitionRule[], InhibitionRule, InhibitionRule>({
    queryKey: rulesQueryKey,
    mutationFn: (rule: InhibitionRule) =>
      updateInhibitionRule(rule.id, { enabled: !rule.enabled }, project),
    optimisticUpdate: (previous, rule) =>
      previous.map((item) =>
        item.id === rule.id ? { ...item, enabled: !item.enabled } : item,
      ),
    onErrorMessage: setToggleError,
    fallbackError: t("alerts.inhibitionSaveError"),
  });

  const { enabled: enabledRules, disabled: disabledRules } = useMemo(
    () => partitionInhibitionRules(rules.data ?? []),
    [rules.data],
  );

  if (!canRead) {
    return (
      <PageContainer>
        <Card>
          <CardContent>
            <EmptyState
              icon={<ShieldAlert className="size-6" />}
              title={t("common.forbiddenTitle")}
              desc={t("alerts.inhibitionsForbiddenDesc")}
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

  function openEdit(rule: InhibitionRule) {
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
            {t("alerts.newInhibition")}
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

      {viewState === "pending" ? (
        <div className="space-y-3">
          <Skeleton className="h-28 w-full" />
          <Skeleton className="h-28 w-full" />
        </div>
      ) : viewState === "error" ? (
        <Card>
          <CardContent>
            <QueryErrorState
              error={rules.error}
              forbiddenDesc={t("alerts.inhibitionsForbiddenDesc")}
              onRetry={() => void rules.refetch()}
            />
          </CardContent>
        </Card>
      ) : viewState === "empty" ? (
        <Card>
          <CardContent>
            <EmptyState
              icon={<ShieldBan className="size-6" />}
              title={t("alerts.noInhibitions")}
              desc={t("alerts.noInhibitionsDesc")}
            />
          </CardContent>
        </Card>
      ) : (
        <div className="space-y-6">
          <Section title={t("alerts.enabledSection")} count={enabledRules.length}>
            {enabledRules.map((rule) => (
              <InhibitionRuleCard
                key={rule.id}
                rule={rule}
                canManage={canWrite}
                togglePending={toggle.isPending}
                onToggle={() => toggle.mutate(rule)}
                onEdit={() => openEdit(rule)}
                onDelete={() => setDeleting(rule)}
              />
            ))}
          </Section>
          {disabledRules.length > 0 && (
            <Section title={t("alerts.disabledSection")} count={disabledRules.length}>
              {disabledRules.map((rule) => (
                <InhibitionRuleCard
                  key={rule.id}
                  rule={rule}
                  canManage={canWrite}
                  togglePending={toggle.isPending}
                  onToggle={() => toggle.mutate(rule)}
                  onEdit={() => openEdit(rule)}
                  onDelete={() => setDeleting(rule)}
                />
              ))}
            </Section>
          )}
        </div>
      )}

      {formOpen && (
        <InhibitionFormDialog
          key={editing?.id ?? "new"}
          open={formOpen}
          onOpenChange={(nextOpen) => {
            setFormOpen(nextOpen);
            if (!nextOpen) setEditing(null);
          }}
          editing={editing}
        />
      )}

      {deleting && (
        <ConfirmDialog
          key={deleting.id}
          open
          onOpenChange={(o) => !o && setDeleting(null)}
          title={t("alerts.deleteInhibitionConfirm")}
          description={t("alerts.deleteInhibitionConfirmDesc")}
          confirmLabel={t("alerts.deleteInhibition")}
          onConfirm={async () => {
            await deleteInhibitionRule(deleting.id, project);
            void queryClient.invalidateQueries({ queryKey: rulesQueryKey });
            setDeleting(null);
          }}
        />
      )}
    </PageContainer>
  );
}

function Section({
  title,
  count,
  children,
}: {
  title: string;
  count: number;
  children: React.ReactNode;
}) {
  if (count === 0) return null;
  return (
    <div>
      <div className="mb-2 flex items-center gap-2 text-sm font-medium text-muted-foreground">
        {title}
        <span className="rounded-full bg-muted px-2 py-0.5 text-xs">{count}</span>
      </div>
      <div className="grid gap-3">{children}</div>
    </div>
  );
}

function MatcherChipList({ matchers }: { matchers: readonly Matcher[] }) {
  return (
    <div className="flex flex-wrap gap-1.5">
      {matchers.map((m, i) => (
        <span
          key={`${m.name}:${m.op}:${m.value}:${i}`}
          className="inline-flex items-center gap-1 rounded border border-border bg-muted/40 px-1.5 py-0.5 font-mono text-xs"
        >
          <span className="text-foreground/80">{m.name}</span>
          <span className="text-muted-foreground">{m.op}</span>
          <span>{m.value}</span>
        </span>
      ))}
    </div>
  );
}

function InhibitionRuleCard({
  rule,
  canManage,
  togglePending,
  onToggle,
  onEdit,
  onDelete,
}: {
  rule: InhibitionRule;
  canManage: boolean;
  togglePending: boolean;
  onToggle: () => void;
  onEdit: () => void;
  onDelete: () => void;
}) {
  const { t } = useTranslation();
  const timestamp = rule.updated_at || rule.created_at;

  return (
    <Card className={rule.enabled ? undefined : "opacity-75"}>
      <CardContent className="flex flex-col gap-3 p-4 sm:flex-row sm:items-start sm:justify-between">
        <div className="min-w-0 flex-1 space-y-3">
          <div className="flex flex-wrap items-center gap-2">
            <span className="text-sm font-medium">
              {rule.comment || t("alerts.inhibitionFallbackTitle", { id: rule.id })}
            </span>
            <Badge variant={rule.enabled ? "secondary" : "outline"} className="text-[11px]">
              {rule.enabled ? t("common.enabled") : t("common.disabled")}
            </Badge>
          </div>

          <div className="grid gap-3 lg:grid-cols-[minmax(0,1fr)_auto_minmax(0,1fr)] lg:items-start">
            <div className="space-y-1">
              <div className="text-[11px] font-medium uppercase tracking-wider text-muted-foreground">
                {t("alerts.sourceMatchers")}
              </div>
              <MatcherChipList matchers={rule.source_matchers} />
            </div>

            <div className="hidden pt-5 text-muted-foreground lg:flex" aria-hidden>
              <ArrowRight className="size-4" />
            </div>

            <div className="space-y-1">
              <div className="text-[11px] font-medium uppercase tracking-wider text-muted-foreground">
                {t("alerts.targetMatchers")}
              </div>
              <MatcherChipList matchers={rule.target_matchers} />
            </div>
          </div>

          <div className="flex flex-wrap items-center gap-x-4 gap-y-1.5 text-xs text-muted-foreground">
            <div className="flex flex-wrap items-center gap-1.5">
              <span>{t("alerts.equalLabels")}:</span>
              {rule.equal_labels.length > 0 ? (
                rule.equal_labels.map((label, idx) => (
                  <span
                    key={`${label}:${idx}`}
                    className="rounded border border-border bg-muted/40 px-1.5 py-0.5 font-mono text-[11px] text-foreground/80"
                  >
                    {label}
                  </span>
                ))
              ) : (
                <span className="italic">{t("alerts.equalLabelsNone")}</span>
              )}
            </div>
            {timestamp && (
              <span>
                {t("alerts.updatedAt")}: {fmt(timestamp)}
              </span>
            )}
          </div>
        </div>

        {canManage && (
          <div className="flex shrink-0 items-center gap-2">
            <Switch
              checked={rule.enabled}
              disabled={togglePending}
              onCheckedChange={onToggle}
              aria-label={t("alerts.enabledField")}
            />
            <Button
              variant="ghost"
              size="icon"
              aria-label={t("common.edit")}
              onClick={onEdit}
            >
              <Pencil className="size-4" />
            </Button>
            <Button
              variant="ghost"
              size="icon"
              aria-label={t("alerts.deleteInhibition")}
              onClick={onDelete}
            >
              <Trash2 className="size-4 text-destructive" />
            </Button>
          </div>
        )}
      </CardContent>
    </Card>
  );
}

function MatcherListEditor({
  label,
  hint,
  matchers,
  onChange,
}: {
  label: string;
  hint: string;
  matchers: Matcher[];
  onChange: (next: Matcher[]) => void;
}) {
  const { t } = useTranslation();

  return (
    <div className="space-y-2">
      <div className="flex items-center gap-1.5">
        <Label>{label}</Label>
        <HelpTip content={hint} />
      </div>
      {matchers.map((m, i) => {
        const regexState =
          m.op === "=~" && m.value.trim() !== ""
            ? validateMatcherPattern(m.value.trim())
            : null;
        return (
          <div key={i} className="space-y-1">
            <div className="flex items-center gap-2">
              <Input
                value={m.name}
                onChange={(e) =>
                  onChange(updateMatcherRow(matchers, i, { name: e.target.value }))
                }
                placeholder={t("alerts.matcherName")}
                className="font-mono text-sm"
                spellCheck={false}
              />
              <Select
                value={m.op}
                onValueChange={(v) =>
                  onChange(updateMatcherRow(matchers, i, { op: v as Matcher["op"] }))
                }
              >
                <SelectTrigger className="w-28 shrink-0">
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  {INHIBITION_MATCHER_OPS.map((op) => (
                    <SelectItem key={op} value={op}>
                      {op === "=" ? t("alerts.opEquals") : t("alerts.opRegex")}
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
              <Input
                value={m.value}
                onChange={(e) =>
                  onChange(updateMatcherRow(matchers, i, { value: e.target.value }))
                }
                placeholder={t("alerts.matcherValue")}
                className="font-mono text-sm"
                spellCheck={false}
              />
              <Button
                type="button"
                variant="ghost"
                size="icon"
                onClick={() => onChange(removeMatcherRow(matchers, i))}
                disabled={matchers.length === 1}
                aria-label={t("common.remove")}
              >
                <X className="size-4" />
              </Button>
            </div>
            {regexState && !regexState.ok && (
              <p className="text-xs text-destructive" role="alert">
                {regexState.code === "regexTooLong"
                  ? t("alerts.matcherRegexTooLongHint", { max: MAX_MATCHER_REGEX_BYTES })
                  : t("alerts.matcherRegexInvalidHint")}
              </p>
            )}
          </div>
        );
      })}
      <Button
        type="button"
        variant="outline"
        size="sm"
        className="gap-1.5"
        disabled={matchers.length >= MAX_INHIBITION_MATCHERS}
        onClick={() => onChange(addMatcherRow(matchers))}
      >
        <Plus className="size-3.5" />
        {t("alerts.addMatcher")}
      </Button>
    </div>
  );
}

function InhibitionFormDialog({
  open,
  onOpenChange,
  editing,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  editing: InhibitionRule | null;
}) {
  const { t } = useTranslation();
  const { currentSlug: project } = useProject();
  const queryClient = useQueryClient();

  const [draft, setDraft] = useState(() => createInhibitionFormDraft(editing));
  const [error, setError] = useState<string | null>(null);

  const mutation = useMutation({
    mutationFn: (input: InhibitionRuleInput) =>
      editing
        ? updateInhibitionRule(editing.id, input, project)
        : createInhibitionRule(input, project),
    onSuccess: () => {
      void queryClient.invalidateQueries({
        queryKey: projectQueryKey(project, ["inhibition-rules"]),
      });
      onOpenChange(false);
    },
    onError: (e) => setError(formatErrorMessage(e, t("alerts.inhibitionSaveError"))),
  });

  function addEqualLabelsFromInput() {
    if (!draft.equalLabelInput.trim()) return;
    const { labels, remaining } = commitEqualLabelDraft(
      draft.equalLabels,
      draft.equalLabelInput,
    );
    setDraft((prev) => ({ ...prev, equalLabels: labels, equalLabelInput: remaining }));
  }

  function handleEqualLabelKeyDown(e: KeyboardEvent<HTMLInputElement>) {
    if (e.key === "Enter" || e.key === ",") {
      e.preventDefault();
      addEqualLabelsFromInput();
    }
  }

  function submit() {
    const result = validateInhibitionDraft(draft);
    if (!result.ok) {
      const key = validationErrorKey(result.code);
      setError(result.params ? t(key, result.params) : t(key));
      return;
    }
    setError(null);
    mutation.mutate(result.input);
  }

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="max-h-[90vh] max-w-2xl overflow-y-auto" aria-describedby={undefined}>
        <DialogHeader>
          <DialogTitle className="flex items-center gap-1.5 pr-6">
            {editing ? t("alerts.editInhibition") : t("alerts.newInhibition")}
            <HelpTip content={t("alerts.inhibitionsSubtitle")} />
          </DialogTitle>
        </DialogHeader>

        <div className="space-y-5">
          <MatcherListEditor
            label={t("alerts.sourceMatchers")}
            hint={t("alerts.sourceMatchersHint")}
            matchers={draft.sourceMatchers}
            onChange={(sourceMatchers) =>
              setDraft((prev) => ({ ...prev, sourceMatchers }))
            }
          />

          <MatcherListEditor
            label={t("alerts.targetMatchers")}
            hint={t("alerts.targetMatchersHint")}
            matchers={draft.targetMatchers}
            onChange={(targetMatchers) =>
              setDraft((prev) => ({ ...prev, targetMatchers }))
            }
          />

          <div className="space-y-2">
            <div className="flex items-center gap-1.5">
              <Label htmlFor="inh-equal-label">{t("alerts.equalLabels")}</Label>
              <HelpTip content={t("alerts.equalLabelsHint")} />
            </div>
            {draft.equalLabels.length > 0 && (
              <div className="flex flex-wrap gap-1.5">
                {draft.equalLabels.map((label) => (
                  <span
                    key={label}
                    className="inline-flex items-center gap-1 rounded border border-border bg-muted/50 px-2 py-0.5 font-mono text-xs"
                  >
                    <span>{label}</span>
                    <button
                      type="button"
                      onClick={() =>
                        setDraft((prev) => ({
                          ...prev,
                          equalLabels: removeEqualLabel(prev.equalLabels, label),
                        }))
                      }
                      aria-label={t("common.remove")}
                      className="rounded p-0.5 text-muted-foreground hover:text-foreground"
                    >
                      <X className="size-3" />
                    </button>
                  </span>
                ))}
              </div>
            )}
            <div className="flex items-center gap-2">
              <Input
                id="inh-equal-label"
                value={draft.equalLabelInput}
                onChange={(e) =>
                  setDraft((prev) => ({ ...prev, equalLabelInput: e.target.value }))
                }
                onKeyDown={handleEqualLabelKeyDown}
                placeholder={t("alerts.equalLabelPlaceholder")}
                className="font-mono text-sm"
                spellCheck={false}
              />
              <Button
                type="button"
                variant="outline"
                size="sm"
                onClick={addEqualLabelsFromInput}
                disabled={!draft.equalLabelInput.trim()}
              >
                <Plus className="size-3.5" />
                {t("alerts.addEqualLabel")}
              </Button>
            </div>
          </div>

          <div className="space-y-1.5">
            <Label htmlFor="inh-comment">{t("alerts.comment")}</Label>
            <Textarea
              id="inh-comment"
              value={draft.comment}
              onChange={(e) =>
                setDraft((prev) => ({ ...prev, comment: e.target.value }))
              }
              placeholder={t("alerts.inhibitionCommentPlaceholder")}
              className="min-h-[60px]"
            />
          </div>

          <div className="flex items-center justify-between rounded-lg border border-border px-3 py-2">
            <Label htmlFor="inh-enabled" className="cursor-pointer">
              {t("alerts.enabledField")}
            </Label>
            <Switch
              id="inh-enabled"
              checked={draft.enabled}
              onCheckedChange={(enabled) =>
                setDraft((prev) => ({ ...prev, enabled }))
              }
            />
          </div>

          {error && (
            <p className="text-sm text-destructive" role="alert">
              {error}
            </p>
          )}
        </div>

        <DialogFooter>
          <Button
            variant="outline"
            onClick={() => onOpenChange(false)}
            disabled={mutation.isPending}
          >
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
