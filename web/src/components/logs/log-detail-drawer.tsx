import { useTranslation } from "react-i18next";
import { Copy, Filter, Waypoints } from "lucide-react";
import type { LogRow } from "@/lib/api/logs";
import { formatDateTime } from "@/lib/format";
import {
  Sheet,
  SheetContent,
  SheetHeader,
  SheetTitle,
  SheetDescription,
} from "@/components/ui/sheet";
import { Tabs, TabsList, TabsTrigger, TabsContent } from "@/components/ui/tabs";
import { Button } from "@/components/ui/button";
import { LevelBadge } from "./level-indicator";

/**
 * Right-side drawer showing every field of a log line. Two views: a key/value
 * table (each value can be added to the query as a filter or copied) and the
 * raw JSON. When the line carries a trace_id, a jump-to-trace action is shown.
 */

interface LogDetailDrawerProps {
  row: LogRow | null;
  open: boolean;
  onOpenChange: (open: boolean) => void;
  /** Append `field:"value"` to the query bar. */
  onAddFilter: (field: string, value: string) => void;
  /** Navigate to the trace waterfall for this line's trace_id. */
  onViewTrace: (traceId: string) => void;
}

function stringify(value: unknown): string {
  if (value === null || value === undefined) return "";
  if (typeof value === "object") return JSON.stringify(value);
  return String(value);
}

export function LogDetailDrawer({
  row,
  open,
  onOpenChange,
  onAddFilter,
  onViewTrace,
}: LogDetailDrawerProps) {
  const { t } = useTranslation();

  return (
    <Sheet open={open} onOpenChange={onOpenChange}>
      <SheetContent className="w-full sm:max-w-xl" hideClose>
        {row && (
          <>
            <SheetHeader>
              <div className="flex items-start justify-between gap-3 pr-2">
                <div className="min-w-0 space-y-1">
                  <SheetTitle className="flex items-center gap-2">
                    <LevelBadge level={row.level} />
                    <span className="truncate text-sm text-muted-foreground">
                      {formatDateTime(row.time)}
                    </span>
                  </SheetTitle>
                  <SheetDescription className="break-words font-mono text-xs">
                    {row.message}
                  </SheetDescription>
                </div>
                {row.traceId && (
                  <Button
                    size="sm"
                    variant="outline"
                    className="shrink-0"
                    onClick={() => onViewTrace(row.traceId!)}
                  >
                    <Waypoints className="size-4" />
                    {t("logs.viewTrace")}
                  </Button>
                )}
              </div>
            </SheetHeader>

            <Tabs defaultValue="fields" className="flex min-h-0 flex-1 flex-col px-4 pb-4">
              <TabsList className="w-fit">
                <TabsTrigger value="fields">{t("logs.tabFields")}</TabsTrigger>
                <TabsTrigger value="raw">{t("logs.tabRaw")}</TabsTrigger>
              </TabsList>

              <TabsContent value="fields" className="min-h-0 flex-1 overflow-auto">
                <table className="w-full border-collapse text-xs">
                  <thead className="sticky top-0 bg-background">
                    <tr className="border-b border-border text-left text-muted-foreground">
                      <th className="w-[34%] py-1.5 pr-2 font-medium">{t("logs.fieldCol")}</th>
                      <th className="py-1.5 font-medium">{t("logs.valueCol")}</th>
                    </tr>
                  </thead>
                  <tbody>
                    {Object.entries(row.raw).map(([key, value]) => {
                      const v = stringify(value);
                      return (
                        <tr
                          key={key}
                          className="group border-b border-border/60 align-top hover:bg-muted/40"
                        >
                          <td className="py-1.5 pr-2 font-mono text-muted-foreground">
                            {key}
                          </td>
                          <td className="py-1.5">
                            <div className="flex items-start justify-between gap-2">
                              <span className="break-all font-mono text-foreground">{v}</span>
                              <span className="flex shrink-0 gap-0.5 opacity-0 transition-opacity group-hover:opacity-100">
                                <button
                                  type="button"
                                  title={t("logs.addFilter")}
                                  aria-label={t("logs.addFilter")}
                                  className="rounded p-1 text-muted-foreground hover:bg-accent hover:text-accent-foreground"
                                  onClick={() => onAddFilter(key, v)}
                                >
                                  <Filter className="size-3.5" />
                                </button>
                                <button
                                  type="button"
                                  title={t("common.copy")}
                                  aria-label={t("common.copy")}
                                  className="rounded p-1 text-muted-foreground hover:bg-accent hover:text-accent-foreground"
                                  onClick={() => void navigator.clipboard?.writeText(v)}
                                >
                                  <Copy className="size-3.5" />
                                </button>
                              </span>
                            </div>
                          </td>
                        </tr>
                      );
                    })}
                  </tbody>
                </table>
              </TabsContent>

              <TabsContent value="raw" className="min-h-0 flex-1 overflow-auto">
                <pre className="rounded-md border border-border bg-muted/40 p-3 font-mono text-xs leading-relaxed text-foreground">
                  {JSON.stringify(row.raw, null, 2)}
                </pre>
              </TabsContent>
            </Tabs>
          </>
        )}
      </SheetContent>
    </Sheet>
  );
}
