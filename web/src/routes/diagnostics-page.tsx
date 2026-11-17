import { useQuery } from "@tanstack/react-query";
import { AlertCircle, Database, Loader2, RefreshCw, ShieldAlert, Stethoscope } from "lucide-react";
import { useTranslation } from "react-i18next";

import { EmptyState } from "@/components/common/empty-state";
import { PageActions, PageContainer } from "@/components/layout/page";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Skeleton } from "@/components/ui/skeleton";
import { useAuth } from "@/lib/auth/auth-provider";
import { canManageUsers } from "@/lib/auth/permissions";
import { formatBytes, formatDateTime } from "@/lib/format";
import { getHealthDiagnostics, getInboxDiagnostics } from "@/lib/api/system";

export function DiagnosticsPage() {
  const { t } = useTranslation();
  const { role } = useAuth();
  const enabled = canManageUsers(role);
  const health = useQuery({
    queryKey: ["system-diagnostics", "health"],
    queryFn: ({ signal }) => getHealthDiagnostics(signal),
    enabled,
    retry: false,
  });
  const inbox = useQuery({
    queryKey: ["system-diagnostics", "error-inbox"],
    queryFn: ({ signal }) => getInboxDiagnostics(signal),
    enabled,
    retry: false,
  });
  const refreshing = health.isFetching || inbox.isFetching;
  const refresh = () => {
    void health.refetch();
    void inbox.refetch();
  };

  if (!enabled) {
    return (
      <PageContainer>
        <Card>
          <CardContent>
            <EmptyState
              icon={<ShieldAlert className="size-6" />}
              title={t("diagnostics.forbiddenTitle")}
              desc={t("diagnostics.forbiddenDesc")}
            />
          </CardContent>
        </Card>
      </PageContainer>
    );
  }

  return (
    <PageContainer className="space-y-4">
      <PageActions>
        <Button variant="outline" size="sm" onClick={refresh} disabled={refreshing}>
          {refreshing ? <Loader2 className="animate-spin" /> : <RefreshCw />}
          {t("common.refresh")}
        </Button>
      </PageActions>

      <Card>
        <CardHeader className="pb-3">
          <CardTitle className="flex items-center gap-2 text-base">
            <Stethoscope className="size-4" />
            {t("diagnostics.componentsTitle")}
          </CardTitle>
        </CardHeader>
        <CardContent>
          {health.isLoading ? (
            <DiagnosticsSkeleton />
          ) : health.isError || !health.data ? (
            <DiagnosticsError onRetry={() => void health.refetch()} />
          ) : (
            <div className="grid gap-2 sm:grid-cols-2 lg:grid-cols-3">
              {health.data.components.map((component) => (
                <div
                  key={component.name}
                  className="flex items-center justify-between rounded-md border border-border px-3 py-2"
                >
                  <span className="font-mono text-sm">{component.name}</span>
                  <Badge variant={component.state === "ok" ? "secondary" : "destructive"}>
                    {t(`diagnostics.componentState.${component.state}`)}
                  </Badge>
                </div>
              ))}
            </div>
          )}
        </CardContent>
      </Card>

      <Card>
        <CardHeader className="pb-3">
          <CardTitle className="flex items-center gap-2 text-base">
            <Database className="size-4" />
            {t("diagnostics.inboxTitle")}
          </CardTitle>
        </CardHeader>
        <CardContent>
          {inbox.isLoading ? (
            <DiagnosticsSkeleton />
          ) : inbox.isError || !inbox.data ? (
            <DiagnosticsError onRetry={() => void inbox.refetch()} />
          ) : (
            <div className="grid gap-3 sm:grid-cols-2 lg:grid-cols-4">
              <DiagnosticValue label={t("diagnostics.activeItems")} value={String(inbox.data.activeItems)} />
              <DiagnosticValue label={t("diagnostics.activeBytes")} value={formatBytes(inbox.data.activeBytes)} />
              <DiagnosticValue label={t("diagnostics.pendingItems")} value={String(inbox.data.pendingItems)} />
              <DiagnosticValue label={t("diagnostics.processingItems")} value={String(inbox.data.processingItems)} />
              <DiagnosticValue label={t("diagnostics.retries")} value={String(inbox.data.retriesTotal)} />
              <DiagnosticValue label={t("diagnostics.rejections")} value={String(inbox.data.rejectionsTotal)} />
              <DiagnosticValue label={t("diagnostics.terminalFailures")} value={String(inbox.data.terminalFailuresTotal)} />
              <DiagnosticValue label={t("diagnostics.drops")} value={String(inbox.data.dropsTotal)} />
              <DiagnosticValue
                label={t("diagnostics.oldestAccepted")}
                value={inbox.data.oldestAcceptedAt
                  ? formatDateTime(Date.parse(inbox.data.oldestAcceptedAt))
                  : t("common.none")}
              />
            </div>
          )}
        </CardContent>
      </Card>
    </PageContainer>
  );
}

function DiagnosticValue({ label, value }: { label: string; value: string }) {
  return (
    <div className="rounded-md border border-border px-3 py-2">
      <div className="text-xs text-muted-foreground">{label}</div>
      <div className="mt-1 text-sm font-semibold tabular-nums">{value}</div>
    </div>
  );
}

function DiagnosticsSkeleton() {
  return (
    <div className="grid gap-2 sm:grid-cols-2 lg:grid-cols-4">
      {[0, 1, 2, 3].map((item) => <Skeleton key={item} className="h-14 w-full" />)}
    </div>
  );
}

function DiagnosticsError({ onRetry }: { onRetry: () => void }) {
  const { t } = useTranslation();
  return (
    <div className="flex flex-col items-center gap-2 py-8 text-center">
      <AlertCircle className="size-5 text-destructive" />
      <div className="text-sm font-medium">{t("diagnostics.errorTitle")}</div>
      <p className="text-xs text-muted-foreground">{t("common.queryErrorDesc")}</p>
      <Button variant="outline" size="sm" onClick={onRetry}>{t("common.retry")}</Button>
    </div>
  );
}
