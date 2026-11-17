import { useMemo, useState } from "react";
import { useTranslation } from "react-i18next";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { BellOff, Loader2, Pencil, Plus, X } from "lucide-react";
import {
  listSilences,
  createSilence,
  updateSilence,
  deleteSilence,
  type Silence,
  type SilenceInput,
  type Matcher,
} from "@/lib/api/alerts";
import { useAuth } from "@/lib/auth/auth-provider";
import { canManageAlerts } from "@/lib/auth/permissions";
import { formatDateTime, formatDurationShort } from "@/lib/format";
import dayjs from "@/lib/dayjs";
import { PageActions, PageContainer } from "@/components/layout/page";
import { EmptyState } from "@/components/common/empty-state";
import { ConfirmDialog } from "@/components/common/confirm-dialog";
import { Card, CardContent } from "@/components/ui/card";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Textarea } from "@/components/ui/textarea";
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
  DialogHeader,
  DialogTitle,
  DialogFooter,
} from "@/components/ui/dialog";

/** ISO <-> <input type="datetime-local"> value, in the browser's local zone. */
function isoToLocalInput(iso: string): string {
  const value = dayjs(iso);
  return value.isValid() ? value.format("YYYY-MM-DDTHH:mm") : "";
}
function localInputToIso(value: string): string {
  const parsed = dayjs(value, "YYYY-MM-DDTHH:mm", true);
  return parsed.isValid() ? parsed.toISOString() : "";
}

const isActive = (s: Silence) => dayjs(s.ends_at).isAfter(dayjs());

export function SilencesPage() {
  const { t } = useTranslation();
  const { role } = useAuth();
  const canManage = canManageAlerts(role);
  const queryClient = useQueryClient();

  const silences = useQuery({ queryKey: ["silences"], queryFn: ({ signal }) => listSilences(signal) });

  const [formOpen, setFormOpen] = useState(false);
  const [editing, setEditing] = useState<Silence | null>(null);
  const [deleting, setDeleting] = useState<Silence | null>(null);

  const { active, expired } = useMemo(() => {
    const list = silences.data ?? [];
    return {
      active: list.filter(isActive),
      expired: list.filter((s) => !isActive(s)),
    };
  }, [silences.data]);

  function openNew() {
    setEditing(null);
    setFormOpen(true);
  }
  function openEdit(s: Silence) {
    setEditing(s);
    setFormOpen(true);
  }

  return (
    <PageContainer>
      {canManage && (
        <PageActions>
          <Button onClick={openNew} className="gap-1.5">
            <Plus className="size-4" />
            {t("alerts.newSilence")}
          </Button>
        </PageActions>
      )}

      {silences.isLoading ? (
        <div className="space-y-3">
          <Skeleton className="h-24 w-full" />
          <Skeleton className="h-24 w-full" />
        </div>
      ) : (silences.data?.length ?? 0) === 0 ? (
        <Card>
          <CardContent>
            <EmptyState
              icon={<BellOff className="size-6" />}
              title={t("alerts.noSilences")}
              desc={t("alerts.noSilencesDesc")}
            />
          </CardContent>
        </Card>
      ) : (
        <div className="space-y-6">
          <Section title={t("alerts.activeSection")} count={active.length}>
            {active.map((s) => (
              <SilenceCard
                key={s.id}
                silence={s}
                canManage={canManage}
                onEdit={() => openEdit(s)}
                onDelete={() => setDeleting(s)}
              />
            ))}
          </Section>
          {expired.length > 0 && (
            <Section title={t("alerts.expiredSection")} count={expired.length}>
              {expired.map((s) => (
                <SilenceCard
                  key={s.id}
                  silence={s}
                  canManage={canManage}
                  onEdit={() => openEdit(s)}
                  onDelete={() => setDeleting(s)}
                />
              ))}
            </Section>
          )}
        </div>
      )}

      {formOpen && (
        <SilenceFormDialog open={formOpen} onOpenChange={setFormOpen} editing={editing} />
      )}
      <ConfirmDialog
        open={!!deleting}
        onOpenChange={(o) => !o && setDeleting(null)}
        title={t("alerts.deleteSilenceConfirm")}
        description={t("alerts.deleteSilenceConfirmDesc")}
        confirmLabel={t("alerts.deleteSilence")}
        onConfirm={async () => {
          if (!deleting) return;
          await deleteSilence(deleting.id);
          queryClient.invalidateQueries({ queryKey: ["silences"] });
          setDeleting(null);
        }}
      />
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

function SilenceCard({
  silence,
  canManage,
  onEdit,
  onDelete,
}: {
  silence: Silence;
  canManage: boolean;
  onEdit: () => void;
  onDelete: () => void;
}) {
  const { t } = useTranslation();
  const active = isActive(silence);
  const now = dayjs();
  const endsAt = dayjs(silence.ends_at);
  const remaining = active
    ? t("alerts.expiresIn", { duration: formatDurationShort(endsAt.diff(now)) })
    : t("alerts.endedAgo", { duration: formatDurationShort(now.diff(endsAt)) });

  return (
    <Card className={active ? undefined : "opacity-70"}>
      <CardContent className="flex flex-col gap-3 p-4 sm:flex-row sm:items-start sm:justify-between">
        <div className="min-w-0 space-y-2">
          <div className="flex flex-wrap gap-1.5">
            {silence.matchers.map((m, i) => (
              <span
                key={i}
                className="inline-flex items-center gap-1 rounded border border-border bg-muted/40 px-1.5 py-0.5 font-mono text-xs"
              >
                <span className="text-foreground/80">{m.name}</span>
                <span className="text-muted-foreground">{m.op}</span>
                <span>{m.value}</span>
              </span>
            ))}
          </div>
          {silence.comment && <p className="text-sm">{silence.comment}</p>}
          <div className="flex flex-wrap items-center gap-x-3 gap-y-1 text-xs text-muted-foreground">
            <span>{formatDateTime(dayjs(silence.starts_at).valueOf())}</span>
            <span>→</span>
            <span>{formatDateTime(dayjs(silence.ends_at).valueOf())}</span>
            <span
              className={
                active
                  ? "rounded border border-level-info/30 bg-level-info/10 px-1.5 py-0.5 text-level-info"
                  : "rounded border border-border px-1.5 py-0.5"
              }
            >
              {remaining}
            </span>
            {silence.creator && (
              <span>
                {t("alerts.createdBy")}: {silence.creator}
              </span>
            )}
          </div>
        </div>
        {canManage && (
          <div className="flex shrink-0 items-center gap-1">
            <Button variant="ghost" size="icon" aria-label={t("common.edit")} onClick={onEdit}>
              <Pencil className="size-4" />
            </Button>
            <Button
              variant="ghost"
              size="icon"
              aria-label={t("alerts.deleteSilence")}
              onClick={onDelete}
            >
              <X className="size-4 text-destructive" />
            </Button>
          </div>
        )}
      </CardContent>
    </Card>
  );
}

function SilenceFormDialog({
  open,
  onOpenChange,
  editing,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  editing: Silence | null;
}) {
  const { t } = useTranslation();
  const queryClient = useQueryClient();

  const [matchers, setMatchers] = useState<Matcher[]>(
    editing?.matchers.length ? editing.matchers : [{ name: "", op: "=", value: "" }],
  );
  const [startsAt, setStartsAt] = useState(
    isoToLocalInput(editing?.starts_at ?? dayjs().toISOString()),
  );
  const [endsAt, setEndsAt] = useState(
    isoToLocalInput(editing?.ends_at ?? dayjs().add(2, "hour").toISOString()),
  );
  const [comment, setComment] = useState(editing?.comment ?? "");
  const [error, setError] = useState<string | null>(null);

  const mutation = useMutation({
    mutationFn: (input: SilenceInput) =>
      editing ? updateSilence(editing.id, input) : createSilence(input),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ["silences"] });
      onOpenChange(false);
    },
    onError: (e) => setError((e as Error).message || t("alerts.silenceSaveError")),
  });

  function updateMatcher(i: number, patch: Partial<Matcher>) {
    setMatchers((prev) => prev.map((m, idx) => (idx === i ? { ...m, ...patch } : m)));
  }

  function submit() {
    const cleaned = matchers.filter((m) => m.name.trim() && m.value.trim());
    if (cleaned.length === 0) return setError(t("alerts.matchersRequired"));
    const startIso = localInputToIso(startsAt);
    const endIso = localInputToIso(endsAt);
    if (!startIso || !endIso || !dayjs(endIso).isAfter(dayjs(startIso))) {
      return setError(t("alerts.endAfterStart"));
    }
    setError(null);
    mutation.mutate({
      matchers: cleaned.map((m) => ({ name: m.name.trim(), op: m.op, value: m.value.trim() })),
      starts_at: startIso,
      ends_at: endIso,
      comment: comment.trim(),
    });
  }

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="max-w-xl">
        <DialogHeader>
          <DialogTitle>{editing ? t("alerts.editSilence") : t("alerts.newSilence")}</DialogTitle>
        </DialogHeader>

        <div className="space-y-4">
          <div className="space-y-2">
            <Label>{t("alerts.matchers")}</Label>
            {matchers.map((m, i) => (
              <div key={i} className="flex items-center gap-2">
                <Input
                  value={m.name}
                  onChange={(e) => updateMatcher(i, { name: e.target.value })}
                  placeholder={t("alerts.matcherName")}
                  className="font-mono text-sm"
                  spellCheck={false}
                />
                <Select value={m.op} onValueChange={(v) => updateMatcher(i, { op: v as Matcher["op"] })}>
                  <SelectTrigger className="w-28 shrink-0">
                    <SelectValue />
                  </SelectTrigger>
                  <SelectContent>
                    <SelectItem value="=">{t("alerts.opEquals")}</SelectItem>
                    <SelectItem value="=~">{t("alerts.opRegex")}</SelectItem>
                  </SelectContent>
                </Select>
                <Input
                  value={m.value}
                  onChange={(e) => updateMatcher(i, { value: e.target.value })}
                  placeholder={t("alerts.matcherValue")}
                  className="font-mono text-sm"
                  spellCheck={false}
                />
                <Button
                  type="button"
                  variant="ghost"
                  size="icon"
                  onClick={() => setMatchers((prev) => prev.filter((_, idx) => idx !== i))}
                  disabled={matchers.length === 1}
                  aria-label={t("common.remove")}
                >
                  <X className="size-4" />
                </Button>
              </div>
            ))}
            <Button
              type="button"
              variant="outline"
              size="sm"
              className="gap-1.5"
              onClick={() => setMatchers((prev) => [...prev, { name: "", op: "=", value: "" }])}
            >
              <Plus className="size-3.5" />
              {t("alerts.addMatcher")}
            </Button>
          </div>

          <div className="grid gap-4 sm:grid-cols-2">
            <div className="space-y-1.5">
              <Label htmlFor="s-start">{t("alerts.startsAt")}</Label>
              <Input
                id="s-start"
                type="datetime-local"
                value={startsAt}
                onChange={(e) => setStartsAt(e.target.value)}
              />
            </div>
            <div className="space-y-1.5">
              <Label htmlFor="s-end">{t("alerts.endsAt")}</Label>
              <Input
                id="s-end"
                type="datetime-local"
                value={endsAt}
                onChange={(e) => setEndsAt(e.target.value)}
              />
            </div>
          </div>

          <div className="space-y-1.5">
            <Label htmlFor="s-comment">{t("alerts.comment")}</Label>
            <Textarea
              id="s-comment"
              value={comment}
              onChange={(e) => setComment(e.target.value)}
              placeholder={t("alerts.commentPlaceholder")}
              className="min-h-[60px]"
            />
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
