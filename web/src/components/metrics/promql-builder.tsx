import { useEffect, useMemo, useRef, useState } from "react";
import {
  AlertTriangle,
  ChevronDown,
  Code2,
  Loader2,
  Plus,
  RotateCcw,
  Search,
  Trash2,
  X,
} from "lucide-react";
import { useTranslation } from "react-i18next";
import { labelNames, labelValues, metricNames } from "@/lib/api/metrics";
import { useProjectQuery } from "@/lib/project/use-project-query";
import {
  metricsAutocompleteRange,
  type ResolvedMetricsRange,
} from "@/lib/metrics-query";
import {
  addBuilderFilter,
  applyBuilderHistogramQuantile,
  buildMetadataMatchSelector,
  createBuilderController,
  PROMQL_AGGREGATIONS,
  PROMQL_LABEL_OPERATORS,
  PROMQL_QUANTILE_PRESETS,
  PROMQL_RANGE_FUNCTIONS,
  PROMQL_RANGE_INTERVAL_PRESETS,
  removeBuilderFilter,
  resetBuilderController,
  syncBuilderControllerFromQuery,
  toggleBuilderAggregationLabel,
  updateBuilderController,
  updateBuilderFilter,
  type PromQLAggregation,
  type PromQLAggregationMode,
  type PromQLBuilderState,
  type PromQLBuilderStatus,
  type PromQLLabelFilter,
  type PromQLLabelOperator,
  type PromQLRangeFunction,
} from "@/lib/promql-builder";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
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

const METADATA_METRIC_LIMIT = 10_000;
const METADATA_LABEL_LIMIT = 1_000;
const MAX_VISIBLE_SUGGESTIONS = 120;

export interface PromQLBuilderMetadataState {
  options: readonly string[];
  isLoading: boolean;
  isError: boolean;
}

export interface PromQLBuilderProps {
  project: string;
  query: string;
  range: ResolvedMetricsRange;
  onQueryChange: (query: string) => void;
  onSwitchToCode: () => void;
}

export interface PromQLBuilderViewProps {
  draft: PromQLBuilderState;
  status: PromQLBuilderStatus;
  rawQuery: string;
  metricsMetadata: PromQLBuilderMetadataState;
  labelsMetadata: PromQLBuilderMetadataState;
  renderFilterValueInput: (filter: PromQLLabelFilter) => React.ReactNode;
  onUpdateDraft: (updater: (draft: PromQLBuilderState) => PromQLBuilderState) => void;
  onSwitchToCode: () => void;
  onReset: () => void;
}

interface SearchableComboInputProps {
  id?: string;
  value: string;
  placeholder: string;
  ariaLabel: string;
  options: readonly string[];
  isLoading?: boolean;
  isError?: boolean;
  allowClear?: boolean;
  clearLabel?: string;
  className?: string;
  onChange: (value: string) => void;
  onSelect?: (value: string) => void;
  onOpenChange?: (open: boolean) => void;
}

function SearchableComboInput({
  id,
  value,
  placeholder,
  ariaLabel,
  options,
  isLoading = false,
  isError = false,
  allowClear = false,
  clearLabel,
  className,
  onChange,
  onSelect,
  onOpenChange,
}: SearchableComboInputProps) {
  const { t } = useTranslation();
  const [open, setOpen] = useState(false);
  const [search, setSearch] = useState("");

  function setPopoverOpen(next: boolean) {
    setOpen(next);
    if (!next) setSearch("");
    onOpenChange?.(next);
  }

  const filtered = useMemo(() => {
    const needle = search.trim().toLowerCase();
    const list = needle
      ? options.filter((item) => item.toLowerCase().includes(needle))
      : options;
    return list.slice(0, MAX_VISIBLE_SUGGESTIONS);
  }, [options, search]);

  return (
    <div className={cn("relative flex min-w-0 items-center", className)}>
      <Input
        id={id}
        aria-label={ariaLabel}
        value={value}
        placeholder={placeholder}
        className={cn(
          "h-9 font-mono text-xs",
          allowClear && value ? "pr-14" : "pr-8",
        )}
        onChange={(event) => onChange(event.target.value)}
      />
      <div className="absolute right-1 flex items-center gap-0.5">
        {allowClear && value && (
          <Button
            type="button"
            variant="ghost"
            size="icon"
            className="size-7 text-muted-foreground hover:text-foreground"
            aria-label={clearLabel ?? t("metrics.builder.metricClear")}
            onClick={() => onChange("")}
          >
            <X className="size-3.5" />
          </Button>
        )}
        <Popover open={open} onOpenChange={setPopoverOpen}>
          <PopoverTrigger asChild>
            <Button
              type="button"
              variant="ghost"
              size="icon"
              className="size-7 text-muted-foreground hover:text-foreground"
              aria-label={t("metrics.builder.openSuggestions", { label: ariaLabel })}
              aria-expanded={open}
            >
              <ChevronDown className="size-3.5" />
            </Button>
          </PopoverTrigger>
          <PopoverContent align="start" className="w-80 max-w-[calc(100vw-2rem)] p-2">
            <div className="space-y-2">
              <div className="relative">
                <Search className="pointer-events-none absolute left-2.5 top-2.5 size-3.5 text-muted-foreground" />
                <Input
                  value={search}
                  onChange={(event) => setSearch(event.target.value)}
                  placeholder={placeholder}
                  className="h-8 pl-8 font-mono text-xs"
                  autoFocus
                />
              </div>
              <div className="max-h-56 overflow-auto rounded-md border border-border" role="listbox">
                {isLoading ? (
                  <div className="flex items-center justify-center gap-2 py-6 text-xs text-muted-foreground">
                    <Loader2 className="size-3.5 animate-spin" />
                    {t("metrics.builder.loadingMetadata")}
                  </div>
                ) : isError ? (
                  <p className="p-3 text-xs text-muted-foreground">
                    {t("metrics.builder.metadataErrorFallback")}
                  </p>
                ) : filtered.length === 0 ? (
                  <p className="p-3 text-xs text-muted-foreground">
                    {t("metrics.builder.metadataEmptyFallback")}
                  </p>
                ) : (
                  filtered.map((item) => (
                    <button
                      key={item}
                      type="button"
                      role="option"
                      aria-selected={item === value}
                      className={cn(
                        "block w-full truncate px-2.5 py-1.5 text-left font-mono text-xs hover:bg-muted",
                        item === value && "bg-accent text-accent-foreground",
                      )}
                      title={item}
                      onClick={() => {
                        if (onSelect) {
                          onSelect(item);
                        } else {
                          onChange(item);
                        }
                        setPopoverOpen(false);
                      }}
                    >
                      {item}
                    </button>
                  ))
                )}
              </div>
            </div>
          </PopoverContent>
        </Popover>
      </div>
    </div>
  );
}

function FilterValueCombobox({
  project,
  range,
  draft,
  filter,
  onChange,
}: {
  project: string;
  range: Pick<ResolvedMetricsRange, "start" | "end">;
  draft: PromQLBuilderState;
  filter: PromQLLabelFilter;
  onChange: (value: string) => void;
}) {
  const { t } = useTranslation();
  const [dropdownOpen, setDropdownOpen] = useState(false);
  const label = filter.label.trim();
  const matchSelector = useMemo(
    () => buildMetadataMatchSelector(draft, { excludeFilterId: filter.id }),
    [draft, filter.id],
  );

  const valuesQuery = useProjectQuery({
    project,
    queryKey: [
      "promql-builder-values",
      label,
      matchSelector ?? "",
      range.start,
      range.end,
    ],
    enabled: dropdownOpen && Boolean(label),
    staleTime: 60_000,
    refetchOnWindowFocus: false,
    retry: false,
    queryFn: ({ signal, project: activeProject }) =>
      labelValues(
        label,
        {
          start: range.start,
          end: range.end,
          match: matchSelector,
          limit: METADATA_LABEL_LIMIT,
          project: activeProject,
        },
        signal,
      ),
  });

  return (
    <SearchableComboInput
      value={filter.value}
      placeholder={t("metrics.builder.valuePlaceholder")}
      ariaLabel={t("metrics.builder.valuePlaceholder")}
      options={valuesQuery.data ?? []}
      isLoading={valuesQuery.isLoading}
      isError={valuesQuery.isError}
      className="flex-1"
      onChange={onChange}
      onOpenChange={setDropdownOpen}
    />
  );
}

function operatorLabelKey(op: PromQLLabelOperator): string {
  switch (op) {
    case "=":
      return "metrics.builder.operators.eq";
    case "!=":
      return "metrics.builder.operators.neq";
    case "=~":
      return "metrics.builder.operators.re";
    case "!~":
      return "metrics.builder.operators.nre";
  }
}

export function PromQLBuilderView({
  draft,
  status,
  rawQuery,
  metricsMetadata,
  labelsMetadata,
  renderFilterValueInput,
  onUpdateDraft,
  onSwitchToCode,
  onReset,
}: PromQLBuilderViewProps) {
  const { t } = useTranslation();
  const [groupLabelInput, setGroupLabelInput] = useState("");
  const isPresetQuantile =
    draft.histogramQuantile === ""
    || (PROMQL_QUANTILE_PRESETS as readonly string[]).includes(draft.histogramQuantile);
  const [customQuantileOpen, setCustomQuantileOpen] = useState(!isPresetQuantile);
  const quantileSelectValue = draft.histogramQuantile === ""
    ? (customQuantileOpen ? "custom" : "none")
    : (PROMQL_QUANTILE_PRESETS as readonly string[]).includes(draft.histogramQuantile) && !customQuantileOpen
      ? draft.histogramQuantile
      : "custom";

  useEffect(() => {
    if (
      draft.histogramQuantile !== ""
      && !(PROMQL_QUANTILE_PRESETS as readonly string[]).includes(draft.histogramQuantile)
    ) {
      setCustomQuantileOpen(true);
    }
  }, [draft.histogramQuantile]);

  if (status !== "synced") {
    const isSyntax = status === "syntax";
    return (
      <div
        className="space-y-3 rounded-md border border-border bg-muted/30 p-3.5"
        role="status"
        data-promql-builder-status={status}
      >
        <div className="flex items-start gap-2.5">
          <AlertTriangle className="mt-0.5 size-4 shrink-0 text-amber-500" />
          <div className="min-w-0 flex-1 space-y-1">
            <div className="text-sm font-medium">
              {t(isSyntax ? "metrics.builder.syntaxTitle" : "metrics.builder.complexTitle")}
            </div>
            <p className="text-xs text-muted-foreground">
              {t(isSyntax ? "metrics.builder.syntaxDesc" : "metrics.builder.complexDesc")}
            </p>
            {rawQuery.trim() && (
              <code className="mt-1.5 block max-h-24 overflow-auto rounded border border-border bg-background px-2.5 py-1.5 font-mono text-xs break-all">
                {rawQuery}
              </code>
            )}
          </div>
        </div>
        <div className="flex flex-wrap items-center gap-2">
          <Button type="button" size="sm" onClick={onSwitchToCode}>
            <Code2 className="size-3.5" />
            {t("metrics.builder.switchToCode")}
          </Button>
          <Button type="button" variant="outline" size="sm" onClick={onReset}>
            <RotateCcw className="size-3.5" />
            {t("metrics.builder.resetBuilder")}
          </Button>
        </div>
      </div>
    );
  }

  const availableGroupLabels = labelsMetadata.options.filter(
    (label) => !draft.aggregationLabels.includes(label),
  );
  const needsParam = draft.aggregation === "topk" || draft.aggregation === "bottomk";

  function commitGroupLabel(raw: string) {
    const trimmed = raw.trim();
    if (!trimmed) return;
    onUpdateDraft((current) => {
      if (current.aggregationLabels.includes(trimmed)) return current;
      return toggleBuilderAggregationLabel(current, trimmed);
    });
    setGroupLabelInput("");
  }

  return (
    <div
      className="space-y-3.5 rounded-md border border-border bg-muted/20 p-3.5"
      data-promql-builder-status="synced"
    >
      {/* 1. Metric selector */}
      <div className="space-y-1.5">
        <div className="flex items-center justify-between gap-2">
          <Label className="text-xs font-medium text-muted-foreground">
            {t("metrics.builder.metric")}
          </Label>
          {metricsMetadata.isError && (
            <span className="text-[11px] text-muted-foreground">
              {t("metrics.builder.metadataErrorFallback")}
            </span>
          )}
        </div>
        <SearchableComboInput
          value={draft.metric}
          placeholder={t("metrics.builder.metricPlaceholder")}
          ariaLabel={t("metrics.builder.metric")}
          options={metricsMetadata.options}
          isLoading={metricsMetadata.isLoading}
          isError={metricsMetadata.isError}
          allowClear
          clearLabel={t("metrics.builder.metricClear")}
          onChange={(metric) => onUpdateDraft((current) => ({ ...current, metric }))}
        />
      </div>

      {/* 2. Label filters */}
      <div className="space-y-2">
        <div className="flex items-center justify-between gap-2">
          <Label className="text-xs font-medium text-muted-foreground">
            {t("metrics.builder.filters")}
          </Label>
          <Button
            type="button"
            variant="outline"
            size="sm"
            className="h-7 gap-1 px-2 text-xs"
            onClick={() => onUpdateDraft(addBuilderFilter)}
          >
            <Plus className="size-3.5" />
            {t("metrics.builder.addFilter")}
          </Button>
        </div>

        {draft.filters.length === 0 ? (
          <p className="text-xs text-muted-foreground">{t("metrics.builder.noFiltersHint")}</p>
        ) : (
          <div className="space-y-2">
            {draft.filters.map((filter) => (
              <div
                key={filter.id}
                className="flex flex-col gap-2 sm:flex-row sm:items-center"
                data-promql-filter-row={filter.id}
              >
                <SearchableComboInput
                  value={filter.label}
                  placeholder={t("metrics.builder.labelPlaceholder")}
                  ariaLabel={t("metrics.builder.labelPlaceholder")}
                  options={labelsMetadata.options}
                  isLoading={labelsMetadata.isLoading}
                  isError={labelsMetadata.isError}
                  className="sm:w-52"
                  onChange={(label) =>
                    onUpdateDraft((current) => updateBuilderFilter(current, filter.id, { label }))
                  }
                />
                <Select
                  value={filter.operator}
                  onValueChange={(operator) =>
                    onUpdateDraft((current) =>
                      updateBuilderFilter(current, filter.id, {
                        operator: operator as PromQLLabelOperator,
                      }),
                    )
                  }
                >
                  <SelectTrigger
                    className="h-9 w-full font-mono text-xs sm:w-36"
                    aria-label={t("metrics.builder.operatorLabel")}
                  >
                    <SelectValue />
                  </SelectTrigger>
                  <SelectContent>
                    {PROMQL_LABEL_OPERATORS.map((op) => (
                      <SelectItem key={op} value={op} className="font-mono text-xs">
                        {t(operatorLabelKey(op))}
                      </SelectItem>
                    ))}
                  </SelectContent>
                </Select>
                {renderFilterValueInput(filter)}
                <Button
                  type="button"
                  variant="ghost"
                  size="icon"
                  className="size-9 shrink-0 text-muted-foreground hover:text-destructive"
                  aria-label={t("metrics.builder.removeFilter")}
                  title={t("metrics.builder.removeFilter")}
                  onClick={() => onUpdateDraft((current) => removeBuilderFilter(current, filter.id))}
                >
                  <Trash2 className="size-4" />
                </Button>
              </div>
            ))}
          </div>
        )}
      </div>

      {/* 3. Range function, aggregation, and histogram_quantile */}
      <div className="grid gap-3 border-t border-border/70 pt-3 md:grid-cols-3">
        {/* Range function + interval */}
        <div className="space-y-2">
          <Label className="text-xs font-medium text-muted-foreground">
            {t("metrics.builder.rangeFunction")}
          </Label>
          <div className="flex items-center gap-2">
            <Select
              value={draft.rangeFunction}
              onValueChange={(value) =>
                onUpdateDraft((current) => ({
                  ...current,
                  rangeFunction: value as PromQLRangeFunction,
                }))
              }
            >
              <SelectTrigger
                className="h-9 flex-1 font-mono text-xs"
                aria-label={t("metrics.builder.rangeFunction")}
              >
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                <SelectItem value="none" className="text-xs">
                  {t("metrics.builder.rangeFunctionNone")}
                </SelectItem>
                {PROMQL_RANGE_FUNCTIONS.map((fn) => (
                  <SelectItem key={fn} value={fn} className="font-mono text-xs">
                    {fn}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
            {draft.rangeFunction !== "none" && (
              <SearchableComboInput
                value={draft.rangeInterval}
                placeholder={t("metrics.builder.rangeIntervalPlaceholder")}
                ariaLabel={t("metrics.builder.rangeInterval")}
                options={PROMQL_RANGE_INTERVAL_PRESETS}
                className="w-36 shrink-0"
                onChange={(rangeInterval) =>
                  onUpdateDraft((current) => ({ ...current, rangeInterval }))
                }
              />
            )}
          </div>
        </div>

        {/* Aggregation + k + grouping */}
        <div className="space-y-2">
          <Label className="text-xs font-medium text-muted-foreground">
            {t("metrics.builder.aggregation")}
          </Label>
          <div className="flex items-center gap-2">
            <Select
              value={draft.aggregation}
              onValueChange={(value) =>
                onUpdateDraft((current) => ({
                  ...current,
                  aggregation: value as PromQLAggregation,
                  aggregationMode: value === "none" ? "none" : current.aggregationMode,
                  aggregationLabels: value === "none" ? [] : current.aggregationLabels,
                }))
              }
            >
              <SelectTrigger
                className="h-9 flex-1 font-mono text-xs"
                aria-label={t("metrics.builder.aggregation")}
              >
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                <SelectItem value="none" className="text-xs">
                  {t("metrics.builder.aggregationNone")}
                </SelectItem>
                {PROMQL_AGGREGATIONS.map((agg) => (
                  <SelectItem key={agg} value={agg} className="font-mono text-xs">
                    {agg}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
            {needsParam && (
              <Input
                type="number"
                min={1}
                step={1}
                aria-label={t("metrics.builder.aggregationParam")}
                className="h-9 w-20 font-mono text-xs"
                value={draft.aggregationParam}
                onChange={(event) =>
                  onUpdateDraft((current) => ({
                    ...current,
                    aggregationParam: event.target.value,
                  }))
                }
              />
            )}
            {draft.aggregation !== "none" && (
              <Select
                value={draft.aggregationMode}
                onValueChange={(value) =>
                  onUpdateDraft((current) => ({
                    ...current,
                    aggregationMode: value as PromQLAggregationMode,
                  }))
                }
              >
                <SelectTrigger
                  className="h-9 w-36 shrink-0 text-xs"
                  aria-label={t("metrics.builder.aggregationMode")}
                >
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  <SelectItem value="none" className="text-xs">
                    {t("metrics.builder.aggregationModeNone")}
                  </SelectItem>
                  <SelectItem value="by" className="text-xs">
                    {t("metrics.builder.aggregationModeBy")}
                  </SelectItem>
                  <SelectItem value="without" className="text-xs">
                    {t("metrics.builder.aggregationModeWithout")}
                  </SelectItem>
                </SelectContent>
              </Select>
            )}
          </div>
        </div>

        {/* Histogram quantile */}
        <div className="space-y-2">
          <Label className="text-xs font-medium text-muted-foreground">
            {t("metrics.builder.quantile")}
          </Label>
          <div className="flex items-center gap-2">
            <Select
              value={quantileSelectValue}
              onValueChange={(value) => {
                if (value === "none") {
                  setCustomQuantileOpen(false);
                  onUpdateDraft((current) => applyBuilderHistogramQuantile(current, ""));
                } else if (value === "custom") {
                  setCustomQuantileOpen(true);
                  if (!draft.histogramQuantile) {
                    onUpdateDraft((current) => applyBuilderHistogramQuantile(current, "0.95"));
                  }
                } else {
                  setCustomQuantileOpen(false);
                  onUpdateDraft((current) => applyBuilderHistogramQuantile(current, value));
                }
              }}
            >
              <SelectTrigger
                className="h-9 flex-1 font-mono text-xs"
                aria-label={t("metrics.builder.quantile")}
              >
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                <SelectItem value="none" className="text-xs">
                  {t("metrics.builder.quantileNone")}
                </SelectItem>
                <SelectItem value="0.5" className="font-mono text-xs">
                  P50 (0.5)
                </SelectItem>
                <SelectItem value="0.9" className="font-mono text-xs">
                  P90 (0.9)
                </SelectItem>
                <SelectItem value="0.95" className="font-mono text-xs">
                  P95 (0.95)
                </SelectItem>
                <SelectItem value="0.99" className="font-mono text-xs">
                  P99 (0.99)
                </SelectItem>
                <SelectItem value="custom" className="text-xs">
                  {t("metrics.builder.quantileCustom")}
                </SelectItem>
              </SelectContent>
            </Select>
            {customQuantileOpen && (
              <Input
                aria-label={t("metrics.builder.quantilePlaceholder")}
                placeholder={t("metrics.builder.quantilePlaceholder")}
                className="h-9 w-28 font-mono text-xs"
                value={draft.histogramQuantile}
                onChange={(event) =>
                  onUpdateDraft((current) => ({
                    ...current,
                    histogramQuantile: event.target.value,
                  }))
                }
              />
            )}
          </div>
        </div>
      </div>

      {/* 4. Grouping labels (only when aggregation + by/without is active) */}
      {draft.aggregation !== "none" && draft.aggregationMode !== "none" && (
        <div className="space-y-2 border-t border-border/70 pt-3">
          <Label className="text-xs font-medium text-muted-foreground">
            {t("metrics.builder.aggregationLabels")} ({draft.aggregationMode})
          </Label>
          <div className="flex flex-wrap items-center gap-1.5">
            {draft.aggregationLabels.map((label) => (
              <Badge key={label} variant="secondary" className="gap-1 font-mono text-xs">
                <span>{label}</span>
                <button
                  type="button"
                  className="rounded-sm hover:text-destructive"
                  aria-label={t("metrics.builder.removeLabel", { label })}
                  onClick={() =>
                    onUpdateDraft((current) => toggleBuilderAggregationLabel(current, label))
                  }
                >
                  <X className="size-3" />
                </button>
              </Badge>
            ))}
            <div className="flex min-w-52 flex-1 items-center gap-1.5 sm:max-w-sm">
              <div
                className="min-w-0 flex-1"
                onKeyDown={(event) => {
                  if (event.key === "Enter") {
                    event.preventDefault();
                    commitGroupLabel(groupLabelInput);
                  }
                }}
              >
                <SearchableComboInput
                  value={groupLabelInput}
                  placeholder={t("metrics.builder.aggregationLabelPlaceholder")}
                  ariaLabel={t("metrics.builder.aggregationLabels")}
                  options={availableGroupLabels}
                  isLoading={labelsMetadata.isLoading}
                  isError={labelsMetadata.isError}
                  onChange={setGroupLabelInput}
                  onSelect={commitGroupLabel}
                />
              </div>
              <Button
                type="button"
                variant="outline"
                size="sm"
                className="h-9 shrink-0"
                disabled={!groupLabelInput.trim()}
                onClick={() => commitGroupLabel(groupLabelInput)}
              >
                {t("metrics.builder.addLabel")}
              </Button>
            </div>
          </div>
        </div>
      )}
    </div>
  );
}

export function PromQLBuilder({
  project,
  query,
  range,
  onQueryChange,
  onSwitchToCode,
}: PromQLBuilderProps) {
  const [controller, setController] = useState(() => createBuilderController(query));
  const [viewEpoch, setViewEpoch] = useState(0);
  const previousProject = useRef(project);

  // Sync incoming external query changes (CodeMirror typing, History, Explorer, URL)
  // without clobbering in-progress blank filter rows.
  useEffect(() => {
    setController((current) => syncBuilderControllerFromQuery(current, query));
  }, [query]);

  // Reset local builder draft and transient UI controls immediately when the active project changes.
  useEffect(() => {
    if (previousProject.current === project) return;
    previousProject.current = project;
    setController(createBuilderController(""));
    setViewEpoch((current) => current + 1);
  }, [project]);

  const bucketRange = useMemo(() => metricsAutocompleteRange(range), [range]);
  const matchSelector = useMemo(
    () => buildMetadataMatchSelector(controller.draft),
    [controller.draft],
  );

  const metricsQuery = useProjectQuery({
    project,
    queryKey: ["promql-builder-metrics", bucketRange.start, bucketRange.end],
    staleTime: 60_000,
    refetchOnWindowFocus: false,
    retry: false,
    queryFn: ({ signal, project: activeProject }) =>
      metricNames(
        {
          start: bucketRange.start,
          end: bucketRange.end,
          limit: METADATA_METRIC_LIMIT,
          project: activeProject,
        },
        signal,
      ),
  });

  const labelsQuery = useProjectQuery({
    project,
    queryKey: [
      "promql-builder-labels",
      matchSelector ?? "",
      bucketRange.start,
      bucketRange.end,
    ],
    staleTime: 60_000,
    refetchOnWindowFocus: false,
    retry: false,
    queryFn: ({ signal, project: activeProject }) =>
      labelNames(
        {
          start: bucketRange.start,
          end: bucketRange.end,
          match: matchSelector,
          limit: METADATA_LABEL_LIMIT,
          project: activeProject,
        },
        signal,
      ),
  });

  const metricOptions = useMemo(() => metricsQuery.data ?? [], [metricsQuery.data]);
  const labelOptions = useMemo(
    () => (labelsQuery.data ?? []).filter((name) => name !== "__name__" && name !== "project_id"),
    [labelsQuery.data],
  );

  function handleUpdateDraft(updater: (draft: PromQLBuilderState) => PromQLBuilderState) {
    const next = updateBuilderController(controller, updater);
    setController(next.controller);
    if (next.query !== query) {
      onQueryChange(next.query);
    }
  }

  function handleReset() {
    const next = resetBuilderController();
    setController(next.controller);
    setViewEpoch((current) => current + 1);
    if (query !== "") {
      onQueryChange("");
    }
  }

  return (
    <PromQLBuilderView
      key={viewEpoch}
      draft={controller.draft}
      status={controller.status}
      rawQuery={query}
      metricsMetadata={{
        options: metricOptions,
        isLoading: metricsQuery.isLoading,
        isError: metricsQuery.isError,
      }}
      labelsMetadata={{
        options: labelOptions,
        isLoading: labelsQuery.isLoading,
        isError: labelsQuery.isError,
      }}
      renderFilterValueInput={(filter) => (
        <FilterValueCombobox
          project={project}
          range={bucketRange}
          draft={controller.draft}
          filter={filter}
          onChange={(value) =>
            handleUpdateDraft((current) => updateBuilderFilter(current, filter.id, { value }))
          }
        />
      )}
      onUpdateDraft={handleUpdateDraft}
      onSwitchToCode={onSwitchToCode}
      onReset={handleReset}
    />
  );
}
