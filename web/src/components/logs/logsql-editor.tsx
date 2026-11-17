import { useEffect, useRef } from "react";
import {
  acceptCompletion,
  autocompletion,
  closeBrackets,
  closeBracketsKeymap,
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
import { bracketMatching } from "@codemirror/language";
import { Annotation, EditorSelection, EditorState, Prec } from "@codemirror/state";
import { EditorView, keymap, placeholder, type ViewUpdate } from "@codemirror/view";
import { Terminal } from "lucide-react";
import { useTranslation } from "react-i18next";
import {
  analyzeLogsQLContext,
  LOGSQL_PIPES,
  type LogsAutocompleteContext,
} from "@/lib/logs-explorer";

export interface LogsQLEditorSuggestion {
  value: string;
  label?: string;
  detail?: string;
  info?: string;
  kind: "field" | "value" | "pipe";
}

interface LogsQLEditorProps {
  id: string;
  value: string;
  onChange: (value: string) => void;
  onRun: () => void;
  autocompleteEnabled?: boolean;
  onComplete?: (
    context: LogsAutocompleteContext,
    signal: AbortSignal,
    query: string,
  ) => Promise<LogsQLEditorSuggestion[]>;
  onHistoryPrevious?: () => void;
  onHistoryNext?: () => void;
}

const externalValue = Annotation.define<boolean>();

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
    minHeight: "3.75rem",
    maxHeight: "9rem",
    padding: "0.3125rem 0.75rem 0.3125rem 2.5rem",
    caretColor: "var(--foreground)",
  },
  ".cm-line": { padding: "0" },
  ".cm-cursor": { borderLeftColor: "var(--foreground)" },
  ".cm-placeholder": { color: "var(--muted-foreground)" },
  ".cm-selectionBackground, &.cm-focused .cm-selectionBackground, ::selection": {
    backgroundColor: "var(--accent) !important",
  },
  ".cm-tooltip-autocomplete": {
    border: "1px solid var(--border)",
    borderRadius: "var(--radius)",
    backgroundColor: "var(--popover)",
    color: "var(--popover-foreground)",
    boxShadow: "0 10px 24px color-mix(in oklab, black 16%, transparent)",
    overflow: "hidden",
  },
  ".cm-tooltip-autocomplete > ul": {
    minWidth: "18rem",
    maxWidth: "min(34rem, calc(100vw - 2rem))",
    fontFamily: "ui-monospace, SFMono-Regular, Menlo, Monaco, Consolas, monospace",
  },
  ".cm-tooltip-autocomplete > ul > li": {
    padding: "0.35rem 0.6rem",
  },
  ".cm-tooltip-autocomplete > ul > li[aria-selected]": {
    backgroundColor: "var(--accent)",
    color: "var(--accent-foreground)",
  },
});

const LOCAL_PIPE_COMPLETIONS: LogsQLEditorSuggestion[] = LOGSQL_PIPES.map((value) => ({
  value,
  kind: "pipe",
  detail: "LogsQL pipe",
}));

function toCompletion(suggestion: LogsQLEditorSuggestion): Completion {
  return {
    label: suggestion.label ?? suggestion.value,
    apply: suggestion.value,
    detail: suggestion.detail,
    info: suggestion.info,
    type: suggestion.kind === "pipe" ? "function" : suggestion.kind === "field" ? "property" : "text",
  };
}

export function LogsQLEditor({
  id,
  value,
  onChange,
  onRun,
  autocompleteEnabled = true,
  onComplete,
  onHistoryPrevious,
  onHistoryNext,
}: LogsQLEditorProps) {
  const { t, i18n } = useTranslation();
  const containerRef = useRef<HTMLDivElement>(null);
  const viewRef = useRef<EditorView | null>(null);
  const onChangeRef = useRef(onChange);
  const onRunRef = useRef(onRun);
  const onCompleteRef = useRef(onComplete);
  const onHistoryPreviousRef = useRef(onHistoryPrevious);
  const onHistoryNextRef = useRef(onHistoryNext);
  const autocompleteEnabledRef = useRef(autocompleteEnabled);
  const tRef = useRef(t);
  // The editor is created once per id; later value changes are synced by the
  // effect below, so creation reads the latest value through a ref.
  const valueRef = useRef(value);

  onChangeRef.current = onChange;
  onRunRef.current = onRun;
  onCompleteRef.current = onComplete;
  onHistoryPreviousRef.current = onHistoryPrevious;
  onHistoryNextRef.current = onHistoryNext;
  autocompleteEnabledRef.current = autocompleteEnabled;
  tRef.current = t;
  valueRef.current = value;

  useEffect(() => {
    if (!containerRef.current) return;

    const state = EditorState.create({
      doc: valueRef.current,
      extensions: [
        editorTheme,
        history(),
        bracketMatching(),
        closeBrackets(),
        EditorView.lineWrapping,
        placeholder(tRef.current("logs.queryPlaceholder")),
        autocompletion({
          activateOnTyping: true,
          maxRenderedOptions: 80,
          override: [async (completionContext: CompletionContext): Promise<CompletionResult | null> => {
            if (!autocompleteEnabledRef.current) return null;
            const query = completionContext.state.doc.toString();
            const context = analyzeLogsQLContext(query, completionContext.pos);
            if (!completionContext.explicit && !context.token) return null;

            const controller = new AbortController();
            completionContext.addEventListener("abort", () => controller.abort());
            const local = context.kind === "pipe" ? LOCAL_PIPE_COMPLETIONS : [];
            let remote: LogsQLEditorSuggestion[] = [];
            try {
              remote = await onCompleteRef.current?.(
                context,
                controller.signal,
                query,
              ) ?? [];
            } catch (error) {
              if (!(error instanceof DOMException && error.name === "AbortError")) {
                console.error("LogsQL autocomplete failed", error);
              }
            }
            const token = context.token.toLowerCase();
            const seen = new Set<string>();
            const options = [...local, ...remote]
              .filter((suggestion) => (
                !token || (suggestion.label ?? suggestion.value).toLowerCase().includes(token)
              ))
              .filter((suggestion) => {
                const key = `${suggestion.kind}:${suggestion.value}`;
                if (seen.has(key)) return false;
                seen.add(key);
                return true;
              })
              .map(toCompletion);
            return options.length ? { from: context.from, to: context.to, options } : null;
          }],
        }),
        EditorView.contentAttributes.of({
          "aria-label": tRef.current("logs.queryLabel"),
          "data-logsql-editor": id,
          spellcheck: "false",
        }),
        Prec.highest(keymap.of([
          {
            key: "Enter",
            run: (editor) => {
              if (acceptCompletion(editor)) return true;
              onRunRef.current();
              return true;
            },
          },
          { key: "Shift-Enter", run: insertNewlineAndIndent },
          {
            key: "Mod-ArrowUp",
            run: () => {
              onHistoryPreviousRef.current?.();
              return Boolean(onHistoryPreviousRef.current);
            },
          },
          {
            key: "Mod-ArrowDown",
            run: () => {
              onHistoryNextRef.current?.();
              return Boolean(onHistoryNextRef.current);
            },
          },
        ])),
        keymap.of([
          ...closeBracketsKeymap,
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
    viewRef.current?.contentDOM.setAttribute("aria-label", t("logs.queryLabel"));
  }, [i18n.language, t]);

  return (
    <div className="relative min-w-0 flex-1">
      <Terminal className="pointer-events-none absolute left-3 top-[1.125rem] z-10 size-4 -translate-y-1/2 text-muted-foreground" />
      <div ref={containerRef} />
    </div>
  );
}
