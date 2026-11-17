import { useEffect, useRef } from "react";
import {
  AlertTriangle,
  CirclePause,
  CirclePlay,
  Radio,
  Trash2,
} from "lucide-react";
import { useTranslation } from "react-i18next";
import type { LogRow } from "@/lib/api/logs";
import { formatDateTime } from "@/lib/format";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { Switch } from "@/components/ui/switch";
import { LevelDot } from "./level-indicator";
import { cn } from "@/lib/utils";

export function LogsLiveView({
  rows,
  stopped,
  following,
  connected,
  reconnecting,
  rawJson,
  offset,
  limit,
  rate,
  dropped,
  error,
  onStoppedChange,
  onFollowingChange,
  onRawJsonChange,
  onOffsetChange,
  onLimitChange,
  onRetry,
  onClear,
  onOpenRow,
}: {
  rows: LogRow[];
  stopped: boolean;
  following: boolean;
  connected: boolean;
  reconnecting: boolean;
  rawJson: boolean;
  offset: string;
  limit: number;
  rate: number;
  dropped: number;
  error?: string;
  onStoppedChange: (stopped: boolean) => void;
  onFollowingChange: (following: boolean) => void;
  onRawJsonChange: (rawJson: boolean) => void;
  onOffsetChange: (offset: string) => void;
  onLimitChange: (limit: number) => void;
  onRetry: () => void;
  onClear: () => void;
  onOpenRow: (row: LogRow) => void;
}) {
  const { t } = useTranslation();
  const viewportRef = useRef<HTMLDivElement>(null);

  useEffect(() => {
    if (stopped || !following || !viewportRef.current) return;
    viewportRef.current.scrollTop = viewportRef.current.scrollHeight;
  }, [following, rows, stopped]);

  return (
    <div className="overflow-hidden rounded-md border border-border">
      <div className="flex flex-wrap items-center justify-between gap-3 border-b border-border bg-muted/20 px-3 py-2">
        <div className="flex flex-wrap items-center gap-2">
          <Button
            type="button"
            variant={stopped ? "default" : "outline"}
            size="sm"
            onClick={() => onStoppedChange(!stopped)}
          >
            {stopped ? <CirclePlay /> : <CirclePause />}
            {stopped ? t("logs.resumeLive") : t("logs.pauseLive")}
          </Button>
          <Button type="button" variant="outline" size="sm" onClick={onClear}>
            <Trash2 />
            {t("common.clear")}
          </Button>
          <span
            className={cn(
              "inline-flex items-center gap-1.5 text-xs",
              connected ? "text-emerald-600" : "text-muted-foreground",
            )}
          >
            <Radio className={cn("size-3.5", connected && !stopped && "animate-pulse")} />
            {stopped
              ? t("logs.livePaused")
              : connected
                ? t("logs.liveConnected")
                : reconnecting
                  ? t("logs.liveReconnecting")
                  : t("logs.liveConnecting")}
          </span>
          <span className="text-xs tabular-nums text-muted-foreground">
            {t("logs.liveRate", { count: rate })}
          </span>
        </div>
        <div className="flex flex-wrap items-center gap-3">
          <Label className="flex items-center gap-2 text-xs font-normal">
            {t("logs.rawJson")}
            <Switch checked={rawJson} onCheckedChange={onRawJsonChange} />
          </Label>
          <Label className="flex items-center gap-2 text-xs font-normal">
            {t("logs.liveOffset")}
            <Select
              value={offset}
              onValueChange={onOffsetChange}
            >
              <SelectTrigger className="h-8 w-20">
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                {[5, 10, 15, 30, 45, 60].map((seconds) => (
                  <SelectItem key={seconds} value={`${seconds}s`}>
                    {seconds}s
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
          </Label>
          <Label className="flex items-center gap-2 text-xs font-normal">
            {t("logs.liveLimit")}
            <Input
              type="number"
              min={10}
              max={50_000}
              value={limit}
              className="h-8 w-24"
              onChange={(event) => onLimitChange(
                Math.max(10, Math.min(50_000, Number(event.target.value) || 100)),
              )}
            />
          </Label>
        </div>
      </div>

      {(dropped > 0 || rate > 1_000) && (
        <div className="flex items-start gap-2 border-b border-amber-500/40 bg-amber-500/10 px-3 py-2 text-xs">
          <AlertTriangle className="mt-0.5 size-4 shrink-0 text-amber-600" />
          <span>
            {t("logs.liveHighVolume", { rate, dropped })}
          </span>
        </div>
      )}
      {error && (
        <div className="flex items-center justify-between gap-3 border-b border-destructive/40 bg-destructive/5 px-3 py-2 text-xs text-destructive">
          <span>{error}</span>
          <Button type="button" variant="outline" size="sm" onClick={onRetry}>
            {t("common.retry")}
          </Button>
        </div>
      )}

      <div
        ref={viewportRef}
        aria-label={t("logs.liveViewport")}
        className="h-[560px] overflow-auto bg-background font-mono text-xs"
        onScroll={(event) => {
          if (stopped) return;
          const target = event.currentTarget;
          const distance = target.scrollHeight - target.scrollTop - target.clientHeight;
          if (distance > 96 && following) onFollowingChange(false);
          else if (distance <= 16 && !following) onFollowingChange(true);
        }}
      >
        {rows.length === 0 ? (
          <div className="flex h-full items-center justify-center text-muted-foreground">
            {stopped ? t("logs.livePausedEmpty") : t("logs.waitingLive")}
          </div>
        ) : (
          rows.map((row) => (
            <button
              key={row.id}
              type="button"
              className="flex w-full items-start gap-2 border-b border-border/50 px-3 py-1.5 text-left hover:bg-muted/40"
              onClick={() => onOpenRow(row)}
            >
              {rawJson ? (
                <span className="min-w-0 break-all whitespace-pre-wrap">
                  {JSON.stringify(row.raw)}
                </span>
              ) : (
                <>
                  <span className="w-[148px] shrink-0 tabular-nums text-muted-foreground">
                    {formatDateTime(row.time)}
                  </span>
                  <LevelDot level={row.level} />
                  <span className="hidden w-40 shrink-0 truncate text-muted-foreground md:inline">
                    {row.stream}
                  </span>
                  <span className="min-w-0 flex-1 whitespace-pre-wrap break-words">
                    {row.message}
                  </span>
                </>
              )}
            </button>
          ))
        )}
      </div>
    </div>
  );
}
