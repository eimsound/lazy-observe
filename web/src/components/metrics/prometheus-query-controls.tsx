import { useState } from "react";
import { ChevronLeft, ChevronRight, Minus, Plus, Settings2 } from "lucide-react";
import { useTranslation } from "react-i18next";
import {
  effectiveMetricsStep,
  formatPrometheusDuration,
  metricsRangeKey,
  metricsRangeSeconds,
  parsePrometheusDuration,
  resolveRangeKeyedDraft,
  setMetricsRangeSeconds,
  shiftMetricsRange,
  stepMetricsRange,
  type MetricsGraphMode,
  type MetricsRange,
  type MetricsResolution,
  type MetricsTableMode,
  type RangeKeyedDraft,
} from "@/lib/metrics-query";
import { MetricsTimeRangePicker } from "@/components/metrics/metrics-time-range-picker";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Popover, PopoverContent, PopoverTrigger } from "@/components/ui/popover";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";

interface PrometheusQueryControlsProps {
  range: MetricsRange;
  resolution: MetricsResolution;
  graphMode: MetricsGraphMode;
  tableMode: MetricsTableMode;
  yAxisZero: boolean;
  localTime: boolean;
  onRangeChange: (range: MetricsRange) => void;
  onResolutionChange: (resolution: MetricsResolution) => void;
  onGraphModeChange: (mode: MetricsGraphMode) => void;
  onTableModeChange: (mode: MetricsTableMode) => void;
  onYAxisZeroChange: (enabled: boolean) => void;
  onLocalTimeChange: (enabled: boolean) => void;
}

export function PrometheusQueryControls({
  range,
  resolution,
  graphMode,
  tableMode,
  yAxisZero,
  localTime,
  onRangeChange,
  onResolutionChange,
  onGraphModeChange,
  onTableModeChange,
  onYAxisZeroChange,
  onLocalTimeChange,
}: PrometheusQueryControlsProps) {
  const { t } = useTranslation();
  const rangeKey = metricsRangeKey(range);
  const seconds = metricsRangeSeconds(range);
  const formattedRange = formatPrometheusDuration(seconds);
  const [rangeDraft, setRangeDraft] = useState<RangeKeyedDraft | null>(null);
  const rangeInput = resolveRangeKeyedDraft(rangeDraft, range, formattedRange);
  const [customStep, setCustomStep] = useState(
    formatPrometheusDuration(effectiveMetricsStep(resolution, seconds)),
  );

  function applyRangeInput() {
    const parsed = parsePrometheusDuration(rangeInput);
    setRangeDraft(null);
    if (parsed === null) {
      return;
    }
    onRangeChange(setMetricsRangeSeconds(range, parsed));
  }

  function applyCustomStep() {
    const parsed = parsePrometheusDuration(customStep);
    if (parsed === null) {
      setCustomStep(formatPrometheusDuration(effectiveMetricsStep(resolution, seconds)));
      return;
    }
    onResolutionChange({ type: "custom", step: parsed });
  }

  const resolutionValue = resolution.type === "auto"
    ? `auto-${resolution.density}`
    : resolution.type === "fixed"
      ? `fixed-${resolution.step}`
      : "custom";

  return (
    <div className="flex flex-wrap items-center justify-between gap-2">
      <div className="flex flex-wrap items-center gap-2">
        <div className="flex h-9 items-center rounded-md border border-input bg-background shadow-sm">
          <Button
            variant="ghost"
            size="icon"
            className="h-full w-9 rounded-r-none"
            aria-label={t("metrics.decreaseRange")}
            onClick={() => onRangeChange(stepMetricsRange(range, -1))}
          ><Minus /></Button>
          <Input
            aria-label={t("metrics.range")}
            className="h-full w-20 rounded-none border-y-0 px-2 text-center font-mono text-xs shadow-none focus-visible:ring-0"
            value={rangeInput}
            onChange={(event) => setRangeDraft({ rangeKey, value: event.target.value })}
            onBlur={applyRangeInput}
            onKeyDown={(event) => event.key === "Enter" && applyRangeInput()}
          />
          <Button
            variant="ghost"
            size="icon"
            className="h-full w-9 rounded-l-none"
            aria-label={t("metrics.increaseRange")}
            onClick={() => onRangeChange(stepMetricsRange(range, 1))}
          ><Plus /></Button>
        </div>

        <div className="inline-flex h-9 items-center">
          <Button variant="outline" size="icon" className="size-9 rounded-r-none" aria-label={t("metrics.previousRange")} onClick={() => onRangeChange(shiftMetricsRange(range, -1))}>
            <ChevronLeft />
          </Button>
          <MetricsTimeRangePicker
            value={range}
            localTime={localTime}
            triggerClassName="min-w-48 rounded-none border-l-0"
            onChange={onRangeChange}
            onLocalTimeChange={onLocalTimeChange}
          />
          <Button variant="outline" size="icon" className="size-9 rounded-l-none border-l-0" aria-label={t("metrics.nextRange")} onClick={() => onRangeChange(shiftMetricsRange(range, 1))}>
            <ChevronRight />
          </Button>
        </div>

        <Select
          value={resolutionValue}
          onValueChange={(value) => {
            if (value.startsWith("auto-")) {
              onResolutionChange({ type: "auto", density: value.slice(5) as "low" | "medium" | "high" });
            } else if (value.startsWith("fixed-")) {
              onResolutionChange({ type: "fixed", step: Number(value.slice(6)) });
            } else {
              const step = effectiveMetricsStep(resolution, seconds);
              setCustomStep(formatPrometheusDuration(step));
              onResolutionChange({ type: "custom", step });
            }
          }}
        >
          <SelectTrigger className="w-40" aria-label={t("metrics.resolution")}><SelectValue /></SelectTrigger>
          <SelectContent>
            <SelectItem value="auto-low">{t("metrics.resolutionLow")}</SelectItem>
            <SelectItem value="auto-medium">{t("metrics.resolutionMedium")}</SelectItem>
            <SelectItem value="auto-high">{t("metrics.resolutionHigh")}</SelectItem>
            {[10, 30, 60, 300, 900, 3600].map((step) => (
              <SelectItem key={step} value={`fixed-${step}`}>{formatPrometheusDuration(step)}</SelectItem>
            ))}
            <SelectItem value="custom">{t("metrics.resolutionCustom")}</SelectItem>
          </SelectContent>
        </Select>
        {resolution.type === "custom" && (
          <Input
            className="h-9 w-24 font-mono text-xs"
            aria-label={t("metrics.customResolution")}
            value={customStep}
            onChange={(event) => setCustomStep(event.target.value)}
            onBlur={applyCustomStep}
            onKeyDown={(event) => event.key === "Enter" && applyCustomStep()}
          />
        )}
      </div>

      <div className="flex items-center gap-2">
        <div className="inline-flex h-9 rounded-md border border-input bg-background p-0.5">
          <Button variant={graphMode === "lines" ? "secondary" : "ghost"} size="sm" className="h-full" onClick={() => onGraphModeChange("lines")}>{t("metrics.unstacked")}</Button>
          <Button variant={graphMode === "stacked" ? "secondary" : "ghost"} size="sm" className="h-full" onClick={() => onGraphModeChange("stacked")}>{t("metrics.stacked")}</Button>
        </div>
        <Popover>
          <PopoverTrigger asChild>
            <Button variant="outline" size="icon" className="size-9" aria-label={t("metrics.graphSettings")}><Settings2 /></Button>
          </PopoverTrigger>
          <PopoverContent
            align="end"
            className="w-max max-w-[calc(100vw-2rem)] space-y-4"
          >
            <div>
              <label className="flex cursor-pointer items-center gap-2 whitespace-nowrap text-sm">
                <input
                  type="checkbox"
                  checked={yAxisZero}
                  onChange={(event) => onYAxisZeroChange(event.target.checked)}
                />
                {t("metrics.yAxisZero")}
              </label>
            </div>
            <div className="space-y-2 border-t border-border pt-3">
              <p className="text-xs font-medium text-muted-foreground">
                {t("metrics.columnMode")}
              </p>
              <div className="inline-flex rounded-md border border-input bg-background p-0.5">
                <Button
                  variant={tableMode === "labels" ? "secondary" : "ghost"}
                  size="sm"
                  className="h-7 px-2.5"
                  onClick={() => onTableModeChange("labels")}
                >
                  {t("metrics.labelColumns")}
                </Button>
                <Button
                  variant={tableMode === "series" ? "secondary" : "ghost"}
                  size="sm"
                  className="h-7 px-2.5"
                  onClick={() => onTableModeChange("series")}
                >
                  {t("metrics.seriesColumns")}
                </Button>
              </div>
            </div>
          </PopoverContent>
        </Popover>
      </div>
    </div>
  );
}
