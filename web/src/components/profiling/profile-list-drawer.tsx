import { useTranslation } from "react-i18next";
import { Link } from "@tanstack/react-router";
import { Download, FileArchive, Loader2, AlertCircle, Waypoints } from "lucide-react";
import { profileRawHref, type ProfileRecord } from "@/lib/api/profiles";
import { formatBytes, formatDateTime, shortId } from "@/lib/format";
import {
  Sheet,
  SheetContent,
  SheetDescription,
  SheetHeader,
  SheetTitle,
} from "@/components/ui/sheet";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { QueryStaleNotice } from "@/components/common/query-stale-notice";

/**
 * Drawer listing the raw profile records captured in the current window, each
 * with a download link to its pprof blob. Purely presentational — data is
 * fetched by the page and passed in.
 */
export function ProfileListDrawer({
  open,
  onOpenChange,
  records,
  isLoading,
  hasError,
  stale,
  isRetrying,
  onRetry,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  records: ProfileRecord[];
  isLoading: boolean;
  hasError: boolean;
  stale: boolean;
  isRetrying: boolean;
  onRetry: () => void;
}) {
  const { t } = useTranslation();
  return (
    <Sheet open={open} onOpenChange={onOpenChange}>
      <SheetContent side="right" className="w-full sm:max-w-lg">
        <SheetHeader>
          <SheetTitle>{t("profiling.profileListTitle")}</SheetTitle>
          <SheetDescription>{t("profiling.profileListDesc")}</SheetDescription>
        </SheetHeader>

        <div className="min-h-0 flex-1 overflow-y-auto p-4">
          {isLoading ? (
            <div className="flex items-center gap-2 py-10 text-sm text-muted-foreground">
              <Loader2 className="size-4 animate-spin" />
              {t("profiling.loading")}
            </div>
          ) : hasError ? (
            <div className="flex flex-col items-center gap-2 py-16 text-center text-sm text-muted-foreground">
              <AlertCircle className="size-6 text-destructive" />
              {t("common.queryErrorDesc")}
              <Button variant="outline" size="sm" onClick={onRetry}>
                {t("common.retry")}
              </Button>
            </div>
          ) : records.length === 0 ? (
            <div className="flex flex-col items-center gap-2 py-16 text-center">
              <FileArchive className="size-6 text-muted-foreground" />
              <div className="text-sm font-medium">{t("profiling.profileListEmpty")}</div>
            </div>
          ) : (
            <>
              {stale && (
                <QueryStaleNotice isRetrying={isRetrying} onRetry={onRetry} />
              )}
              <ul className="space-y-2">
                {records.map((rec) => (
                <li
                  key={rec.id}
                  className="flex items-center gap-3 rounded-md border border-border p-3"
                >
                  <FileArchive className="size-4 shrink-0 text-muted-foreground" />
                  <div className="min-w-0 flex-1">
                    <div className="flex items-center gap-2">
                      <span className="truncate text-sm font-medium tabular-nums">
                        {formatDateTime(rec.startMs)}
                      </span>
                      <Badge variant="secondary" className="shrink-0 uppercase">
                        {rec.type}
                      </Badge>
                    </div>
                    <div className="mt-0.5 flex flex-wrap items-center gap-3 text-xs text-muted-foreground">
                      <span className="truncate font-mono">{shortId(rec.id, 12)}</span>
                      <span className="tabular-nums">{formatBytes(rec.size)}</span>
                      {rec.traceId && (
                        <Link
                          to="/traces/$traceID"
                          params={{ traceID: rec.traceId }}
                          className="inline-flex items-center gap-1 font-medium text-primary hover:underline"
                        >
                          <Waypoints className="size-3" />
                          {t("logs.viewTrace")}
                        </Link>
                      )}
                    </div>
                  </div>
                  <a
                    href={profileRawHref(rec)}
                    download={`${rec.service}-${rec.type}-${shortId(rec.id)}.pprof`}
                    className="inline-flex shrink-0 items-center gap-1.5 rounded-md border border-border px-2.5 py-1.5 text-xs font-medium text-foreground hover:bg-accent"
                  >
                    <Download className="size-3.5" />
                    {t("profiling.download")}
                  </a>
                </li>
                ))}
              </ul>
            </>
          )}
        </div>
      </SheetContent>
    </Sheet>
  );
}
