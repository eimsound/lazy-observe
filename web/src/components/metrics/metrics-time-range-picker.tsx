import { useId, useMemo, useState } from "react";
import { Check, ChevronLeft, ChevronRight, X } from "lucide-react";
import { useTranslation } from "react-i18next";
import {
  calendarCells,
  dateParts,
  formatPrometheusDuration,
  metricsRangeKey,
  metricsRangeSeconds,
  parseDateTime,
  type MetricsRange,
} from "@/lib/metrics-query";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Popover, PopoverContent, PopoverTrigger } from "@/components/ui/popover";
import { Switch } from "@/components/ui/switch";
import dayjs, { dayjsLocale } from "@/lib/dayjs";
import { cn } from "@/lib/utils";

interface MetricsTimeRangePickerProps {
  value: MetricsRange;
  localTime: boolean;
  triggerClassName?: string;
  onChange: (range: MetricsRange) => void;
  onLocalTimeChange: (value: boolean) => void;
}

interface MonthRef { year: number; month: number }

const DATE_TIME_FORMAT = "YYYY-MM-DD HH:mm:ss";

function formatDateTimeInput(ms: number, local: boolean): string {
  const parts = dateParts(ms, local);
  return `${parts.date} ${parts.time}`;
}

function parseDateTimeInput(value: string, local: boolean): number | null {
  const match = /^(\d{4}-\d{2}-\d{2})\s+(\d{2}:\d{2}:\d{2})$/.exec(value.trim());
  return match ? parseDateTime(match[1], match[2], local) : null;
}

function monthRef(ms: number, local: boolean): MonthRef {
  const value = local ? dayjs(ms) : dayjs.utc(ms);
  return { year: value.year(), month: value.month() };
}

function shiftMonth(value: MonthRef, delta: number): MonthRef {
  const shifted = dayjs.utc(`${value.year}-${value.month + 1}-1`, "YYYY-M-D", true).add(delta, "month");
  return { year: shifted.year(), month: shifted.month() };
}

function endLabel(value: MetricsRange, local: boolean, nowLabel: string) {
  if (value.mode === "relative" || value.endMs === undefined) return nowLabel;
  const end = dateParts(value.endMs, local);
  return `${end.date} ${end.time}`;
}

export function MetricsTimeRangePicker({
  value,
  localTime,
  triggerClassName,
  onChange,
  onLocalTimeChange,
}: MetricsTimeRangePickerProps) {
  const { t, i18n } = useTranslation();
  const localTimeId = useId();
  const [open, setOpen] = useState(false);
  const [selectedMs, setSelectedMs] = useState(() =>
    value.mode === "absolute" && value.endMs !== undefined ? value.endMs : dayjs().valueOf()
  );
  const [dateTimeInput, setDateTimeInput] = useState(() =>
    formatDateTimeInput(selectedMs, localTime)
  );
  const rangeKey = metricsRangeKey(value);
  const [viewMonth, setViewMonth] = useState<MonthRef>(() => monthRef(selectedMs, localTime));
  const [prevProps, setPrevProps] = useState({ rangeKey, localTime });

  if (prevProps.rangeKey !== rangeKey || prevProps.localTime !== localTime) {
    const rangeChanged = prevProps.rangeKey !== rangeKey;
    setPrevProps({ rangeKey, localTime });
    if (open) {
      const nextMs =
        rangeChanged && value.mode === "absolute" && value.endMs !== undefined
          ? value.endMs
          : selectedMs;
      if (nextMs !== selectedMs) {
        setSelectedMs(nextMs);
      }
      setDateTimeInput(formatDateTimeInput(nextMs, localTime));
      setViewMonth(monthRef(nextMs, localTime));
    }
  }

  function handleOpenChange(nextOpen: boolean) {
    setOpen(nextOpen);
    if (nextOpen) {
      const endMs = value.mode === "absolute" && value.endMs !== undefined
        ? value.endMs
        : dayjs().valueOf();
      setSelectedMs(endMs);
      setDateTimeInput(formatDateTimeInput(endMs, localTime));
      setViewMonth(monthRef(endMs, localTime));
    }
  }

  const timezone = dayjs.tz.guess() || t("metrics.localTime");
  const dateTimeInvalid = parseDateTimeInput(dateTimeInput, localTime) === null;

  function selectDay(dayMs: number) {
    const nextDate = dateParts(dayMs, localTime).date;
    const inputMs = parseDateTimeInput(dateTimeInput, localTime);
    const currentTime = dateParts(inputMs ?? selectedMs, localTime).time;
    const parsed = parseDateTime(nextDate, currentTime, localTime);
    if (parsed !== null) {
      setSelectedMs(parsed);
      setDateTimeInput(formatDateTimeInput(parsed, localTime));
    }
  }

  function commitDateTimeInput() {
    const parsed = parseDateTimeInput(dateTimeInput, localTime);
    if (parsed === null) {
      setDateTimeInput(formatDateTimeInput(selectedMs, localTime));
      return null;
    }
    setSelectedMs(parsed);
    setDateTimeInput(formatDateTimeInput(parsed, localTime));
    setViewMonth(monthRef(parsed, localTime));
    return parsed;
  }

  function clearDateTime() {
    const seconds = metricsRangeSeconds(value);
    onChange({ mode: "relative", presetId: formatPrometheusDuration(seconds), seconds });
    setOpen(false);
  }

  function apply() {
    const parsed = commitDateTimeInput();
    if (parsed === null) return;
    const seconds = metricsRangeSeconds(value);
    onChange({ mode: "absolute", startMs: parsed - seconds * 1000, endMs: parsed });
    setOpen(false);
  }

  return (
    <Popover open={open} onOpenChange={handleOpenChange}>
      <div className="relative">
        <PopoverTrigger asChild>
          <Button
            variant="outline"
            className={cn(
              "h-9 justify-center whitespace-nowrap font-normal",
              value.mode === "absolute" && value.endMs !== undefined && "pr-9",
              triggerClassName,
            )}
          >
            {endLabel(value, localTime, t("metrics.now"))}
          </Button>
        </PopoverTrigger>
        {value.mode === "absolute" && value.endMs !== undefined && (
          <Button
            type="button"
            variant="ghost"
            size="icon"
            className="absolute right-1 top-1/2 size-7 -translate-y-1/2"
            aria-label={t("common.clear")}
            onClick={clearDateTime}
          >
            <X className="size-3.5" />
          </Button>
        )}
      </div>
      <PopoverContent align="center" className="w-[360px] p-0">
        <div className="flex gap-2 px-4 pt-4">
          <Input
            type="text"
            autoComplete="off"
            value={dateTimeInput}
            placeholder={DATE_TIME_FORMAT}
            className="min-w-0 flex-1 font-mono"
            aria-label={t("metrics.dateTime")}
            aria-invalid={dateTimeInvalid}
            onChange={(event) => setDateTimeInput(event.target.value)}
            onBlur={commitDateTimeInput}
            onKeyDown={(event) => {
              if (event.key === "Enter") apply();
            }}
          />
          <Button
            variant="outline"
            size="icon"
            className="size-9 shrink-0"
            aria-label={t("common.apply")}
            disabled={dateTimeInvalid}
            onClick={apply}
          >
            <Check />
          </Button>
        </div>
        <MonthCalendar
          month={viewMonth}
          locale={i18n.language}
          localTime={localTime}
          selectedMs={selectedMs}
          onPrevious={() => setViewMonth((current) => shiftMonth(current, -1))}
          onNext={() => setViewMonth((current) => shiftMonth(current, 1))}
          onSelect={selectDay}
        />
        <div className="flex items-center justify-center gap-2 border-t border-border px-4 py-3">
          <span className={cn("text-xs", localTime ? "text-muted-foreground" : "font-medium text-foreground")}>UTC</span>
          <Switch id={localTimeId} checked={localTime} onCheckedChange={onLocalTimeChange} />
          <Label
            htmlFor={localTimeId}
            className={cn("cursor-pointer text-xs font-normal", localTime ? "font-medium text-foreground" : "text-muted-foreground")}
          >
            {t("metrics.useLocalTime")} ({timezone})
          </Label>
        </div>
      </PopoverContent>
    </Popover>
  );
}

function MonthCalendar({
  month,
  locale,
  localTime,
  selectedMs,
  onPrevious,
  onNext,
  onSelect,
}: {
  month: MonthRef;
  locale: string;
  localTime: boolean;
  selectedMs: number;
  onPrevious: () => void;
  onNext: () => void;
  onSelect: (ms: number) => void;
}) {
  const { t } = useTranslation();
  const allCells = useMemo(() => calendarCells(month.year, month.month, localTime), [month.year, month.month, localTime]);
  const cells = allCells.slice(35).every((cell) => cell.monthOffset === 1) ? allCells.slice(0, 35) : allCells;
  const title = useMemo(() => {
    const input = `${month.year}-${month.month + 1}-1`;
    const value = localTime
      ? dayjs(input, "YYYY-M-D", true)
      : dayjs.utc(input, "YYYY-M-D", true);
    const localeName = dayjsLocale(locale);
    return value.locale(localeName).format(
      localeName === "zh-cn" ? "YYYY年MMMM" : "MMMM YYYY",
    );
  }, [locale, localTime, month.month, month.year]);
  const weekdays = useMemo(() => Array.from({ length: 7 }, (_, index) =>
    dayjs.utc("2026-01-05").add(index, "day").locale(dayjsLocale(locale)).format("dd")
  ), [locale]);
  const selectedDate = dateParts(selectedMs, localTime).date;

  return (
    <div className="p-4">
      <div className="mb-3 grid grid-cols-[2rem_1fr_2rem] items-center">
        <Button variant="ghost" size="icon" className="size-8" aria-label={t("common.previousMonth")} onClick={onPrevious}>
          <ChevronLeft />
        </Button>
        <div className="text-center text-sm font-semibold">{title}</div>
        <Button variant="ghost" size="icon" className="size-8" aria-label={t("common.nextMonth")} onClick={onNext}>
          <ChevronRight />
        </Button>
      </div>
      <div className="grid grid-cols-7 text-center text-xs text-muted-foreground">
        {weekdays.map((weekday, index) => <div key={`${weekday}-${index}`} className="py-1.5">{weekday}</div>)}
      </div>
      <div className="grid grid-cols-7 gap-y-1">
        {cells.map((cell, index) => {
          const date = dateParts(cell.ms, localTime).date;
          const selected = date === selectedDate;
          const weekday = index % 7;
          return (
            <button
              key={cell.key}
              type="button"
              className={cn(
                "mx-auto size-9 rounded-md text-xs transition-colors hover:bg-accent",
                cell.monthOffset !== 0 && "text-muted-foreground/35",
                cell.monthOffset === 0 && weekday >= 5 && "text-destructive",
                selected && "bg-primary font-medium text-primary-foreground hover:bg-primary/90",
              )}
              onClick={() => onSelect(cell.ms)}
            >
              {cell.day}
            </button>
          );
        })}
      </div>
    </div>
  );
}
