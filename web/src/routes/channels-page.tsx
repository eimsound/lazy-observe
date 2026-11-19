import { useState } from "react";
import { useTranslation } from "react-i18next";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import {
  Check,
  Loader2,
  Pencil,
  Plus,
  Send,
  Trash2,
  X,
} from "lucide-react";
import {
  listChannels,
  createChannel,
  updateChannel,
  deleteChannel,
  testChannel,
  CHANNEL_TYPES,
  type NotificationChannel,
  type ChannelInput,
  type ChannelType,
  type ChannelTestResult,
} from "@/lib/api/alerts";
import { useAuth } from "@/lib/auth/auth-provider";
import { canManageAlerts } from "@/lib/auth/permissions";
import { PageActions, PageContainer } from "@/components/layout/page";
import { EmptyState } from "@/components/common/empty-state";
import { ConfirmDialog } from "@/components/common/confirm-dialog";
import { Card, CardContent } from "@/components/ui/card";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Badge } from "@/components/ui/badge";
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

/** Config field descriptors per channel type — labelKey + required + type. */
interface FieldSpec {
  key: string;
  labelKey: string;
  required?: boolean;
  password?: boolean;
}

const CHANNEL_FIELDS: Record<ChannelType, FieldSpec[]> = {
  webhook: [
    { key: "url", labelKey: "alerts.cfgUrl", required: true },
    { key: "secret", labelKey: "alerts.cfgSecret", password: true },
  ],
  feishu: [{ key: "url", labelKey: "alerts.cfgWebhookUrl", required: true }],
  dingtalk: [
    { key: "url", labelKey: "alerts.cfgWebhookUrl", required: true },
    { key: "secret", labelKey: "alerts.cfgSecret", password: true },
  ],
  wecom: [{ key: "url", labelKey: "alerts.cfgWebhookUrl", required: true }],
  slack: [{ key: "url", labelKey: "alerts.cfgWebhookUrl", required: true }],
  smtp: [
    { key: "to", labelKey: "alerts.cfgTo", required: true },
    { key: "from", labelKey: "alerts.cfgFrom", required: true },
    { key: "host", labelKey: "alerts.cfgSmtpHost", required: true },
    { key: "port", labelKey: "alerts.cfgSmtpPort", required: true },
    { key: "username", labelKey: "alerts.cfgUsername" },
    { key: "password", labelKey: "alerts.cfgPassword", password: true },
  ],
};

function typeLabelKey(type: ChannelType): string {
  if (type === "smtp") return "alerts.typeEmail";
  if (type === "slack") return "alerts.typeSlack";
  return `alerts.type${type.charAt(0).toUpperCase()}${type.slice(1)}`;
}

export function ChannelsPage() {
  const { t } = useTranslation();
  const { role } = useAuth();
  const canManage = canManageAlerts(role);
  const queryClient = useQueryClient();

  const channels = useQuery({ queryKey: ["channels"], queryFn: ({ signal }) => listChannels(signal) });

  const [formOpen, setFormOpen] = useState(false);
  const [editing, setEditing] = useState<NotificationChannel | null>(null);
  const [deleting, setDeleting] = useState<NotificationChannel | null>(null);
  const [testResults, setTestResults] = useState<Record<string, ChannelTestResult>>({});

  const test = useMutation({
    mutationFn: (id: string) => testChannel(id),
    onSuccess: (res, id) => {
      setTestResults((prev) => ({ ...prev, [id]: res }));
      setTimeout(() => setTestResults((prev) => {
        const next = { ...prev };
        delete next[id];
        return next;
      }), 4000);
    },
  });

  function openNew() {
    setEditing(null);
    setFormOpen(true);
  }
  function openEdit(ch: NotificationChannel) {
    setEditing(ch);
    setFormOpen(true);
  }

  return (
    <PageContainer>
      {canManage && (
        <PageActions>
          <Button onClick={openNew} className="gap-1.5">
            <Plus className="size-4" />
            {t("alerts.newChannel")}
          </Button>
        </PageActions>
      )}

      {channels.isLoading ? (
        <div className="grid gap-3 sm:grid-cols-2">
          {[0, 1, 2, 3].map((i) => (
            <Skeleton key={i} className="h-28 w-full" />
          ))}
        </div>
      ) : channels.data && channels.data.length > 0 ? (
        <div className="grid gap-3 sm:grid-cols-2">
          {channels.data.map((ch) => {
            const result = testResults[ch.id];
            const testing = test.isPending && test.variables === ch.id;
            return (
              <Card key={ch.id}>
                <CardContent className="flex flex-col gap-3 p-4">
                  <div className="flex items-start justify-between gap-2">
                    <div className="min-w-0">
                      <div className="flex items-center gap-2">
                        <span className="truncate font-medium">{ch.name}</span>
                        {!ch.enabled && (
                          <Badge variant="secondary" className="shrink-0 text-[11px]">
                            {t("common.disabled")}
                          </Badge>
                        )}
                      </div>
                      <Badge variant="outline" className="mt-1 text-[11px]">
                        {t(typeLabelKey(ch.type))}
                      </Badge>
                    </div>
                    {canManage && (
                      <div className="flex shrink-0 items-center gap-1">
                        <Button variant="ghost" size="icon" aria-label={t("common.edit")} onClick={() => openEdit(ch)}>
                          <Pencil className="size-4" />
                        </Button>
                        <Button
                          variant="ghost"
                          size="icon"
                          aria-label={t("alerts.deleteChannel")}
                          onClick={() => setDeleting(ch)}
                        >
                          <Trash2 className="size-4 text-destructive" />
                        </Button>
                      </div>
                    )}
                  </div>

                  <div className="truncate font-mono text-xs text-muted-foreground">
                    {Object.values(ch.config)[0] || "—"}
                  </div>

                  <div className="flex items-center gap-2">
                    <Button
                      variant="outline"
                      size="sm"
                      className="gap-1.5"
                      disabled={testing}
                      onClick={() => test.mutate(ch.id)}
                    >
                      {testing ? <Loader2 className="size-3.5 animate-spin" /> : <Send className="size-3.5" />}
                      {testing ? t("common.testing") : t("alerts.sendTest")}
                    </Button>
                    {result && (
                      <span
                        className={cn(
                          "inline-flex items-center gap-1 text-xs font-medium",
                          result.ok ? "text-level-info" : "text-destructive",
                        )}
                        role="status"
                      >
                        {result.ok ? <Check className="size-3.5" /> : <X className="size-3.5" />}
                        {result.ok ? t("alerts.testSuccess") : t("alerts.testFailed")}
                        <span className="font-normal text-muted-foreground">· {result.message}</span>
                      </span>
                    )}
                  </div>
                </CardContent>
              </Card>
            );
          })}
        </div>
      ) : (
        <Card>
          <CardContent>
            <EmptyState
              icon={<Send className="size-6" />}
              title={t("alerts.noChannels")}
              desc={t("alerts.noChannelsDesc")}
            />
          </CardContent>
        </Card>
      )}

      {formOpen && (
        <ChannelFormDialog open={formOpen} onOpenChange={setFormOpen} editing={editing} />
      )}
      <ConfirmDialog
        open={!!deleting}
        onOpenChange={(o) => !o && setDeleting(null)}
        title={t("alerts.deleteChannelConfirm", { name: deleting?.name ?? "" })}
        description={t("alerts.deleteChannelConfirmDesc")}
        confirmLabel={t("alerts.deleteChannel")}
        onConfirm={async () => {
          if (!deleting) return;
          await deleteChannel(deleting.id);
          void queryClient.invalidateQueries({ queryKey: ["channels"] });
          void queryClient.invalidateQueries({ queryKey: ["alert-rules"] });
          setDeleting(null);
        }}
      />
    </PageContainer>
  );
}

function ChannelFormDialog({
  open,
  onOpenChange,
  editing,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  editing: NotificationChannel | null;
}) {
  const { t } = useTranslation();
  const queryClient = useQueryClient();

  const [name, setName] = useState(editing?.name ?? "");
  const [type, setType] = useState<ChannelType>(editing?.type ?? "webhook");
  const [config, setConfig] = useState<Record<string, string>>(editing?.config ?? {});
  const [enabled, setEnabled] = useState<boolean>(editing?.enabled ?? true);
  const [error, setError] = useState<string | null>(null);

  const fields = CHANNEL_FIELDS[type];

  const mutation = useMutation({
    mutationFn: (input: ChannelInput) =>
      editing ? updateChannel(editing.id, input) : createChannel(input),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ["channels"] });
      onOpenChange(false);
    },
    onError: (e) => setError((e as Error).message || t("alerts.channelSaveError")),
  });

  function submit() {
    if (!name.trim()) return setError(t("alerts.channelNameRequired"));
    for (const f of fields) {
      if (f.required && !(config[f.key] ?? "").trim()) {
        return setError(t("alerts.configRequired"));
      }
    }
    setError(null);
    // Keep only fields relevant to the selected type.
    const cleanConfig: Record<string, string> = {};
    for (const f of fields) {
      const v = (config[f.key] ?? "").trim();
      if (v) cleanConfig[f.key] = v;
    }
    mutation.mutate({ name: name.trim(), type, config: cleanConfig, enabled });
  }

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="max-w-lg">
        <DialogHeader>
          <DialogTitle>{editing ? t("alerts.editChannel") : t("alerts.newChannel")}</DialogTitle>
        </DialogHeader>

        <div className="space-y-4">
          <div className="grid gap-4 sm:grid-cols-2">
            <div className="space-y-1.5">
              <Label htmlFor="c-name">{t("alerts.channelName")}</Label>
              <Input
                id="c-name"
                value={name}
                onChange={(e) => setName(e.target.value)}
                placeholder={t("alerts.channelNamePlaceholder")}
                autoFocus
              />
            </div>
            <div className="space-y-1.5">
              <Label>{t("alerts.channelType")}</Label>
              <Select value={type} onValueChange={(v) => setType(v as ChannelType)}>
                <SelectTrigger>
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  {CHANNEL_TYPES.map((ty) => (
                    <SelectItem key={ty} value={ty}>
                      {t(typeLabelKey(ty))}
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
            </div>
          </div>

          <div className="space-y-3 rounded-lg border border-border p-3">
            {fields.map((f) => (
              <div key={f.key} className="space-y-1.5">
                <Label htmlFor={`c-${f.key}`}>
                  {t(f.labelKey)}
                  {!f.required && (
                    <span className="ml-1 text-xs font-normal text-muted-foreground">
                      ({t("common.optional")})
                    </span>
                  )}
                </Label>
                <Input
                  id={`c-${f.key}`}
                  type={f.password ? "password" : "text"}
                  value={config[f.key] ?? ""}
                  onChange={(e) => setConfig((prev) => ({ ...prev, [f.key]: e.target.value }))}
                  className="font-mono text-sm"
                  spellCheck={false}
                  autoComplete="off"
                />
              </div>
            ))}
          </div>

          <div className="flex items-center justify-between rounded-lg border border-border px-3 py-2">
            <Label htmlFor="c-enabled" className="cursor-pointer">
              {t("common.enabled")}
            </Label>
            <Switch id="c-enabled" checked={enabled} onCheckedChange={setEnabled} />
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
