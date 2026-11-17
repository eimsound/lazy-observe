import { AlertCircle, Loader2, RefreshCw } from "lucide-react";
import { useTranslation } from "react-i18next";

import { Button } from "@/components/ui/button";

export function QueryStaleNotice({
  onRetry,
  isRetrying = false,
}: {
  onRetry: () => void;
  isRetrying?: boolean;
}) {
  const { t } = useTranslation();

  return (
    <div
      data-testid="query-stale-notice"
      role="status"
      className="mb-3 flex flex-wrap items-center justify-between gap-3 rounded-md border border-amber-500/30 bg-amber-500/5 px-3 py-2"
    >
      <div className="flex min-w-0 items-start gap-2">
        <AlertCircle className="mt-0.5 size-4 shrink-0 text-amber-600" />
        <div>
          <div className="text-sm font-medium">{t("common.staleDataTitle")}</div>
          <p className="text-xs text-muted-foreground">{t("common.staleDataDesc")}</p>
        </div>
      </div>
      <Button variant="outline" size="sm" onClick={onRetry} disabled={isRetrying}>
        {isRetrying ? <Loader2 className="animate-spin" /> : <RefreshCw />}
        {t("common.retry")}
      </Button>
    </div>
  );
}
