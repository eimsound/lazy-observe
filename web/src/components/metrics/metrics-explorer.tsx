import { useEffect, useMemo, useRef, useState } from "react";
import { ArrowLeft, Check, Copy, Loader2, Plus, Search } from "lucide-react";
import { useTranslation } from "react-i18next";
import { labelNames, labelValues, metricNames } from "@/lib/api/metrics";
import { useProject } from "@/lib/project/project-provider";
import { useProjectQuery } from "@/lib/project/use-project-query";
import type { ResolvedMetricsRange } from "@/lib/metrics-query";
import { Button } from "@/components/ui/button";
import {
  Dialog,
  DialogContent,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";

interface MetricsExplorerProps {
  open: boolean;
  range: ResolvedMetricsRange;
  onOpenChange: (open: boolean) => void;
  onInsert: (selector: string) => void;
}

const escapePromQL = (value: string) => value.replace(/\\/g, "\\\\").replace(/"/g, '\\"');

export function MetricsExplorer({ open, range, onOpenChange, onInsert }: MetricsExplorerProps) {
  const { t } = useTranslation();
  const { currentSlug: project } = useProject();
  const previousProject = useRef(project);
  const [filter, setFilter] = useState("");
  const [selectedMetric, setSelectedMetric] = useState("");
  const [selectedLabel, setSelectedLabel] = useState("");
  const [matchers, setMatchers] = useState<Record<string, string>>({});
  const base = { start: range.start, end: range.end, limit: 10_000, project };

  useEffect(() => {
    if (previousProject.current === project) return;
    previousProject.current = project;
    setFilter("");
    setSelectedMetric("");
    setSelectedLabel("");
    setMatchers({});
  }, [project]);

  const names = useProjectQuery({
    queryKey: ["metrics-explorer", range.start, range.end],
    queryFn: ({ signal, project: activeProject }) =>
      metricNames({ ...base, project: activeProject }, signal),
    enabled: open,
  });
  const selector = selectedMetric
    ? `${selectedMetric}${Object.keys(matchers).length > 0
      ? `{${Object.entries(matchers).map(([name, value]) => `${name}="${escapePromQL(value)}"`).join(",")}}`
      : ""}`
    : "";
  const labels = useProjectQuery({
    queryKey: ["metrics-explorer-labels", selectedMetric, selector, range.start, range.end],
    queryFn: ({ signal, project: activeProject }) =>
      labelNames({ ...base, project: activeProject, match: selector || undefined }, signal),
    enabled: open && Boolean(selectedMetric),
  });
  const values = useProjectQuery({
    queryKey: ["metrics-explorer-values", selectedMetric, selectedLabel, selector, range.start, range.end],
    queryFn: ({ signal, project: activeProject }) =>
      labelValues(selectedLabel, { ...base, project: activeProject, match: selector }, signal),
    enabled: open && Boolean(selectedMetric && selectedLabel),
  });
  const visibleMetrics = useMemo(() => {
    const needle = filter.trim().toLowerCase();
    return (names.data ?? []).filter((name) => !needle || name.toLowerCase().includes(needle));
  }, [filter, names.data]);
  const [copied, setCopied] = useState(false);

  function resetMetric() {
    setSelectedMetric("");
    setSelectedLabel("");
    setMatchers({});
  }

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="max-w-5xl" aria-describedby={undefined}>
        <DialogHeader>
          <DialogTitle>{t("metrics.exploreMetrics")}</DialogTitle>
        </DialogHeader>

        {!selectedMetric ? (
          <div className="space-y-3">
            <div className="relative">
              <Search className="absolute left-3 top-2.5 size-4 text-muted-foreground" />
              <Input
                autoFocus
                className="pl-9"
                value={filter}
                onChange={(event) => setFilter(event.target.value)}
                placeholder={t("metrics.filterMetrics")}
              />
            </div>
            <div className="max-h-[60vh] overflow-auto rounded-md border border-border">
              {names.isLoading ? (
                <div className="flex items-center justify-center gap-2 py-16 text-sm text-muted-foreground">
                  <Loader2 className="size-4 animate-spin" /> {t("common.loading")}
                </div>
              ) : names.error ? (
                <p className="p-4 text-sm text-destructive">{names.error.message}</p>
              ) : visibleMetrics.length === 0 ? (
                <p className="p-8 text-center text-sm text-muted-foreground">{t("metrics.noMetrics")}</p>
              ) : visibleMetrics.map((name) => (
                <div key={name} className="flex items-center justify-between gap-3 border-b border-border px-3 py-2 last:border-0 hover:bg-muted/40">
                  <code className="min-w-0 truncate text-xs">{name}</code>
                  <div className="flex shrink-0 gap-1">
                    <Button variant="ghost" size="sm" onClick={() => setSelectedMetric(name)}>
                      {t("metrics.explore")}
                    </Button>
                    <Button
                      variant="outline"
                      size="sm"
                      onClick={() => {
                        onInsert(name);
                        onOpenChange(false);
                      }}
                    >
                      <Plus /> {t("metrics.insert")}
                    </Button>
                  </div>
                </div>
              ))}
            </div>
          </div>
        ) : (
          <div className="space-y-4">
            <div className="flex flex-wrap items-center justify-between gap-2">
              <Button variant="ghost" size="sm" onClick={resetMetric}><ArrowLeft />{t("metrics.allMetrics")}</Button>
              <div className="flex min-w-0 items-center gap-2">
                <code className="max-w-xl truncate rounded bg-muted px-2 py-1 text-xs">{selector}</code>
                <Button
                  variant="outline"
                  size="sm"
                  onClick={() => {
                    onInsert(selector);
                    onOpenChange(false);
                  }}
                ><Plus />{t("metrics.insert")}</Button>
                <Button
                  variant="ghost"
                  size="icon"
                  aria-label={t("common.copy")}
                  onClick={() => {
                    void navigator.clipboard.writeText(selector).then(() => {
                      setCopied(true);
                      window.setTimeout(() => setCopied(false), 1500);
                    });
                  }}
                >{copied ? <Check /> : <Copy />}</Button>
              </div>
            </div>
            <div className="grid gap-4 md:grid-cols-[220px_1fr]">
              <div className="max-h-[55vh] overflow-auto rounded-md border border-border p-1">
                {(labels.data ?? []).filter((name) => name !== "__name__").map((name) => (
                  <button
                    key={name}
                    type="button"
                    className={`block w-full rounded px-2 py-1.5 text-left font-mono text-xs ${selectedLabel === name ? "bg-accent" : "hover:bg-muted"}`}
                    onClick={() => setSelectedLabel(name)}
                  >
                    {name}{matchers[name] !== undefined ? ` = "${matchers[name]}"` : ""}
                  </button>
                ))}
              </div>
              <div className="max-h-[55vh] overflow-auto rounded-md border border-border p-2">
                {!selectedLabel ? (
                  <p className="p-8 text-center text-sm text-muted-foreground">{t("metrics.selectLabel")}</p>
                ) : values.isLoading ? (
                  <div className="flex items-center justify-center gap-2 py-16 text-sm text-muted-foreground"><Loader2 className="size-4 animate-spin" />{t("common.loading")}</div>
                ) : (values.data ?? []).map((value) => (
                  <button
                    key={value}
                    type="button"
                    className="block w-full rounded px-2 py-1.5 text-left font-mono text-xs hover:bg-muted"
                    onClick={() => setMatchers((current) => ({ ...current, [selectedLabel]: value }))}
                  >
                    &quot;{value}&quot;
                  </button>
                ))}
              </div>
            </div>
          </div>
        )}
      </DialogContent>
    </Dialog>
  );
}
