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
import { ConfirmDialog } from "@/components/common/confirm-dialog";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Skeleton } from "@/components/ui/skeleton";

const fmt = (iso: string) => formatDateTime(dayjs(iso).valueOf());

export function UserAPITokensCard() {
  const { t } = useTranslation();
  const queryClient = useQueryClient();
  const [name, setName] = useState("");
  const [created, setCreated] = useState<IssuedUserAPIToken | null>(null);
  const [copied, setCopied] = useState(false);
  const [revoking, setRevoking] = useState<UserAPIToken | null>(null);
  const [error, setError] = useState<string | null>(null);

  const tokens = useQuery({
    queryKey: ["user-api-tokens"],
    queryFn: ({ signal }) => listUserAPITokens(signal),
  });
  const issue = useMutation({
    mutationFn: () => createUserAPIToken(name.trim()),
    onSuccess: (token) => {
      setCreated(token);
      setName("");
      setCopied(false);
      setError(null);
      queryClient.invalidateQueries({ queryKey: ["user-api-tokens"] });
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

  return (
    <Card>
      <CardHeader>
        <CardTitle className="flex items-center gap-2">
          <KeyRound className="size-4" />
          {t("userTokens.title")}
        </CardTitle>
        <CardDescription>{t("userTokens.description")}</CardDescription>
      </CardHeader>
      <CardContent className="space-y-4">
        {created && (
          <div className="space-y-2 rounded-lg border border-primary/40 bg-primary/5 p-3">
            <div className="flex items-center gap-2 text-sm font-medium">
              <Check className="size-4 text-primary" />
              {t("userTokens.createdTitle")}
            </div>
            <p className="text-xs text-muted-foreground">{t("userTokens.createdDescription")}</p>
            <div className="flex items-center gap-2">
              <code className="flex-1 truncate rounded border border-border bg-background px-2 py-1.5 font-mono text-xs">
                {created.value}
              </code>
              <Button size="sm" variant="outline" onClick={() => copy(created.value)}>
                {copied ? <Check className="size-3.5" /> : <Copy className="size-3.5" />}
                {copied ? t("common.copied") : t("common.copy")}
              </Button>
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
        {error && <p className="text-sm text-destructive">{error}</p>}

        <div className="rounded-lg border border-border">
          {tokens.isLoading ? (
            <div className="space-y-2 p-3">
              <Skeleton className="h-8 w-full" />
              <Skeleton className="h-8 w-full" />
            </div>
          ) : tokens.data && tokens.data.length > 0 ? (
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
                {tokens.data.map((token) => (
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
          ) : (
            <p className="px-3 py-6 text-center text-sm text-muted-foreground">{t("userTokens.empty")}</p>
          )}
        </div>
      </CardContent>

      <ConfirmDialog
        open={!!revoking}
        onOpenChange={(open) => !open && setRevoking(null)}
        title={t("userTokens.revokeConfirm", { name: revoking?.name ?? "" })}
        description={t("userTokens.revokeDescription")}
        confirmLabel={t("userTokens.revoke")}
        onConfirm={async () => {
          if (!revoking) return;
          await revokeUserAPIToken(revoking.id);
          await queryClient.invalidateQueries({ queryKey: ["user-api-tokens"] });
          setRevoking(null);
        }}
      />
    </Card>
  );
}
