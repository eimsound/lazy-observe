import { useState } from "react";
import {
  BarChart3,
  Check,
  ChevronDown,
  ChevronLeft,
  ChevronRight,
  Minus,
  Plus,
  RefreshCw,
  Settings2,
} from "lucide-react";
import { useTranslation } from "react-i18next";
import {
  formatPrometheusDuration,
  metricsRangeKey,
  metricsRangeSeconds,
  parsePrometheusDuration,
  resolveRangeKeyedDraft,
  setMetricsRangeSeconds,
  shiftMetricsRange,
  stepMetricsRange,
  type MetricsRange,
  type RangeKeyedDraft,
} from "@/lib/metrics-query";
import {
  LOGS_REFRESH_OPTIONS,
  MAX_LOGS_LIMIT,
  type LogsGraphMode,
} from "@/lib/logs-explorer";
import { HelpTip } from "@/components/common/help-tip";
import { MetricsTimeRangePicker } from "@/components/metrics/metrics-time-range-picker";
import { Button } from "@/components/ui/button";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
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
import { Switch } from "@/components/ui/switch";
import { cn } from "@/lib/utils";

export function LogsTimeControls({
  range,
  localTime,
  refreshSeconds,
  disabled,
  onRangeChange,
  onLocalTimeChange,
  onRefreshSecondsChange,
  onRefresh,
}: {
  range: MetricsRange;
  localTime: boolean;
  refreshSeconds: number;
  disabled?: boolean;
  onRangeChange: (range: MetricsRange) => void;
  onLocalTimeChange: (localTime: boolean) => void;
  onRefreshSecondsChange: (seconds: number) => void;
  onRefresh: () => void;
}) {
  const { t } = useTranslation();
  const rangeKey = metricsRangeKey(range);
  const formattedDuration = formatPrometheusDuration(metricsRangeSeconds(range));
  const [durationDraft, setDurationDraft] = useState<RangeKeyedDraft | null>(null);
  const durationInput = resolveRangeKeyedDraft(durationDraft, range, formattedDuration);

  const parsedDuration = parsePrometheusDuration(durationInput);

  function commitDuration() {
    setDurationDraft(null);
    if (parsedDuration === null) {
      return;
    }
    onRangeChange(setMetricsRangeSeconds(range, parsedDuration));
  }

  return (
    <div className="flex flex-wrap items-center gap-2">
      <div className="flex items-center">
        <Button
          type="button"
          variant="outline"
          size="icon"
          className="size-9 rounded-r-none"
          aria-label={t("logs.previousRange")}
          disabled={disabled}
          onClick={() => onRangeChange(shiftMetricsRange(range, -1))}
        >
          <ChevronLeft />
        </Button>
        <MetricsTimeRangePicker
          value={range}
          localTime={localTime}
          triggerClassName="rounded-none border-x-0"
          onChange={onRangeChange}
          onLocalTimeChange={onLocalTimeChange}
        />
        <Button
          type="button"
          variant="outline"
          size="icon"
          className="size-9 rounded-l-none"
          aria-label={t("logs.nextRange")}
          disabled={disabled}
          onClick={() => onRangeChange(shiftMetricsRange(range, 1))}
        >
          <ChevronRight />
        </Button>
      </div>

      <div className="flex items-center">
        <Button
          type="button"
          variant="outline"
          size="icon"
          className="size-9 rounded-r-none"
          aria-label={t("logs.shorterRange")}
          disabled={disabled}
          onClick={() => onRangeChange(stepMetricsRange(range, -1))}
        >
          <Minus />
        </Button>
        <Input
          className={cn(
            "h-9 w-20 rounded-none px-2 text-center font-mono",
            parsedDuration === null && "border-destructive text-destructive",
          )}
          value={durationInput}
          aria-label={t("logs.duration")}
          aria-invalid={parsedDuration === null}
          disabled={disabled}
          onChange={(event) => setDurationDraft({ rangeKey, value: event.target.value })}
          onBlur={commitDuration}
          onKeyDown={(event) => {
            if (event.key === "Enter") {
              event.currentTarget.blur();
            }
          }}
        />
        <Button
          type="button"
          variant="outline"
          size="icon"
          className="size-9 rounded-l-none"
          aria-label={t("logs.longerRange")}
          disabled={disabled}
          onClick={() => onRangeChange(stepMetricsRange(range, 1))}
        >
          <Plus />
        </Button>
      </div>

      <div className="flex items-center">
        <Button
          type="button"
          variant="outline"
          size="sm"
          className="h-9 gap-1.5 rounded-r-none text-sm"
          aria-label={t("logs.refreshNow")}
          disabled={disabled}
          onClick={onRefresh}
        >
          <RefreshCw />
          {t("common.refresh")}
        </Button>
        <DropdownMenu>
          <DropdownMenuTrigger asChild>
            <Button
              type="button"
              variant="outline"
              size="sm"
              className="-ml-px h-9 gap-1 rounded-l-none px-2.5 text-sm tabular-nums"
              aria-label={t("logs.autoRefresh")}
              disabled={disabled}
            >
              {refreshSeconds === 0
                ? t("logs.refreshOff")
                : formatPrometheusDuration(refreshSeconds)}
              <ChevronDown className="size-4 opacity-60" />
            </Button>
          </DropdownMenuTrigger>
          <DropdownMenuContent align="end" className="min-w-[8rem]">
            {LOGS_REFRESH_OPTIONS.map((seconds) => (
              <DropdownMenuItem
                key={seconds}
                onSelect={() => onRefreshSecondsChange(seconds)}
              >
                {seconds === 0
                  ? t("logs.refreshOff")
                  : formatPrometheusDuration(seconds)}
                {seconds === refreshSeconds && <Check className="ml-auto size-4" />}
              </DropdownMenuItem>
            ))}
          </DropdownMenuContent>
        </DropdownMenu>
      </div>
    </div>
  );
}

export function LogsLimitInput({
  value,
  disabled,
  onChange,
}: {
  value: number;
  disabled?: boolean;
  onChange: (value: number) => void;
}) {
  const { t } = useTranslation();
  const invalid = !Number.isInteger(value) || value < 1 || value > MAX_LOGS_LIMIT;
  return (
    <div className="flex items-center gap-2">
      <Label htmlFor="logs-limit" className="whitespace-nowrap text-xs text-muted-foreground">
        {t("logs.limit")}
      </Label>
      <Input
        id="logs-limit"
        type="number"
        inputMode="numeric"
        min={1}
        max={MAX_LOGS_LIMIT}
        step={1}
        value={value}
        disabled={disabled}
        aria-invalid={invalid}
        className={cn("h-9 w-24", invalid && "border-destructive text-destructive")}
        onChange={(event) => onChange(Number(event.target.value))}
      />
    </div>
  );
}

export interface LogsChartOptionsValue {
  graphMode: LogsGraphMode;
  groupBy: string;
  topN: number;
  step: string;
  stacked: boolean;
  cumulative: boolean;
  hidden: boolean;
}

export function LogsChartOptions({
  value,
  fields,
  onChange,
}: {
  value: LogsChartOptionsValue;
  fields: readonly string[];
  onChange: (patch: Partial<LogsChartOptionsValue>) => void;
}) {
  const { t } = useTranslation();
  const stepInvalid = Boolean(value.step && parsePrometheusDuration(value.step) === null);
  return (
    <Popover>
      <PopoverTrigger asChild>
        <Button variant="outline" size="sm">
          <Settings2 />
          {t("logs.chartSettings")}
        </Button>
      </PopoverTrigger>
      <PopoverContent align="end" className="w-80 space-y-4">
        <div className="flex items-center gap-2">
          <BarChart3 className="size-4 text-muted-foreground" />
          <span className="text-sm font-semibold">{t("logs.chartSettings")}</span>
        </div>
        <div className="grid grid-cols-2 gap-3">
          <ControlLabel label={t("logs.chartMode")} hint={t("logs.statsModeHint")}>
            <Select
              value={value.graphMode}
              onValueChange={(graphMode) => onChange({ graphMode: graphMode as LogsGraphMode })}
            >
              <SelectTrigger><SelectValue /></SelectTrigger>
              <SelectContent>
                <SelectItem value="hits">{t("logs.hitsMode")}</SelectItem>
                <SelectItem value="stats">{t("logs.statsMode")}</SelectItem>
              </SelectContent>
            </Select>
          </ControlLabel>
          {value.graphMode === "hits" && (
            <ControlLabel label={t("logs.groupBy")}>
              <Select
                value={value.groupBy || "__none"}
                onValueChange={(groupBy) => onChange({
                  groupBy: groupBy === "__none" ? "" : groupBy,
                })}
              >
                <SelectTrigger><SelectValue /></SelectTrigger>
                <SelectContent>
                  <SelectItem value="__none">{t("logs.noGrouping")}</SelectItem>
                  {fields.map((field) => (
                    <SelectItem key={field} value={field}>{field}</SelectItem>
                  ))}
                </SelectContent>
              </Select>
            </ControlLabel>
          )}
          <ControlLabel label={t("logs.topSeries")}>
            <Input
              type="number"
              min={1}
              max={50}
              value={value.topN}
              onChange={(event) => onChange({
                topN: Math.max(1, Math.min(50, Number(event.target.value) || 1)),
              })}
            />
          </ControlLabel>
          <ControlLabel label={t("logs.step")}>
            <Input
              value={value.step}
              placeholder={t("logs.auto")}
              aria-invalid={stepInvalid}
              title={stepInvalid ? t("logs.invalidStep") : undefined}
              className={cn(stepInvalid && "border-destructive text-destructive")}
              onChange={(event) => onChange({ step: event.target.value })}
            />
          </ControlLabel>
        </div>
        <ToggleRow
          label={t("logs.stacked")}
          checked={value.stacked}
          disabled={value.cumulative}
          onCheckedChange={(stacked) => onChange({ stacked })}
        />
        <ToggleRow
          label={t("logs.cumulative")}
          checked={value.cumulative}
          onCheckedChange={(cumulative) => onChange({
            cumulative,
            ...(cumulative ? { stacked: false } : {}),
          })}
        />
        <ToggleRow
          label={t("logs.hideChart")}
          checked={value.hidden}
          onCheckedChange={(hidden) => onChange({ hidden })}
        />
      </PopoverContent>
    </Popover>
  );
}

function ControlLabel({
  label,
  hint,
  children,
}: {
  label: string;
  hint?: string;
  children: React.ReactNode;
}) {
  if (hint) {
    return (
      <div className="space-y-1.5">
        <div className="flex items-center gap-1">
          <span className="text-xs font-normal text-muted-foreground">{label}</span>
          <HelpTip content={hint} />
        </div>
        {children}
      </div>
    );
  }
  return (
    <Label className="space-y-1.5 text-xs font-normal text-muted-foreground">
      <span>{label}</span>
      {children}
    </Label>
  );
}

function ToggleRow({
  label,
  checked,
  disabled,
  onCheckedChange,
}: {
  label: string;
  checked: boolean;
  disabled?: boolean;
  onCheckedChange: (checked: boolean) => void;
}) {
  return (
    <Label className="flex items-center justify-between gap-3 text-sm font-normal">
      <span>{label}</span>
      <Switch
        checked={checked}
        disabled={disabled}
        onCheckedChange={onCheckedChange}
      />
    </Label>
  );
}
