import { useState } from "react";
import { useTranslation } from "react-i18next";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Check, Copy, KeyRound, Loader2, Plus, Trash2 } from "lucide-react";
import {
  createUserAPIToken,
  listUserAPITokens,
  revokeUserAPIToken,
  type IssuedUserAPIToken,
  type UserAPIToken,
} from "@/lib/api/user-api-tokens";
import { formatDateTime } from "@/lib/format";
import dayjs from "@/lib/dayjs";
import { useProject } from "@/lib/project/project-provider";
import {
  grafanaDataSourceUrl,
  grafanaHeaderBlock,
  mcpCommand,
} from "@/lib/tokens/token-usage";
import { ConfirmDialog } from "@/components/common/confirm-dialog";
import { HelpTip } from "@/components/common/help-tip";
import { QueryErrorState } from "@/components/common/query-error-state";
import { QueryStaleNotice } from "@/components/common/query-stale-notice";
import { SnippetBlock } from "@/components/common/snippet-block";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Skeleton } from "@/components/ui/skeleton";
import { queryViewState } from "@/routes/query-view-state";

const fmt = (iso: string) => formatDateTime(dayjs(iso).valueOf());

export function UserAPITokensCard() {
  const { t } = useTranslation();
  const queryClient = useQueryClient();
  const { currentSlug } = useProject();
  const origin = window.location.origin;
  const [name, setName] = useState("");
  const [created, setCreated] = useState<IssuedUserAPIToken | null>(null);
  const [copied, setCopied] = useState(false);
  const [revoking, setRevoking] = useState<UserAPIToken | null>(null);
  const [error, setError] = useState<string | null>(null);

  const tokens = useQuery({
    queryKey: ["user-api-tokens"],
    queryFn: ({ signal }) => listUserAPITokens(signal),
    retry: false,
  });
  const issue = useMutation({
    mutationFn: () => createUserAPIToken(name.trim()),
    onSuccess: (token) => {
      setCreated(token);
      setName("");
      setCopied(false);
      setError(null);
      void queryClient.invalidateQueries({ queryKey: ["user-api-tokens"] });
    },
    onError: (cause) => setError((cause as Error).message),
  });

  async function copy(value: string) {
    try {
      await navigator.clipboard.writeText(value);
      setCopied(true);
      setTimeout(() => setCopied(false), 1500);
    } catch {
      setError(t("userTokens.copyFailed"));
    }
  }

  const rows = tokens.data ?? [];
  const viewState = queryViewState({
    hasData: tokens.data !== undefined,
    isLoading: tokens.isLoading,
    isError: tokens.isError,
    isEmpty: rows.length === 0,
  });

  return (
    <Card>
      <CardHeader>
        <CardTitle className="flex items-center gap-2">
          <KeyRound className="size-4" />
          {t("userTokens.title")}
          <HelpTip
            scopeTitle={t("common.applicableScope")}
            scope={[
              {
                text: t("userTokens.scopeReadTitle"),
                children: [
                  t("userTokens.scopeQueryApi"),
                  t("userTokens.scopeMcp"),
                  t("userTokens.scopeGrafana"),
                ],
              },
            ]}
          />
        </CardTitle>
      </CardHeader>
      <CardContent className="space-y-4">
        {created && (
          <div className="space-y-2 rounded-lg border border-primary/40 bg-primary/5 p-3">
            <div className="flex items-center gap-2 text-sm font-medium">
              <Check className="size-4 text-primary" />
              {t("userTokens.createdTitle")}
            </div>
            <p className="text-xs text-muted-foreground">{t("common.tokenCopyOnce")}</p>
            <div className="flex items-center gap-2">
              <code className="flex-1 truncate rounded border border-border bg-background px-2 py-1.5 font-mono text-xs">
                {created.value}
              </code>
              <Button size="sm" variant="outline" onClick={() => copy(created.value)}>
                {copied ? <Check className="size-3.5" /> : <Copy className="size-3.5" />}
                {copied ? t("common.copied") : t("common.copy")}
              </Button>
            </div>

            {/* ADR 0010: a user API token is the read credential for MCP and Grafana. */}
            <div className="space-y-3 border-t border-primary/20 pt-3">
              <p className="text-xs font-medium">{t("userTokens.usageTitle")}</p>
              <SnippetBlock
                label={t("userTokens.mcpLabel")}
                value={mcpCommand(origin, currentSlug, created.value)}
              />
              <div className="space-y-2">
                <p className="text-xs font-medium">{t("userTokens.grafanaTitle")}</p>
                <SnippetBlock
                  label={t("userTokens.grafanaUrlLabel")}
                  value={grafanaDataSourceUrl(origin)}
                />
                <SnippetBlock
                  label={t("userTokens.grafanaHeadersLabel")}
                  value={grafanaHeaderBlock(created.value, currentSlug)}
                />
              </div>
            </div>
          </div>
        )}

        <div className="flex items-end gap-2">
          <div className="flex-1 space-y-1.5">
            <Label htmlFor="user-token-name">{t("userTokens.name")}</Label>
            <Input
              id="user-token-name"
              value={name}
              onChange={(event) => setName(event.target.value)}
              placeholder={t("userTokens.namePlaceholder")}
              onKeyDown={(event) => {
                if (event.key === "Enter" && name.trim()) issue.mutate();
              }}
            />
          </div>
          <Button onClick={() => issue.mutate()} disabled={!name.trim() || issue.isPending}>
            {issue.isPending ? <Loader2 className="size-4 animate-spin" /> : <Plus className="size-4" />}
            {t("userTokens.create")}
          </Button>
        </div>
        {error && <p className="text-sm text-destructive" role="alert">{error}</p>}

        {viewState === "stale" && (
          <QueryStaleNotice
            isRetrying={tokens.isFetching}
            onRetry={() => void tokens.refetch()}
          />
        )}

        <div className="rounded-lg border border-border">
          {viewState === "pending" ? (
            <div className="space-y-2 p-3">
              <Skeleton className="h-8 w-full" />
              <Skeleton className="h-8 w-full" />
            </div>
          ) : viewState === "error" ? (
            <QueryErrorState error={tokens.error} onRetry={() => void tokens.refetch()} />
          ) : viewState === "empty" ? (
            <p className="px-3 py-6 text-center text-sm text-muted-foreground">{t("userTokens.empty")}</p>
          ) : (
            <table className="w-full text-sm">
              <thead>
                <tr className="border-b border-border text-left text-xs uppercase tracking-wider text-muted-foreground">
                  <th className="px-3 py-2 font-medium">{t("userTokens.name")}</th>
                  <th className="px-3 py-2 font-medium">{t("userTokens.prefix")}</th>
                  <th className="hidden px-3 py-2 font-medium sm:table-cell">{t("userTokens.lastUsed")}</th>
                  <th className="px-3 py-2" />
                </tr>
              </thead>
              <tbody>
                {rows.map((token) => (
                  <tr key={token.id} className="border-b border-border/60 last:border-0">
                    <td className="px-3 py-2 font-medium">
                      <span className={token.revoked ? "text-muted-foreground line-through" : ""}>{token.name}</span>
                      {token.revoked && <span className="ml-1.5 text-xs text-muted-foreground">{t("userTokens.revoked")}</span>}
                    </td>
                    <td className="px-3 py-2 font-mono text-xs text-muted-foreground">{token.prefix}…</td>
                    <td className="hidden px-3 py-2 text-xs text-muted-foreground sm:table-cell">
                      {token.last_used_at ? fmt(token.last_used_at) : t("userTokens.neverUsed")}
                    </td>
                    <td className="px-3 py-2 text-right">
                      <Button
                        variant="ghost"
                        size="icon"
                        disabled={token.revoked}
                        aria-label={t("userTokens.revoke")}
                        onClick={() => setRevoking(token)}
                      >
                        <Trash2 className="size-4 text-destructive" />
                      </Button>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          )}
        </div>
      </CardContent>

      {revoking && (
        <ConfirmDialog
          key={revoking.id}
          open
          onOpenChange={(open) => !open && setRevoking(null)}
          title={t("userTokens.revokeConfirm", { name: revoking.name })}
          description={t("userTokens.revokeDescription")}
          confirmLabel={t("userTokens.revoke")}
          onConfirm={async () => {
            await revokeUserAPIToken(revoking.id);
            await queryClient.invalidateQueries({ queryKey: ["user-api-tokens"] });
            setRevoking(null);
          }}
        />
      )}
    </Card>
  );
}
