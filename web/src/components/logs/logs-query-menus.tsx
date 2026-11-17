import { useMemo, useState } from "react";
import {
  BookOpen,
  Copy,
  Download,
  History,
  Play,
  Square,
  Star,
  Trash2,
} from "lucide-react";
import { useTranslation } from "react-i18next";
import dayjs, { dayjsLocale } from "@/lib/dayjs";
import {
  LOGS_QUERY_EXAMPLES,
  type LogsHistoryEntry,
} from "@/lib/logs-explorer";
import { Button } from "@/components/ui/button";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
  DialogTrigger,
} from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Popover, PopoverContent, PopoverTrigger } from "@/components/ui/popover";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { cn } from "@/lib/utils";

export function LogsHistoryMenu({
  history,
  onLoad,
  onRun,
  onFavorite,
  onRemove,
  onClear,
}: {
  history: LogsHistoryEntry[];
  onLoad: (query: string) => void;
  onRun: (query: string) => void;
  onFavorite: (query: string) => void;
  onRemove: (query: string) => void;
  onClear: () => void;
}) {
  const { t, i18n } = useTranslation();
  const [open, setOpen] = useState(false);
  const entries = useMemo(
    () => [...history].sort((left, right) => (
      Number(right.favorite) - Number(left.favorite) || right.lastRunAt - left.lastRunAt
    )),
    [history],
  );

  return (
    <Popover open={open} onOpenChange={setOpen}>
      <PopoverTrigger asChild>
        <Button variant="outline" size="sm" aria-label={t("logs.history")}>
          <History />
          {t("logs.history")}
          {history.length > 0 && (
            <span className="rounded-full bg-muted px-1.5 text-[10px] tabular-nums">
              {history.length}
            </span>
          )}
        </Button>
      </PopoverTrigger>
      <PopoverContent align="end" className="w-[min(92vw,32rem)] p-0">
        <div className="flex items-center justify-between gap-2 border-b border-border px-3 py-2">
          <span className="text-sm font-semibold">{t("logs.history")}</span>
          {history.length > 0 && (
            <Button variant="ghost" size="sm" className="h-7 text-xs" onClick={onClear}>
              <Trash2 />
              {t("logs.clearHistory")}
            </Button>
          )}
        </div>
        {entries.length === 0 ? (
          <p className="px-4 py-8 text-center text-sm text-muted-foreground">
            {t("logs.historyEmpty")}
          </p>
        ) : (
          <div className="max-h-[28rem] divide-y divide-border/60 overflow-auto">
            {entries.map((entry) => (
              <div key={entry.query} className="group flex items-start gap-1 p-2 hover:bg-muted/40">
                <Button
                  variant="ghost"
                  size="icon"
                  className={cn(
                    "mt-0.5 size-7 shrink-0",
                    entry.favorite && "text-amber-500",
                  )}
                  title={entry.favorite ? t("logs.unfavorite") : t("logs.favorite")}
                  aria-label={entry.favorite ? t("logs.unfavorite") : t("logs.favorite")}
                  onClick={() => onFavorite(entry.query)}
                >
                  <Star className={cn(entry.favorite && "fill-current")} />
                </Button>
                <button
                  type="button"
                  className="min-w-0 flex-1 rounded px-1 py-0.5 text-left hover:bg-accent"
                  title={entry.query}
                  onClick={() => {
                    onLoad(entry.query);
                    setOpen(false);
                  }}
                >
                  <span className="block whitespace-pre-wrap break-all font-mono text-xs">
                    {entry.query}
                  </span>
                  <span className="mt-1 block text-[10px] text-muted-foreground">
                    {dayjs(entry.lastRunAt)
                      .locale(dayjsLocale(i18n.language))
                      .format("YYYY-MM-DD HH:mm:ss")}
                  </span>
                </button>
                <HistoryAction
                  label={t("logs.runHistory")}
                  onClick={() => {
                    onRun(entry.query);
                    setOpen(false);
                  }}
                >
                  <Play />
                </HistoryAction>
                <HistoryAction
                  label={t("common.copy")}
                  onClick={() => void navigator.clipboard?.writeText(entry.query)}
                >
                  <Copy />
                </HistoryAction>
                <HistoryAction
                  label={t("common.delete")}
                  onClick={() => onRemove(entry.query)}
                >
                  <Trash2 />
                </HistoryAction>
              </div>
            ))}
          </div>
        )}
      </PopoverContent>
    </Popover>
  );
}

export function LogsExamplesMenu({
  onSelect,
}: {
  onSelect: (query: string) => void;
}) {
  const { t } = useTranslation();
  const [open, setOpen] = useState(false);
  return (
    <Popover open={open} onOpenChange={setOpen}>
      <PopoverTrigger asChild>
        <Button variant="outline" size="sm">
          <BookOpen />
          {t("logs.examples")}
        </Button>
      </PopoverTrigger>
      <PopoverContent align="end" className="w-[min(92vw,34rem)] p-0">
        <div className="border-b border-border px-4 py-3 text-sm font-semibold">
          {t("logs.queryExamples")}
        </div>
        <div className="max-h-[30rem] divide-y divide-border/60 overflow-auto">
          {LOGS_QUERY_EXAMPLES.map((example) => (
            <button
              key={example.id}
              type="button"
              className="block w-full space-y-1 px-4 py-3 text-left hover:bg-muted/50"
              onClick={() => {
                onSelect(example.query);
                setOpen(false);
              }}
            >
              <span className="block text-sm font-medium">{t(example.titleKey)}</span>
              <span className="block text-xs text-muted-foreground">
                {t(example.descriptionKey)}
              </span>
              <code className="block whitespace-pre-wrap break-all rounded bg-muted px-2 py-1.5 text-xs">
                {example.query}
              </code>
            </button>
          ))}
        </div>
      </PopoverContent>
    </Popover>
  );
}

export function LogsDownloadMenu({
  disabled,
  downloading,
  project,
  start,
  end,
  onCancel,
  onDownload,
}: {
  disabled?: boolean;
  downloading?: boolean;
  project: string;
  start?: number;
  end?: number;
  onCancel: () => void;
  onDownload: (format: "jsonl" | "csv", filename: string) => void;
}) {
  const { t } = useTranslation();
  const [open, setOpen] = useState(false);
  const [filename, setFilename] = useState("lazy-observe-logs");
  const [format, setFormat] = useState<"jsonl" | "csv">("jsonl");
  const period = start !== undefined && end !== undefined
    ? `${dayjs(start * 1_000).format("YYYY-MM-DD HH:mm:ss")} – ${
      dayjs(end * 1_000).format("YYYY-MM-DD HH:mm:ss")
    }`
    : "—";

  function submitDownload() {
    const safeFilename = filename
      .trim()
      .replace(/[\\/:*?"<>|]+/g, "-")
      .slice(0, 180) || "lazy-observe-logs";
    setOpen(false);
    onDownload(format, safeFilename);
  }

  if (downloading) {
    return (
      <Button type="button" variant="destructive" size="sm" onClick={onCancel}>
        <Square className="fill-current" />
        {t("logs.cancelDownload")}
      </Button>
    );
  }
  return (
    <Dialog open={open} onOpenChange={setOpen}>
      <DialogTrigger asChild>
        <Button variant="outline" size="sm" disabled={disabled}>
          <Download />
          {t("logs.download")}
        </Button>
      </DialogTrigger>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>{t("logs.downloadDialogTitle")}</DialogTitle>
          <DialogDescription>
            {t("logs.downloadScope", { project, period })}
          </DialogDescription>
        </DialogHeader>
        <div className="grid gap-4 sm:grid-cols-[minmax(0,1fr)_10rem]">
          <Label className="space-y-1.5 text-sm">
            <span>{t("logs.downloadFilename")}</span>
            <Input
              autoFocus
              value={filename}
              maxLength={180}
              onChange={(event) => setFilename(event.target.value)}
              onKeyDown={(event) => {
                if (event.key === "Enter") submitDownload();
              }}
            />
          </Label>
          <Label className="space-y-1.5 text-sm">
            <span>{t("logs.downloadFormat")}</span>
            <Select
              value={format}
              onValueChange={(value) => setFormat(value as "jsonl" | "csv")}
            >
              <SelectTrigger><SelectValue /></SelectTrigger>
              <SelectContent>
                <SelectItem value="jsonl">JSON Lines</SelectItem>
                <SelectItem value="csv">CSV</SelectItem>
              </SelectContent>
            </Select>
          </Label>
        </div>
        <DialogFooter>
          <Button type="button" variant="outline" onClick={() => setOpen(false)}>
            {t("common.cancel")}
          </Button>
          <Button type="button" onClick={submitDownload}>
            <Download />
            {t("logs.download")}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

function HistoryAction({
  label,
  children,
  onClick,
}: {
  label: string;
  children: React.ReactNode;
  onClick: () => void;
}) {
  return (
    <Button
      type="button"
      variant="ghost"
      size="icon"
      className="mt-0.5 size-7 shrink-0 opacity-70 sm:opacity-0 sm:group-hover:opacity-100 sm:focus-visible:opacity-100"
      title={label}
      aria-label={label}
      onClick={onClick}
    >
      {children}
    </Button>
  );
}
