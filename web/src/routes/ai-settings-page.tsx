import { useEffect, useState } from "react";
import { useTranslation } from "react-i18next";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import {
  AlertCircle,
  CheckCircle2,
  Info,
  KeyRound,
  Loader2,
  Lock,
  PlugZap,
  Save,
  Sparkles,
} from "lucide-react";
import {
  getAiSettings,
  getAiStatus,
  providerDefaultBaseUrl,
  testAi,
  updateAiSettings,
  AI_PROVIDERS,
  type AiTestResult,
} from "@/lib/api/ai";
import { ApiError } from "@/lib/api/client";
import { IS_MOCK } from "@/lib/settings";
import { useAuth } from "@/lib/auth/auth-provider";
import { canManageUsers } from "@/lib/auth/permissions";
import {
  INITIAL_AI_SETTINGS_FORM,
  applySavedAiSettings,
  buildAiSettingsUpdatePayload,
  patchAiDraft,
  requestClearAiApiKey,
  setAiApiKeyDraft,
  syncServerAiSettings,
  type AiSettingsFormState,
} from "./ai-settings-state";
import { PageContainer } from "@/components/layout/page";
import { EmptyState } from "@/components/common/empty-state";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import { Button } from "@/components/ui/button";
import { Skeleton } from "@/components/ui/skeleton";
import { Switch } from "@/components/ui/switch";
import { Label } from "@/components/ui/label";
import { Input } from "@/components/ui/input";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";

/**
 * AI settings (/settings/ai, admin). Administrators can enable AI, pick a
 * provider (OpenAI / DeepSeek / Anthropic / Ollama, all OpenAI-compatible
 * where applicable), optionally override the base URL, set the model, and
 * store the BYO API key. The key is sealed server-side, never returned to the
 * browser (only `has_api_key` is reported), and the provider client hot-swaps
 * without a restart. A "test connection" button verifies the live endpoint.
 */
export function AiSettingsPage() {
  const { t } = useTranslation();
  const { role } = useAuth();
  const queryClient = useQueryClient();

  const statusQuery = useQuery({
    queryKey: ["ai-status"],
    queryFn: ({ signal }) => getAiStatus(signal),
  });
  const settingsQuery = useQuery({
    queryKey: ["ai-settings"],
    queryFn: ({ signal }) => getAiSettings(signal),
    enabled: canManageUsers(role),
    refetchOnWindowFocus: false,
  });

  const [form, setForm] = useState<AiSettingsFormState>(INITIAL_AI_SETTINGS_FORM);
  const [testResult, setTestResult] = useState<AiTestResult | null>(null);

  const { draft, apiKeyDraft, clearKey } = form;

  // Seed the form from server data only when the user has no unsaved edits.
  useEffect(() => {
    if (settingsQuery.data) {
      setForm((prev) => syncServerAiSettings(prev, settingsQuery.data));
    }
  }, [settingsQuery.data]);

  const save = useMutation({
    mutationFn: () => {
      if (!form.draft) throw new ApiError("no settings", 400);
      return updateAiSettings(buildAiSettingsUpdatePayload(form));
    },
    onSuccess: (saved) => {
      setForm(applySavedAiSettings(saved));
      setTestResult(null);
      queryClient.setQueryData(["ai-settings"], saved);
      void queryClient.invalidateQueries({ queryKey: ["ai-status"] });
    },
  });

  const test = useMutation({
    mutationFn: () => testAi(),
    onMutate: () => setTestResult(null),
    onSuccess: (r) => setTestResult(r),
    onError: () => setTestResult(null),
  });

  if (!canManageUsers(role)) {
    return (
      <PageContainer className="max-w-3xl">
        <Card>
          <CardContent className="py-16">
            <EmptyState
              icon={<Lock className="size-6" />}
              title={t("aiSettings.forbiddenTitle")}
              desc={t("aiSettings.forbiddenDesc")}
            />
          </CardContent>
        </Card>
      </PageContainer>
    );
  }

  const status = statusQuery.data;
  const enabled = status?.enabled ?? draft?.enabled ?? false;
  const settingsLoading = settingsQuery.isLoading && !draft;

  function setProvider(provider: string) {
    if (!draft) return;
    // When the base URL is empty or still the previous provider's default,
    // switch it to the newly selected provider's default.
    const previousDefault = providerDefaultBaseUrl(draft.provider);
    const base_url =
      draft.base_url === "" || draft.base_url === previousDefault
        ? providerDefaultBaseUrl(provider)
        : draft.base_url;
    setForm((prev) => patchAiDraft(prev, { provider, base_url }));
  }

  return (
    <PageContainer className="max-w-3xl space-y-6">

      {/* Status */}
      <Card>
        <CardHeader>
          <CardTitle className="flex items-center gap-2">
            <Sparkles className="size-4" />
            {t("aiSettings.statusSection")}
          </CardTitle>
          <CardDescription>{t("aiSettings.subtitle")}</CardDescription>
        </CardHeader>
        <CardContent className="space-y-4">
          {statusQuery.isLoading ? (
            <div className="space-y-2">
              <Skeleton className="h-5 w-40" />
              <Skeleton className="h-5 w-56" />
            </div>
          ) : statusQuery.error ? (
            <div className="flex items-center gap-2 text-sm text-destructive">
              <AlertCircle className="size-4" />
              {statusQuery.error instanceof ApiError
                ? statusQuery.error.message
                : t("common.loadFailed")}
            </div>
          ) : (
            <dl className="grid grid-cols-1 gap-3 sm:grid-cols-3">
              <Field label={t("aiSettings.statusLabel")}>
                <span
                  className={
                    "inline-flex items-center gap-1.5 rounded-md border px-2 py-0.5 text-xs font-medium " +
                    (enabled
                      ? "border-vital-good/30 bg-vital-good/10 text-vital-good"
                      : "border-border bg-muted/50 text-muted-foreground")
                  }
                >
                  <span
                    className={
                      "inline-block size-1.5 rounded-full " +
                      (enabled ? "bg-vital-good" : "bg-muted-foreground")
                    }
                    aria-hidden
                  />
                  {enabled ? t("aiSettings.enabled") : t("aiSettings.disabled")}
                </span>
              </Field>
              <Field label={t("aiSettings.provider")}>
                <span className="text-sm text-foreground">
                  {status?.provider || t("aiSettings.notConfigured")}
                </span>
              </Field>
              <Field label={t("aiSettings.model")}>
                <span className="font-mono text-sm text-foreground">
                  {status?.model || t("aiSettings.notConfigured")}
                </span>
              </Field>
            </dl>
          )}

          {enabled && (
            <div className="space-y-2 border-t border-border pt-4">
              <Button onClick={() => test.mutate()} disabled={test.isPending}>
                {test.isPending ? (
                  <Loader2 className="size-4 animate-spin" />
                ) : (
                  <PlugZap className="size-4" />
                )}
                {test.isPending ? t("aiSettings.testing") : t("aiSettings.testConnection")}
              </Button>
              {test.isError && (
                <div className="flex items-center gap-2 text-sm text-destructive">
                  <AlertCircle className="size-4" />
                  {test.error instanceof ApiError ? test.error.message : t("aiSettings.testError")}
                </div>
              )}
              {testResult && (
                <div className="flex flex-wrap items-center gap-x-4 gap-y-1 rounded-md border border-vital-good/30 bg-vital-good/10 px-3 py-2 text-sm">
                  <span className="flex items-center gap-1.5 font-medium text-vital-good">
                    <CheckCircle2 className="size-4" />
                    {t("aiSettings.testSuccess")}
                  </span>
                  <span className="tabular-nums text-foreground/90">
                    {t("aiSettings.testLatency", { ms: testResult.latency_ms })}
                  </span>
                  <span className="font-mono text-xs text-muted-foreground">
                    {t("aiSettings.testEcho", { echo: testResult.echo })}
                  </span>
                </div>
              )}
            </div>
          )}
        </CardContent>
      </Card>

      {/* Configuration (admin) */}
      <Card>
        <CardHeader>
          <CardTitle className="flex items-center gap-2">
            <KeyRound className="size-4" />
            {t("aiSettings.configSection")}
          </CardTitle>
          <CardDescription>{t("aiSettings.configDesc")}</CardDescription>
        </CardHeader>
        <CardContent className="space-y-4">
          {settingsLoading ? (
            <div className="space-y-2">
              <Skeleton className="h-9 w-full" />
              <Skeleton className="h-9 w-full" />
              <Skeleton className="h-9 w-full" />
            </div>
          ) : settingsQuery.error ? (
            <div className="flex items-center gap-2 text-sm text-destructive">
              <AlertCircle className="size-4" />
              {settingsQuery.error instanceof ApiError
                ? settingsQuery.error.message
                : t("common.loadFailed")}
            </div>
          ) : draft ? (
            <>
              <div className="flex items-center justify-between gap-4">
                <div className="space-y-1">
                  <Label htmlFor="ai-enabled">{t("aiSettings.enableLabel")}</Label>
                  <p className="text-xs text-muted-foreground">{t("aiSettings.enableHint")}</p>
                </div>
                <Switch
                  id="ai-enabled"
                  checked={draft.enabled}
                  onCheckedChange={(on) => setForm((prev) => patchAiDraft(prev, { enabled: on }))}
                />
              </div>

              <div className="grid grid-cols-1 gap-4 sm:grid-cols-2">
                <div className="space-y-1">
                  <Label htmlFor="ai-provider">{t("aiSettings.provider")}</Label>
                  <Select value={draft.provider || undefined} onValueChange={setProvider}>
                    <SelectTrigger id="ai-provider">
                      <SelectValue placeholder={t("aiSettings.providerPlaceholder")} />
                    </SelectTrigger>
                    <SelectContent>
                      {AI_PROVIDERS.map((p) => (
                        <SelectItem key={p.id} value={p.id}>
                          {p.label}
                        </SelectItem>
                      ))}
                    </SelectContent>
                  </Select>
                </div>
                <div className="space-y-1">
                  <Label htmlFor="ai-model">{t("aiSettings.model")}</Label>
                  <Input
                    id="ai-model"
                    value={draft.model}
                    placeholder={draft.provider === "deepseek" ? "deepseek-chat" : "gpt-4o-mini"}
                    onChange={(e) => setForm((prev) => patchAiDraft(prev, { model: e.target.value }))}
                  />
                </div>
              </div>

              <div className="space-y-1">
                <Label htmlFor="ai-base-url">{t("aiSettings.baseUrl")}</Label>
                <Input
                  id="ai-base-url"
                  value={draft.base_url}
                  placeholder={providerDefaultBaseUrl(draft.provider) || "https://…"}
                  onChange={(e) => setForm((prev) => patchAiDraft(prev, { base_url: e.target.value }))}
                />
                <p className="text-xs text-muted-foreground">{t("aiSettings.baseUrlHint")}</p>
              </div>

              <div className="grid grid-cols-1 gap-4 sm:grid-cols-2">
                <div className="space-y-1">
                  <Label htmlFor="ai-key">{t("aiSettings.apiKey")}</Label>
                  <Input
                    id="ai-key"
                    type="password"
                    autoComplete="new-password"
                    value={apiKeyDraft}
                    placeholder={
                      draft.has_api_key
                        ? t("aiSettings.keyStoredPlaceholder")
                        : t("aiSettings.keyEmptyPlaceholder")
                    }
                    onChange={(e) => setForm((prev) => setAiApiKeyDraft(prev, e.target.value))}
                  />
                </div>
                <div className="space-y-1">
                  <Label htmlFor="ai-max-tokens">{t("aiSettings.maxTokens")}</Label>
                  <Input
                    id="ai-max-tokens"
                    type="number"
                    min={1}
                    value={draft.max_tokens || 1024}
                    onChange={(e) =>
                      setForm((prev) =>
                        patchAiDraft(prev, { max_tokens: Number(e.target.value) || 0 }),
                      )
                    }
                  />
                </div>
              </div>

              <div className="flex flex-wrap items-center gap-2 pt-2">
                <Button
                  onClick={() => save.mutate()}
                  disabled={save.isPending}
                  className="gap-2"
                >
                  {save.isPending ? (
                    <Loader2 className="size-4 animate-spin" />
                  ) : (
                    <Save className="size-4" />
                  )}
                  {t("aiSettings.save")}
                </Button>
                {draft.has_api_key && !clearKey && (
                  <Button
                    variant="outline"
                    disabled={save.isPending}
                    onClick={() => setForm((prev) => requestClearAiApiKey(prev))}
                  >
                    {t("aiSettings.clearKey")}
                  </Button>
                )}
                {clearKey && (
                  <span className="text-xs text-muted-foreground">{t("aiSettings.clearKeyHint")}</span>
                )}
              </div>
              {save.isError && (
                <div className="flex items-center gap-2 text-sm text-destructive">
                  <AlertCircle className="size-4" />
                  {save.error instanceof ApiError ? save.error.message : t("aiSettings.saveError")}
                </div>
              )}
              {save.isSuccess && (
                <div className="flex items-center gap-2 text-sm text-vital-good">
                  <CheckCircle2 className="size-4" />
                  {t("aiSettings.saveSuccess")}
                </div>
              )}
            </>
          ) : null}
        </CardContent>
      </Card>

      {/* Disabled guidance */}
      {!statusQuery.isLoading && !enabled && (
        <Card>
          <CardHeader>
            <CardTitle className="flex items-center gap-2">
              <Info className="size-4" />
              {t("aiSettings.disabledTitle")}
            </CardTitle>
            <CardDescription>{t("aiSettings.disabledDesc")}</CardDescription>
          </CardHeader>
          <CardContent className="space-y-3 text-sm text-foreground/90">
            <p>{t("aiSettings.enableSteps")}</p>
            <p className="flex items-start gap-2 rounded-md border border-border bg-muted/40 px-3 py-2 text-xs text-muted-foreground">
              <Lock className="mt-0.5 size-3.5 shrink-0" />
              {t("aiSettings.privacyNote")}
            </p>
          </CardContent>
        </Card>
      )}

      {/* Mock-only note (acceptance testing of the disabled guidance) */}
      {IS_MOCK && (
        <Card>
          <CardHeader>
            <CardTitle className="text-sm">{t("aiSettings.mockToggleTitle")}</CardTitle>
            <CardDescription>{t("aiSettings.mockToggleDesc")}</CardDescription>
          </CardHeader>
        </Card>
      )}
    </PageContainer>
  );
}

function Field({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <div className="space-y-1">
      <dt className="text-xs text-muted-foreground">{label}</dt>
      <dd>{children}</dd>
    </div>
  );
}
