import { useState } from "react";
import { useTranslation } from "react-i18next";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { Laptop, ShieldCheck, X } from "lucide-react";
import { listMySessions, revokeSession, type SessionInfo } from "@/lib/api/auth";
import { formatDateTime } from "@/lib/format";
import dayjs from "@/lib/dayjs";
import { ConfirmDialog } from "@/components/common/confirm-dialog";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Skeleton } from "@/components/ui/skeleton";

const fmt = (iso: string) => formatDateTime(dayjs(iso).valueOf());

export function SessionsCard() {
  const { t } = useTranslation();
  const queryClient = useQueryClient();
  const [revoking, setRevoking] = useState<SessionInfo | null>(null);

  const sessions = useQuery({
    queryKey: ["auth", "sessions"],
    queryFn: ({ signal }) => listMySessions(signal),
  });

  return (
    <Card>
      <CardHeader>
        <CardTitle className="flex items-center gap-2">
          <ShieldCheck className="size-4" />
          {t("sessions.title")}
        </CardTitle>
        <CardDescription>{t("sessions.description")}</CardDescription>
      </CardHeader>
      <CardContent>
        {sessions.isLoading ? (
          <div className="space-y-2">
            <Skeleton className="h-10 w-full" />
            <Skeleton className="h-10 w-full" />
          </div>
        ) : sessions.data && sessions.data.length > 0 ? (
          <ul className="divide-y divide-border rounded-lg border border-border">
            {sessions.data.map((session) => (
              <li key={session.id} className="flex items-center gap-3 px-3 py-2.5">
                <Laptop className="size-4 shrink-0 text-muted-foreground" />
                <div className="min-w-0 flex-1">
                  <div className="flex items-center gap-2">
                    <span className="truncate text-sm font-medium">
                      {t("sessions.since", { when: fmt(session.created_at) })}
                    </span>
                    {session.current && (
                      <span className="rounded border border-primary/40 bg-primary/10 px-1.5 py-0.5 text-[11px] text-primary">
                        {t("sessions.current")}
                      </span>
                    )}
                  </div>
                  <p className="text-xs text-muted-foreground">
                    {t("sessions.expires", { when: fmt(session.expires_at) })}
                  </p>
                </div>
                {!session.current && (
                  <Button
                    variant="ghost"
                    size="icon"
                    aria-label={t("sessions.revoke")}
                    onClick={() => setRevoking(session)}
                  >
                    <X className="size-4 text-destructive" />
                  </Button>
                )}
              </li>
            ))}
          </ul>
        ) : (
          <p className="px-3 py-6 text-center text-sm text-muted-foreground">
            {t("sessions.empty")}
          </p>
        )}
      </CardContent>
      <ConfirmDialog
        open={!!revoking}
        onOpenChange={(open) => !open && setRevoking(null)}
        title={t("sessions.revokeConfirm")}
        description={t("sessions.revokeDescription")}
        confirmLabel={t("sessions.revoke")}
        onConfirm={async () => {
          if (!revoking) return;
          await revokeSession(revoking.id);
          await queryClient.invalidateQueries({ queryKey: ["auth", "sessions"] });
          setRevoking(null);
        }}
      />
    </Card>
  );
}
