import { useEffect, useRef, useState } from "react";
import { useTranslation } from "react-i18next";
import { useQuery } from "@tanstack/react-query";
import { useNavigate } from "@tanstack/react-router";
import {
  AlertCircle,
  Bot,
  Check,
  Copy,
  Loader2,
  LineChart,
  ScrollText,
  SendHorizontal,
  Settings,
  Sparkles,
  Trash2,
} from "lucide-react";
import { getAiStatus, type AiSignal, type AssistantAnswer } from "@/lib/api/ai";
import { RANGE_PRESETS, DEFAULT_RANGE_ID } from "@/lib/api/ranges";
import { formatRelativeTime } from "@/lib/format";
import { LevelDot } from "@/components/logs/level-indicator";
import { TimeSeriesChart } from "@/components/charts/time-series-chart";
import { EmptyState } from "@/components/common/empty-state";
import { useAssistant, type AssistantTurn } from "./assistant-provider";
import { Sheet, SheetContent } from "@/components/ui/sheet";
import { Button } from "@/components/ui/button";
import { Textarea } from "@/components/ui/textarea";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";

const LOGS_PREVIEW_ROWS = 8;

/**
 * Global AI assistant panel (right-side Sheet, opened from the top bar). Chosen
 * over a standalone /assistant route because the assistant is contextual and
 * should be reachable from any page without a navigation that discards the
 * user's current view (rationale mirrored in web/README.md). Multi-turn history
 * lives in AssistantProvider (persisted to localStorage).
 */
export function AssistantPanel() {
  const { t } = useTranslation();
  const { open, setOpen, turns, send, clear } = useAssistant();

  const statusQuery = useQuery({
    queryKey: ["ai-status"],
    queryFn: ({ signal }) => getAiStatus(signal),
    // Re-check whenever the panel is (re)opened so a config flip is reflected.
    enabled: open,
    staleTime: 10_000,
  });

  const [question, setQuestion] = useState("");
  const [signal, setSignal] = useState<AiSignal>("auto");
  const [rangeId, setRangeId] = useState(DEFAULT_RANGE_ID);

  const scrollRef = useRef<HTMLDivElement>(null);
  useEffect(() => {
    if (scrollRef.current) scrollRef.current.scrollTop = scrollRef.current.scrollHeight;
  }, [turns, open]);

  const aiEnabled = statusQuery.data?.enabled ?? true;

  function submit() {
    if (!question.trim() || !aiEnabled) return;
    send(question, signal, rangeId);
    setQuestion("");
  }

  return (
    <Sheet open={open} onOpenChange={setOpen}>
      <SheetContent
        side="right"
        hideClose
        className="w-full gap-0 sm:max-w-xl"
        aria-describedby={undefined}
      >
        {/* Header */}
        <div className="flex items-center justify-between border-b border-border p-4">
          <div className="flex items-center gap-2">
            <div className="flex size-7 items-center justify-center rounded-md bg-primary/10 text-primary">
              <Sparkles className="size-4" />
            </div>
            <div>
              <h2 className="text-sm font-semibold tracking-tight">{t("assistant.title")}</h2>
              <p className="text-xs text-muted-foreground">{t("assistant.subtitle")}</p>
            </div>
          </div>
          <div className="flex items-center gap-1">
            {turns.length > 0 && (
              <Button
                variant="ghost"
                size="icon"
                className="size-8"
                onClick={clear}
                aria-label={t("assistant.clearHistory")}
                title={t("assistant.clearHistory")}
              >
                <Trash2 className="size-4" />
              </Button>
            )}
            <Button
              variant="ghost"
              size="icon"
              className="size-8"
              onClick={() => setOpen(false)}
              aria-label={t("common.close")}
            >
              <span aria-hidden>✕</span>
            </Button>
          </div>
        </div>

        {/* Conversation */}
        <div ref={scrollRef} className="flex-1 space-y-4 overflow-y-auto p-4">
          {!aiEnabled && !statusQuery.isLoading ? (
            <DisabledGuide />
          ) : turns.length === 0 ? (
            <EmptyState
              className="py-20"
              icon={<Bot className="size-6" />}
              title={t("assistant.emptyTitle")}
              desc={t("assistant.emptyDesc")}
            />
          ) : (
            turns.map((turn) => <TurnView key={turn.id} turn={turn} onClose={() => setOpen(false)} />)
          )}
        </div>

        {/* Composer */}
        <div className="space-y-2 border-t border-border p-3">
          <div className="flex items-center gap-2">
            <Select value={signal} onValueChange={(v) => setSignal(v as AiSignal)}>
              <SelectTrigger className="h-8 w-28" aria-label={t("assistant.signalLabel")}>
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                <SelectItem value="auto">{t("assistant.signalAuto")}</SelectItem>
                <SelectItem value="metrics">{t("assistant.signalMetrics")}</SelectItem>
                <SelectItem value="logs">{t("assistant.signalLogs")}</SelectItem>
              </SelectContent>
            </Select>
            <Select value={rangeId} onValueChange={setRangeId}>
              <SelectTrigger className="h-8 w-32" aria-label={t("assistant.timeRange")}>
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                {RANGE_PRESETS.map((r) => (
                  <SelectItem key={r.id} value={r.id}>
                    {t(r.labelKey)}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
          </div>
          <div className="flex items-end gap-2">
            <Textarea
              value={question}
              onChange={(e) => setQuestion(e.target.value)}
              onKeyDown={(e) => {
                if (e.key === "Enter" && !e.shiftKey) {
                  e.preventDefault();
                  submit();
                }
              }}
              disabled={!aiEnabled}
              rows={2}
              placeholder={t("assistant.inputPlaceholder")}
              className="min-h-[44px] resize-none text-sm"
              spellCheck={false}
            />
            <Button
              size="icon"
              className="size-9 shrink-0"
              onClick={submit}
              disabled={!question.trim() || !aiEnabled}
              aria-label={t("assistant.send")}
            >
              <SendHorizontal className="size-4" />
            </Button>
          </div>
        </div>
      </SheetContent>
    </Sheet>
  );
}

function DisabledGuide() {
  const { t } = useTranslation();
  const navigate = useNavigate();
  const { setOpen } = useAssistant();
  return (
    <EmptyState
      className="py-20"
      icon={<Sparkles className="size-6" />}
      title={t("assistant.disabledTitle")}
      desc={t("assistant.disabledDesc")}
      action={
        <Button
          variant="outline"
          size="sm"
          onClick={() => {
            setOpen(false);
            navigate({ to: "/settings/ai" });
          }}
        >
          <Settings className="size-4" />
          {t("assistant.goToSettings")}
        </Button>
      }
    />
  );
}

function TurnView({ turn, onClose }: { turn: AssistantTurn; onClose: () => void }) {
  const { t } = useTranslation();
  return (
    <div className="space-y-2">
      {/* User question */}
      <div className="flex justify-end">
        <div className="max-w-[85%] rounded-lg rounded-br-sm bg-primary px-3 py-2 text-sm text-primary-foreground">
          {turn.question}
        </div>
      </div>

      {/* Assistant response */}
      <div className="flex gap-2">
        <div className="mt-0.5 flex size-6 shrink-0 items-center justify-center rounded-md bg-muted text-muted-foreground">
          <Sparkles className="size-3.5" />
        </div>
        <div className="min-w-0 flex-1">
          {turn.status === "pending" ? (
            <div className="flex items-center gap-2 py-2 text-sm text-muted-foreground">
              <Loader2 className="size-4 animate-spin" />
              {t("assistant.thinking")}
            </div>
          ) : turn.status === "error" ? (
            <div className="flex items-start gap-2 rounded-md border border-destructive/30 bg-destructive/10 px-3 py-2 text-sm text-destructive">
              <AlertCircle className="mt-0.5 size-4 shrink-0" />
              <span className="min-w-0 break-words">
                {turn.errorStatus === 429 ? t("assistant.rateLimited") : turn.errorMessage}
              </span>
            </div>
          ) : turn.answer ? (
            <AnswerView answer={turn.answer} onClose={onClose} />
          ) : null}
        </div>
      </div>
    </div>
  );
}

function AnswerView({ answer, onClose }: { answer: AssistantAnswer; onClose: () => void }) {
  const { t } = useTranslation();
  const navigate = useNavigate();
  const isMetrics = answer.signal === "metrics";

  function openInQueryPage() {
    onClose();
    if (isMetrics) {
      navigate({ to: "/metrics", search: { q: answer.query, run: "1" } });
    } else {
      navigate({ to: "/logs", search: { q: answer.query, range: DEFAULT_RANGE_ID } });
    }
  }

  return (
    <div className="space-y-3">
      {/* Signal badge */}
      <span className="inline-flex items-center gap-1.5 rounded border border-border bg-muted/50 px-1.5 py-0.5 text-[11px] font-medium text-muted-foreground">
        {isMetrics ? <LineChart className="size-3" /> : <ScrollText className="size-3" />}
        {isMetrics ? t("assistant.metricsBadge") : t("assistant.logsBadge")}
      </span>

      {/* Generated query */}
      <div className="space-y-1.5">
        <div className="flex items-center justify-between">
          <span className="text-xs font-medium text-muted-foreground">
            {t("assistant.generatedQuery")}
          </span>
          <div className="flex items-center gap-1">
            <CopyButton value={answer.query} />
            <Button variant="outline" size="sm" className="h-7 gap-1.5 px-2 text-xs" onClick={openInQueryPage}>
              {isMetrics ? <LineChart className="size-3" /> : <ScrollText className="size-3" />}
              {isMetrics ? t("assistant.openInMetrics") : t("assistant.openInLogs")}
            </Button>
          </div>
        </div>
        <pre className="overflow-x-auto rounded-md border border-border bg-muted/40 px-3 py-2 font-mono text-xs text-foreground">
          {answer.query}
        </pre>
      </div>

      {/* Explanation */}
      <div className="space-y-1">
        <span className="text-xs font-medium text-muted-foreground">{t("assistant.explanation")}</span>
        <p className="text-sm leading-relaxed text-foreground/90">{answer.explanation}</p>
      </div>

      {/* Inline result / execution error */}
      {answer.executionError ? (
        <div className="space-y-1.5 rounded-md border border-vital-ni/30 bg-vital-ni/10 px-3 py-2">
          <div className="flex items-center gap-1.5 text-xs font-medium text-vital-ni">
            <AlertCircle className="size-3.5" />
            {t("assistant.executionErrorTitle")}
          </div>
          <p className="font-mono text-xs text-foreground/80">{answer.executionError}</p>
          <p className="text-xs text-muted-foreground">{t("assistant.executionErrorHint")}</p>
        </div>
      ) : answer.result ? (
        <ResultView answer={answer} />
      ) : null}
    </div>
  );
}

function ResultView({ answer }: { answer: AssistantAnswer }) {
  const { t, i18n } = useTranslation();
  const result = answer.result;
  if (!result) return null;

  if (result.kind === "metrics") {
    if (result.series.length === 0) {
      return <p className="text-xs text-muted-foreground">{t("assistant.noResult")}</p>;
    }
    return (
      <div className="space-y-1.5">
        <span className="text-xs font-medium text-muted-foreground">{t("assistant.result")}</span>
        <div className="rounded-md border border-border p-2">
          <TimeSeriesChart series={result.series} height={180} />
        </div>
      </div>
    );
  }

  // logs
  const rows = result.rows;
  if (rows.length === 0) {
    return <p className="text-xs text-muted-foreground">{t("assistant.noResult")}</p>;
  }
  const shown = rows.slice(0, LOGS_PREVIEW_ROWS);
  const extra = rows.length - shown.length;
  return (
    <div className="space-y-1.5">
      <span className="text-xs font-medium text-muted-foreground">{t("assistant.result")}</span>
      <div className="divide-y divide-border rounded-md border border-border">
        {shown.map((row) => (
          <div key={row.id} className="flex items-start gap-2 px-2.5 py-1.5 text-xs">
            <span className="shrink-0 tabular-nums text-muted-foreground">
              {formatRelativeTime(row.time, i18n.language)}
            </span>
            <LevelDot level={row.level} className="mt-1" />
            <span className="min-w-0 flex-1 truncate font-mono text-foreground/90" title={row.message}>
              {row.message}
            </span>
          </div>
        ))}
      </div>
      {extra > 0 && (
        <p className="text-xs text-muted-foreground">{t("assistant.logsMore", { count: extra })}</p>
      )}
    </div>
  );
}

function CopyButton({ value }: { value: string }) {
  const { t } = useTranslation();
  const [copied, setCopied] = useState(false);
  return (
    <Button
      variant="ghost"
      size="sm"
      className="h-7 gap-1.5 px-2 text-xs"
      onClick={() => {
        void navigator.clipboard?.writeText(value);
        setCopied(true);
        setTimeout(() => setCopied(false), 1200);
      }}
    >
      {copied ? <Check className="size-3" /> : <Copy className="size-3" />}
      {copied ? t("common.copied") : t("common.copy")}
    </Button>
  );
}
