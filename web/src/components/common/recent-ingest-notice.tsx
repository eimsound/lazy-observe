import { Info, Loader2, RefreshCw } from "lucide-react";
import { useTranslation } from "react-i18next";

import { Button } from "@/components/ui/button";
import { cn } from "@/lib/utils";

export interface RecentIngestNoticeProps {
  title?: string;
  description?: string;
  onRefresh?: () => void;
  isRefreshing?: boolean;
  className?: string;
}

export function RecentIngestNotice({
  title,
  description,
  onRefresh,
  isRefreshing = false,
  className,
}: RecentIngestNoticeProps) {
  const { t } = useTranslation();

  return (
    <div
      data-testid="recent-ingest-notice"
      role="status"
      className={cn(
        "mb-3 flex flex-wrap items-center justify-between gap-3 rounded-md border border-level-info/30 bg-level-info/5 px-3 py-2",
        className,
      )}
    >
      <div className="flex min-w-0 items-start gap-2">
        <Info className="mt-0.5 size-4 shrink-0 text-level-info" />
        <div>
          <div className="text-sm font-medium">{title ?? t("common.recentIngestTitle")}</div>
          <p className="text-xs text-muted-foreground">
            {description ?? t("common.recentIngestDesc")}
          </p>
        </div>
      </div>
      {onRefresh && (
        <Button variant="outline" size="sm" onClick={onRefresh} disabled={isRefreshing}>
          {isRefreshing ? <Loader2 className="animate-spin" /> : <RefreshCw />}
          {t("common.refresh")}
        </Button>
      )}
    </div>
  );
}
