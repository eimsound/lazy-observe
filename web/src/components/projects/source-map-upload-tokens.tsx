import { useState } from "react";
import { useTranslation } from "react-i18next";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Check, Copy, Loader2, Plus, Trash2 } from "lucide-react";
import {
  createSourceMapUploadToken,
  listSourceMapUploadTokens,
  revokeSourceMapUploadToken,
  type IssuedSourceMapUploadToken,
  type Project,
  type SourceMapUploadToken,
} from "@/lib/api/projects";
import { formatDateTime } from "@/lib/format";
import dayjs from "@/lib/dayjs";
import { projectDsn } from "@/lib/tokens/token-usage";
import { ConfirmDialog } from "@/components/common/confirm-dialog";
import { HelpTip } from "@/components/common/help-tip";
import { QueryErrorState } from "@/components/common/query-error-state";
import { QueryStaleNotice } from "@/components/common/query-stale-notice";
import { SnippetBlock } from "@/components/common/snippet-block";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Skeleton } from "@/components/ui/skeleton";
import { queryViewState } from "@/routes/query-view-state";

const fmt = (iso: string) => (iso ? formatDateTime(dayjs(iso).valueOf()) : "—");

/**
 * ADR 0018: the confidential, upload-only credential for the build pipeline.
 * Admin-only (the list endpoint is admin-only too); the value is shown once.
 */
export function SourceMapUploadTokens({ project }: { project: Project }) {
  const { t } = useTranslation();
  const queryClient = useQueryClient();
  const projectId = project.id;
  const queryKey = ["source-map-upload-tokens", projectId];

  const tokens = useQuery({
    queryKey,
    queryFn: ({ signal }) => listSourceMapUploadTokens(projectId, signal),
    retry: false,
  });

  const [name, setName] = useState("");
  const [created, setCreated] = useState<IssuedSourceMapUploadToken | null>(null);
  const [copied, setCopied] = useState(false);
  const [revoking, setRevoking] = useState<SourceMapUploadToken | null>(null);
  const [error, setError] = useState<string | null>(null);

  const issue = useMutation({
    mutationFn: () => createSourceMapUploadToken(projectId, name.trim()),
    onSuccess: (tok) => {
      setCreated(tok);
      setName("");
      setCopied(false);
      setError(null);
      void queryClient.invalidateQueries({ queryKey });
    },
    onError: (e) => setError((e as Error).message),
  });

  async function copy(value: string) {
    try {
      await navigator.clipboard.writeText(value);
      setCopied(true);
      setTimeout(() => setCopied(false), 1500);
    } catch {
      /* clipboard unavailable */
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
    <section className="space-y-3" aria-label={t("projects.smUploadTitle")}>
      <div className="flex items-center gap-1.5 border-t border-border pt-4">
        <h3 className="text-sm font-medium text-foreground">{t("projects.smUploadTitle")}</h3>
        <HelpTip
          scopeTitle={t("common.applicableScope")}
          scope={[{ text: t("projects.smUploadScopeTitle") }]}
        />
      </div>

      {created && (
        <div className="space-y-2 rounded-lg border border-primary/40 bg-primary/5 p-3">
          <div className="flex items-center gap-2 text-sm font-medium">
            <Check className="size-4 text-primary" />
            {t("projects.smUploadCreatedTitle")}
          </div>
          <p className="text-xs text-muted-foreground">{t("common.tokenCopyOnce")}</p>
          <div className="flex items-center gap-2">
            <code className="flex-1 truncate rounded border border-border bg-background px-2 py-1.5 font-mono text-xs">
              {created.value}
            </code>
            <Button size="sm" variant="outline" className="gap-1.5" onClick={() => copy(created.value)}>
              {copied ? <Check className="size-3.5" /> : <Copy className="size-3.5" />}
              {copied ? t("common.copied") : t("common.copy")}
            </Button>
          </div>
          <SnippetBlock
            label={t("projects.smUploadDsnLabel")}
            value={projectDsn(window.location.origin, project.slug, created.value)}
          />
        </div>
      )}

      <div className="flex items-end gap-2">
        <div className="flex-1 space-y-1.5">
          <Label htmlFor="sm-upload-name">{t("projects.smUploadName")}</Label>
          <Input
            id="sm-upload-name"
            value={name}
            onChange={(e) => setName(e.target.value)}
            placeholder={t("projects.smUploadNamePlaceholder")}
            onKeyDown={(e) => {
              if (e.key === "Enter" && name.trim()) issue.mutate();
            }}
          />
        </div>
        <Button onClick={() => issue.mutate()} disabled={!name.trim() || issue.isPending} className="gap-1.5">
          {issue.isPending ? <Loader2 className="size-4 animate-spin" /> : <Plus className="size-4" />}
          {t("projects.smUploadCreate")}
        </Button>
      </div>
      {error && (
        <p className="text-sm text-destructive" role="alert">
          {error}
        </p>
      )}

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
          </div>
        ) : viewState === "error" ? (
          <QueryErrorState error={tokens.error} onRetry={() => void tokens.refetch()} />
        ) : viewState === "empty" ? (
          <p className="px-3 py-6 text-center text-sm text-muted-foreground">{t("projects.smUploadNone")}</p>
        ) : (
          <table className="w-full text-sm">
            <thead>
              <tr className="border-b border-border text-left text-xs uppercase tracking-wider text-muted-foreground">
                <th className="px-3 py-2 font-medium">{t("projects.tokenColName")}</th>
                <th className="px-3 py-2 font-medium">{t("projects.tokenColPrefix")}</th>
                <th className="hidden px-3 py-2 font-medium sm:table-cell">
                  {t("projects.tokenColLastUsed")}
                </th>
                <th className="px-3 py-2" />
              </tr>
            </thead>
            <tbody>
              {rows.map((tok) => (
                <tr key={tok.id} className="border-b border-border/60 last:border-0">
                  <td className="px-3 py-2 font-medium">
                    <span className={tok.revoked ? "text-muted-foreground line-through" : ""}>
                      {tok.name}
                    </span>
                    {tok.revoked && (
                      <span className="ml-1.5 text-[11px] font-normal text-muted-foreground">
                        {t("projects.revoked")}
                      </span>
                    )}
                  </td>
                  <td className="px-3 py-2">
                    <span className="font-mono text-xs text-muted-foreground">{tok.prefix}…</span>
                  </td>
                  <td className="hidden px-3 py-2 text-xs text-muted-foreground sm:table-cell">
                    {tok.last_used_at ? fmt(tok.last_used_at) : t("projects.neverUsed")}
                  </td>
                  <td className="px-3 py-2 text-right">
                    <Button
                      variant="ghost"
                      size="icon"
                      aria-label={t("projects.smUploadRevoke")}
                      onClick={() => setRevoking(tok)}
                      disabled={tok.revoked}
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

      {revoking && (
        <ConfirmDialog
          key={revoking.id}
          open
          onOpenChange={(o) => !o && setRevoking(null)}
          title={t("projects.smUploadRevokeConfirm", { name: revoking.name })}
          description={t("projects.smUploadRevokeConfirmDesc")}
          confirmLabel={t("projects.smUploadRevoke")}
          onConfirm={async () => {
            await revokeSourceMapUploadToken(projectId, revoking.id);
            void queryClient.invalidateQueries({ queryKey });
            setRevoking(null);
          }}
        />
      )}
    </section>
  );
}
