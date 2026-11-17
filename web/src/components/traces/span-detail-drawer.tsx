import { useTranslation } from "react-i18next";
import { ScrollText } from "lucide-react";
import type { JaegerSpan, JaegerTrace } from "@/lib/api/traces";
import { isErrorSpan } from "@/lib/api/traces";
import { bounds } from "@/lib/trace-tree";
import { formatDurationMicros, formatPercent } from "@/lib/format";
import { extractGenAISpanDetails } from "@/lib/api/llm-contract";
import {
  Sheet,
  SheetContent,
  SheetHeader,
  SheetTitle,
  SheetDescription,
} from "@/components/ui/sheet";
import { Button } from "@/components/ui/button";
import { Badge } from "@/components/ui/badge";

/**
 * Right-side drawer for a single span: identity + timing summary, its tag set,
 * and its process (service) metadata. "View related logs" jumps to the Logs
 * page with the query pre-filled by this span's trace_id.
 */

interface SpanDetailDrawerProps {
  span: JaegerSpan | null;
  trace: JaegerTrace;
  open: boolean;
  onOpenChange: (open: boolean) => void;
  onViewLogs: () => void;
}

function KeyValueTable({ rows }: { rows: Array<{ key: string; value: string }> }) {
  return (
    <table className="w-full border-collapse text-xs">
      <tbody>
        {rows.map((r, i) => (
          <tr key={`${r.key}-${i}`} className="border-b border-border/60 align-top">
            <td className="w-[38%] py-1.5 pr-3 font-mono text-muted-foreground">{r.key}</td>
            <td className="break-all py-1.5 font-mono text-foreground">{r.value}</td>
          </tr>
        ))}
      </tbody>
    </table>
  );
}

export function SpanDetailDrawer({
  span,
  trace,
  open,
  onOpenChange,
  onViewLogs,
}: SpanDetailDrawerProps) {
  const { t } = useTranslation();
  const { start, total } = bounds(trace);
  const process = span ? trace.processes[span.processID] : undefined;
  const service = process?.serviceName ?? span?.processID ?? "";
  const errored = span ? isErrorSpan(span) : false;
  const genAI = span ? extractGenAISpanDetails(span.tags) : null;

  return (
    <Sheet open={open} onOpenChange={onOpenChange}>
      <SheetContent className="w-full sm:max-w-lg" hideClose>
        {span && (
          <>
            <SheetHeader>
              <div className="flex items-start justify-between gap-3 pr-2">
                <div className="min-w-0 space-y-1">
                  <SheetTitle className="flex items-center gap-2">
                    <span className="truncate">{span.operationName}</span>
                    {errored && (
                      <Badge variant="destructive" className="shrink-0">
                        {t("traces.errorBadge")}
                      </Badge>
                    )}
                  </SheetTitle>
                  <SheetDescription>{service}</SheetDescription>
                </div>
                <div className="flex shrink-0 flex-col items-end gap-1">
                  <Badge variant="outline">{t("traces.exactEvidence")}</Badge>
                  <Button size="sm" variant="outline" onClick={onViewLogs}>
                    <ScrollText className="size-4" />
                    {t("traces.viewExactLogs")}
                  </Button>
                </div>
              </div>
            </SheetHeader>

            <div className="min-h-0 flex-1 space-y-5 overflow-auto px-4 pb-4">
              {/* Summary */}
              <div className="grid grid-cols-2 gap-3">
                <Summary label={t("traces.spanDuration")} value={formatDurationMicros(span.duration)} />
                <Summary
                  label={t("traces.spanSelfPercent")}
                  value={formatPercent(span.duration / total)}
                />
                <Summary
                  label={t("traces.spanStart")}
                  value={`+${formatDurationMicros(span.startTime - start)}`}
                />
                <Summary label={t("traces.spanId")} value={span.spanID} mono />
              </div>

              {/* Tags */}
              {genAI && (
                <section className="space-y-2">
                  <h3 className="text-xs font-semibold uppercase tracking-wider text-muted-foreground">
                    {t("traces.genAiSection")}
                  </h3>
                  <KeyValueTable
                    rows={[
                      { key: t("traces.genAiOperation"), value: genAI.operation },
                      ...(genAI.provider ? [{ key: t("traces.genAiProvider"), value: genAI.provider }] : []),
                      ...(genAI.requestModel ? [{ key: t("traces.genAiRequestModel"), value: genAI.requestModel }] : []),
                      ...(genAI.responseModel ? [{ key: t("traces.genAiResponseModel"), value: genAI.responseModel }] : []),
                      ...(genAI.inputTokens !== undefined ? [{ key: t("traces.genAiInputTokens"), value: String(genAI.inputTokens) }] : []),
                      ...(genAI.outputTokens !== undefined ? [{ key: t("traces.genAiOutputTokens"), value: String(genAI.outputTokens) }] : []),
                    ]}
                  />
                  {genAI.hasCapturedContent && (
                    <details className="rounded-md border border-vital-ni/40 bg-vital-ni/5 p-3">
                      <summary className="cursor-pointer text-xs font-medium text-vital-ni">
                        {t("traces.genAiCapturedContent")}
                      </summary>
                      <p className="mt-2 text-xs text-muted-foreground">
                        {t("traces.genAiPrivacyHint")}
                      </p>
                      <div className="mt-3 space-y-3">
                        {genAI.inputContent && (
                          <CapturedContent label={t("traces.genAiInput")} value={genAI.inputContent} />
                        )}
                        {genAI.outputContent && (
                          <CapturedContent label={t("traces.genAiOutput")} value={genAI.outputContent} />
                        )}
                      </div>
                    </details>
                  )}
                </section>
              )}

              {/* Tags */}
              <section className="space-y-2">
                <h3 className="text-xs font-semibold uppercase tracking-wider text-muted-foreground">
                  {t("traces.tagsSection")}
                </h3>
                <KeyValueTable
                  rows={span.tags
                    .filter((tag) => !GEN_AI_CONTENT_TAGS.has(tag.key))
                    .map((tag) => ({ key: tag.key, value: String(tag.value) }))}
                />
              </section>

              {/* Process */}
              {process && (
                <section className="space-y-2">
                  <h3 className="text-xs font-semibold uppercase tracking-wider text-muted-foreground">
                    {t("traces.processSection")}
                  </h3>
                  <KeyValueTable
                    rows={[
                      { key: "serviceName", value: process.serviceName },
                      ...process.tags.map((tag) => ({ key: tag.key, value: String(tag.value) })),
                    ]}
                  />
                </section>
              )}
            </div>
          </>
        )}
      </SheetContent>
    </Sheet>
  );
}

const GEN_AI_CONTENT_TAGS = new Set([
  "gen_ai.input.messages",
  "gen_ai.output.messages",
  "gen_ai.prompt",
  "gen_ai.completion",
]);

function CapturedContent({ label, value }: { label: string; value: string }) {
  return (
    <div className="space-y-1">
      <div className="text-xs font-medium text-muted-foreground">{label}</div>
      <pre className="max-h-48 overflow-auto whitespace-pre-wrap break-all rounded bg-muted p-2 text-xs text-foreground">
        {value}
      </pre>
    </div>
  );
}

function Summary({ label, value, mono }: { label: string; value: string; mono?: boolean }) {
  return (
    <div className="space-y-0.5">
      <div className="text-xs text-muted-foreground">{label}</div>
      <div className={mono ? "break-all font-mono text-xs" : "text-sm font-medium"}>{value}</div>
    </div>
  );
}
