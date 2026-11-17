import {
  forwardRef,
  useEffect,
  useImperativeHandle,
  useRef,
  type ReactNode,
} from "react";
import {
  acceptCompletion,
  autocompletion,
  closeBrackets,
  closeBracketsKeymap,
  closeCompletion,
  completionKeymap,
  completionStatus,
  pickedCompletion,
  selectedCompletionIndex,
  type Completion,
  type CompletionContext,
  type CompletionResult,
} from "@codemirror/autocomplete";
import {
  defaultKeymap,
  history,
  historyKeymap,
  insertNewlineAndIndent,
} from "@codemirror/commands";
import {
  bracketMatching,
  HighlightStyle,
  LRLanguage,
  syntaxHighlighting,
} from "@codemirror/language";
import {
  Annotation,
  EditorSelection,
  EditorState,
  Prec,
} from "@codemirror/state";
import {
  EditorView,
  keymap,
  type ViewUpdate,
} from "@codemirror/view";
import { tags } from "@lezer/highlight";
import { parser } from "@prometheus-io/lezer-promql";
import { Terminal } from "lucide-react";
import { useTranslation } from "react-i18next";
import { labelNames, labelValues, metricNames } from "@/lib/api/metrics";
import { getProjectSlug } from "@/lib/settings";
import {
  analyzePromQLContext,
  insertPromQLSuggestion,
  METRICSQL_FUNCTIONS,
  metricsAutocompleteRange,
  type PromQLContext,
  type PromQLSuggestion,
  type ResolvedMetricsRange,
} from "@/lib/metrics-query";

export interface PromQLEditorHandle {
  insertText: (text: string) => void;
  focus: () => void;
}

interface PromQLEditorProps {
  id: string;
  value: string;
  range: ResolvedMetricsRange;
  actions?: ReactNode;
  onChange: (value: string) => void;
  onRun: () => void;
}

const MAX_SUGGESTIONS = 50;
const AUTOCOMPLETE_QUERY_LIMIT = 1_000;
const AUTOCOMPLETE_CACHE_LIMIT = 1_000;
const externalValue = Annotation.define<boolean>();
const autocompleteCache = new Map<string, string[]>();

const promQLLanguage = LRLanguage.define({
  parser: parser.configure({ top: "PromQL" }),
  languageData: {
    closeBrackets: { brackets: ["(", "[", "{", "'", "\"", "`"] },
    commentTokens: { line: "#" },
  },
});

const promQLHighlight = HighlightStyle.define([
  { tag: tags.keyword, color: "var(--primary)", fontWeight: "600" },
  { tag: [tags.function(tags.variableName), tags.function(tags.propertyName)], color: "var(--chart-2)" },
  { tag: [tags.string, tags.regexp], color: "var(--chart-3)" },
  { tag: [tags.number, tags.bool], color: "var(--chart-4)" },
  { tag: [tags.operator, tags.punctuation], color: "var(--muted-foreground)" },
  { tag: tags.comment, color: "var(--muted-foreground)", fontStyle: "italic" },
]);

const editorTheme = EditorView.theme({
  "&": {
    width: "100%",
    minHeight: "2.25rem",
    border: "1px solid var(--input)",
    borderRadius: "calc(var(--radius) - 2px)",
    backgroundColor: "var(--background)",
    color: "var(--foreground)",
    fontSize: "0.875rem",
  },
  "&.cm-focused": {
    outline: "none",
    borderColor: "var(--ring)",
    boxShadow: "0 0 0 2px color-mix(in oklab, var(--ring) 20%, transparent)",
  },
  ".cm-scroller": {
    overflow: "auto",
    fontFamily: "ui-monospace, SFMono-Regular, Menlo, Monaco, Consolas, monospace",
    lineHeight: "1.5rem",
  },
  ".cm-content": {
    minHeight: "2.125rem",
    padding: "0.3125rem 2.75rem 0.3125rem 2.5rem",
    caretColor: "var(--foreground)",
  },
  ".cm-line": { padding: "0" },
  ".cm-cursor": { borderLeftColor: "var(--foreground)" },
  ".cm-selectionBackground, &.cm-focused .cm-selectionBackground, ::selection": {
    backgroundColor: "var(--accent) !important",
  },
  ".cm-tooltip": {
    border: "1px solid var(--border)",
    borderRadius: "calc(var(--radius) - 2px)",
    backgroundColor: "var(--popover)",
    color: "var(--popover-foreground)",
    boxShadow: "0 10px 30px color-mix(in oklab, var(--foreground) 12%, transparent)",
    overflow: "hidden",
  },
  ".cm-tooltip-autocomplete > ul": {
    maxHeight: "18rem",
    fontFamily: "ui-monospace, SFMono-Regular, Menlo, Monaco, Consolas, monospace",
  },
  ".cm-tooltip-autocomplete > ul > li": {
    minHeight: "2rem",
    padding: "0.375rem 0.625rem",
  },
  ".cm-tooltip-autocomplete > ul > li[aria-selected]": {
    backgroundColor: "var(--accent)",
    color: "var(--accent-foreground)",
  },
  ".cm-completionDetail": {
    marginLeft: "auto",
    color: "var(--muted-foreground)",
    fontFamily: "ui-sans-serif, system-ui, sans-serif",
    fontSize: "0.625rem",
    textTransform: "uppercase",
  },
  ".cm-completionIcon": { color: "var(--muted-foreground)" },
});

const escapeMatcher = (value: string) => value.replace(/\\/g, "\\\\").replace(/"/g, '\\"');
const escapeRegex = (value: string) =>
  value.replace(/[\\.*+?()[\]{}|^$]/g, "\\$&").replace(/"/g, '\\"');

function cacheGet(key: string) {
  const value = autocompleteCache.get(key);
  if (!value) return undefined;
  autocompleteCache.delete(key);
  autocompleteCache.set(key, value);
  return value;
}

function cachePut(key: string, value: string[]) {
  autocompleteCache.delete(key);
  autocompleteCache.set(key, value);
  while (autocompleteCache.size > AUTOCOMPLETE_CACHE_LIMIT) {
    const oldest = autocompleteCache.keys().next().value;
    if (oldest === undefined) break;
    autocompleteCache.delete(oldest);
  }
}

async function fetchSuggestions(
  context: PromQLContext,
  range: ResolvedMetricsRange,
  project: string,
  signal: AbortSignal,
) {
  const autocompleteRange = metricsAutocompleteRange(range);
  const base = {
    ...autocompleteRange,
    limit: AUTOCOMPLETE_QUERY_LIMIT,
  };
  const metric = context.metric ? `__name__="${escapeMatcher(context.metric)}"` : "";
  const escapedToken = escapeRegex(context.token);
  let cacheKey = "";
  let fetcher: (() => Promise<string[]>) | undefined;

  if (context.kind === "metric" && context.token) {
    const match = `{__name__=~".*${escapedToken}.*"}`;
    cacheKey = JSON.stringify({ project, kind: context.kind, ...base, match });
    fetcher = () => metricNames({ ...base, match }, signal);
  } else if (context.kind === "label") {
    const matchers = [metric, ...context.matchers].filter(Boolean).join(",");
    const match = matchers ? `{${matchers}}` : undefined;
    cacheKey = JSON.stringify({ project, kind: context.kind, ...base, match });
    fetcher = () => labelNames({ ...base, match }, signal);
  } else if (context.kind === "value" && context.label) {
    const currentValue = `${context.label}=~".*${escapedToken}.*"`;
    const match = `{${[metric, ...context.matchers, currentValue].filter(Boolean).join(",")}}`;
    cacheKey = JSON.stringify({ project, kind: context.kind, label: context.label, ...base, match });
    fetcher = () => labelValues(context.label, { ...base, match }, signal);
  }

  if (!fetcher) return [];
  const cached = cacheGet(cacheKey);
  if (cached) return cached;
  const result = await fetcher();
  cachePut(cacheKey, result);
  return result;
}

function applySuggestion(
  view: EditorView,
  completion: Completion,
  suggestion: PromQLSuggestion,
) {
  const query = view.state.doc.toString();
  const cursor = view.state.selection.main.head;
  const context = analyzePromQLContext(query, cursor);
  const next = insertPromQLSuggestion(query, cursor, context, suggestion);
  view.dispatch({
    changes: { from: 0, to: view.state.doc.length, insert: next.query },
    selection: EditorSelection.cursor(next.cursor),
    annotations: pickedCompletion.of(completion),
    scrollIntoView: true,
  });
}

function completionOption(
  suggestion: PromQLSuggestion,
  detail: string,
): Completion {
  return {
    label: suggestion.value,
    type: suggestion.kind === "function"
      ? "function"
      : suggestion.kind === "metric"
        ? "variable"
        : suggestion.kind === "label"
          ? "property"
          : "constant",
    detail,
    boost: suggestion.kind === "metric" ? 1 : 0,
    apply: (view, completion) => applySuggestion(view, completion, suggestion),
  };
}

export const PromQLEditor = forwardRef<PromQLEditorHandle, PromQLEditorProps>(function PromQLEditor({
  id,
  value,
  range,
  actions,
  onChange,
  onRun,
}, forwardedRef) {
  const { t, i18n } = useTranslation();
  const containerRef = useRef<HTMLDivElement>(null);
  const viewRef = useRef<EditorView | null>(null);
  const onChangeRef = useRef(onChange);
  const onRunRef = useRef(onRun);
  const rangeRef = useRef(range);
  const projectRef = useRef(getProjectSlug());
  const tRef = useRef(t);
  // The editor is created once per id; later value changes are synced by the
  // effect below, so creation reads the latest value through a ref.
  const valueRef = useRef(value);

  onChangeRef.current = onChange;
  onRunRef.current = onRun;
  rangeRef.current = range;
  projectRef.current = getProjectSlug();
  tRef.current = t;
  valueRef.current = value;

  useEffect(() => {
    if (!containerRef.current) return;

    const completionSource = async (cmContext: CompletionContext): Promise<CompletionResult | null> => {
      const query = cmContext.state.doc.toString();
      const context = analyzePromQLContext(query, cmContext.pos);
      if (context.kind === "none" || (!cmContext.explicit && !context.token)) return null;

      const controller = new AbortController();
      cmContext.addEventListener("abort", () => controller.abort(), { onDocChange: true });

      let remote: string[] = [];
      try {
        remote = await fetchSuggestions(
          context,
          rangeRef.current,
          projectRef.current,
          controller.signal,
        );
      } catch (error) {
        if (controller.signal.aborted) return null;
        console.error("Unable to load PromQL suggestions:", error);
      }
      if (cmContext.aborted) return null;

      const remoteKind = context.kind === "metric"
        ? "metric"
        : context.kind === "label"
          ? "label"
          : "value";
      const token = context.token.toLowerCase();
      const remoteSuggestions: PromQLSuggestion[] = remote
        .filter((item) => remoteKind !== "label" || item !== "project_id")
        .filter((item) => !token || item.toLowerCase().includes(token))
        .map((item) => ({ value: item, kind: remoteKind }));
      const functionSuggestions: PromQLSuggestion[] = context.kind === "metric"
        ? METRICSQL_FUNCTIONS
          .filter((item) => !token || item.toLowerCase().includes(token))
          .map((item) => ({ value: item, kind: "function" }))
        : [];
      const seen = new Set<string>();
      const options = [...remoteSuggestions, ...functionSuggestions]
        .filter((item) => {
          const key = `${item.kind}:${item.value}`;
          if (seen.has(key)) return false;
          seen.add(key);
          return true;
        })
        .map((item) => completionOption(
          item,
          tRef.current(`metrics.suggestionKinds.${item.kind}`),
        ))
        .slice(0, MAX_SUGGESTIONS);

      return {
        from: context.tokenStart,
        options,
      };
    };

    const state = EditorState.create({
      doc: valueRef.current,
      extensions: [
        promQLLanguage,
        syntaxHighlighting(promQLHighlight),
        editorTheme,
        history(),
        bracketMatching(),
        closeBrackets(),
        EditorView.lineWrapping,
        EditorView.contentAttributes.of({
          "aria-label": tRef.current("metrics.queryLabel"),
          "aria-autocomplete": "list",
          "data-promql-editor": id,
          spellcheck: "false",
        }),
        autocompletion({
          override: [completionSource],
          activateOnTyping: true,
          activateOnTypingDelay: 500,
          selectOnOpen: false,
          maxRenderedOptions: MAX_SUGGESTIONS,
        }),
        Prec.highest(keymap.of([
          {
            key: "Enter",
            run: (view) => {
              if (selectedCompletionIndex(view.state) !== null) return acceptCompletion(view);
              if (completionStatus(view.state)) closeCompletion(view);
              onRunRef.current();
              return true;
            },
          },
          { key: "Shift-Enter", run: insertNewlineAndIndent },
          {
            key: "Tab",
            run: (view) => selectedCompletionIndex(view.state) !== null && acceptCompletion(view),
          },
        ])),
        keymap.of([
          ...closeBracketsKeymap,
          ...completionKeymap,
          ...defaultKeymap,
          ...historyKeymap,
        ]),
        EditorView.updateListener.of((update: ViewUpdate) => {
          if (
            update.docChanged
            && !update.transactions.some((transaction) => transaction.annotation(externalValue))
          ) {
            onChangeRef.current(update.state.doc.toString());
          }
        }),
      ],
    });
    const view = new EditorView({ state, parent: containerRef.current });
    viewRef.current = view;
    return () => {
      view.destroy();
      viewRef.current = null;
    };
  }, [id]);

  useEffect(() => {
    const view = viewRef.current;
    if (!view) return;
    const current = view.state.doc.toString();
    if (current === value) return;
    view.dispatch({
      changes: { from: 0, to: current.length, insert: value },
      selection: EditorSelection.cursor(value.length),
      annotations: externalValue.of(true),
    });
  }, [value]);

  useEffect(() => {
    viewRef.current?.contentDOM.setAttribute("aria-label", t("metrics.queryLabel"));
  }, [i18n.language, t]);

  useImperativeHandle(forwardedRef, () => ({
    insertText(text) {
      const view = viewRef.current;
      if (!view) return;
      const selection = view.state.selection.main;
      view.dispatch({
        changes: { from: selection.from, to: selection.to, insert: text },
        selection: EditorSelection.cursor(selection.from + text.length),
        scrollIntoView: true,
      });
      view.focus();
    },
    focus() {
      viewRef.current?.focus();
    },
  }), []);

  return (
    <div className="relative min-w-0 flex-1">
      <Terminal className="pointer-events-none absolute left-3 top-[1.125rem] z-10 size-4 -translate-y-1/2 text-muted-foreground" />
      <div ref={containerRef} />
      {actions && (
        <div className="absolute right-1 top-[1.125rem] z-10 -translate-y-1/2">
          {actions}
        </div>
      )}
    </div>
  );
});
