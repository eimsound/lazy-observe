import { useMemo, useState } from "react";
import { useTranslation } from "react-i18next";
import { ChevronDown, ChevronRight } from "lucide-react";
import type { StackFrame, StackTrace } from "@/lib/api/issues";
import { cn } from "@/lib/utils";

/**
 * Stack-trace viewer. Highlights the function name, shows file:line:col, and
 * greys out non-`in_app` (framework / node_modules) frames. When both a
 * symbolicated and a raw (minified) stack are present it offers a toggle
 * between them. System frames collapse behind a single toggle, and any frame
 * with a source `context_line` expands to show it.
 */

type StackMode = "symbolicated" | "raw";

export function StacktraceViewer({
  symbolicated,
  raw,
}: {
  symbolicated: StackTrace;
  raw?: StackTrace;
}) {
  const { t } = useTranslation();
  const [mode, setMode] = useState<StackMode>("symbolicated");
  const [showSystem, setShowSystem] = useState(false);

  const active = mode === "raw" && raw ? raw : symbolicated;
  const systemCount = useMemo(
    () => active.frames.filter((f) => !f.in_app).length,
    [active],
  );

  const visible = showSystem ? active.frames : active.frames.filter((f) => f.in_app);

  if (active.frames.length === 0) {
    return <p className="text-sm text-muted-foreground">{t("issues.noStack")}</p>;
  }

  return (
    <div className="space-y-3">
      <div className="flex flex-wrap items-center justify-between gap-2">
        <h3 className="text-sm font-semibold">{t("issues.stacktrace")}</h3>
        {raw && (
          <div className="inline-flex rounded-md border border-border p-0.5 text-xs">
            <ToggleButton active={mode === "symbolicated"} onClick={() => setMode("symbolicated")}>
              {t("issues.stackSymbolicated")}
            </ToggleButton>
            <ToggleButton active={mode === "raw"} onClick={() => setMode("raw")}>
              {t("issues.stackRaw")}
            </ToggleButton>
          </div>
        )}
      </div>

      <div className="overflow-hidden rounded-md border border-border">
        {visible.map((frame, i) => (
          <FrameRow key={`${frame.filename}-${frame.lineno}-${i}`} frame={frame} />
        ))}
      </div>

      {systemCount > 0 && (
        <button
          type="button"
          onClick={() => setShowSystem((s) => !s)}
          className="text-xs font-medium text-muted-foreground hover:text-foreground"
        >
          {showSystem
            ? t("issues.hideSystemFrames")
            : t("issues.showSystemFrames", { count: systemCount })}
        </button>
      )}
    </div>
  );
}

function ToggleButton({
  active,
  onClick,
  children,
}: {
  active: boolean;
  onClick: () => void;
  children: React.ReactNode;
}) {
  return (
    <button
      type="button"
      onClick={onClick}
      className={cn(
        "rounded px-2 py-0.5 font-medium transition-colors",
        active ? "bg-primary text-primary-foreground" : "text-muted-foreground hover:text-foreground",
      )}
    >
      {children}
    </button>
  );
}

function FrameRow({ frame }: { frame: StackFrame }) {
  const [open, setOpen] = useState(false);
  const hasContext = !!frame.context_line;

  return (
    <div className={cn("border-b border-border/60 last:border-b-0", !frame.in_app && "bg-muted/30")}>
      <button
        type="button"
        disabled={!hasContext}
        onClick={() => setOpen((o) => !o)}
        className={cn(
          "flex w-full items-start gap-2 px-3 py-2 text-left text-xs",
          hasContext && "hover:bg-muted/40",
          !hasContext && "cursor-default",
        )}
      >
        {hasContext ? (
          open ? (
            <ChevronDown className="mt-0.5 size-3.5 shrink-0 text-muted-foreground" />
          ) : (
            <ChevronRight className="mt-0.5 size-3.5 shrink-0 text-muted-foreground" />
          )
        ) : (
          <span className="mt-0.5 size-3.5 shrink-0" />
        )}
        <span className="min-w-0 flex-1">
          <span
            className={cn(
              "font-mono font-semibold",
              frame.in_app ? "text-foreground" : "text-muted-foreground",
            )}
          >
            {frame.function || "<anonymous>"}
          </span>
          <span className="ml-2 break-all font-mono text-muted-foreground">
            {frame.filename}
            <span className="text-foreground/60">
              :{frame.lineno}:{frame.colno}
            </span>
          </span>
        </span>
        {!frame.in_app && (
          <span className="shrink-0 rounded bg-muted px-1.5 py-0.5 text-[10px] uppercase tracking-wide text-muted-foreground">
            system
          </span>
        )}
      </button>
      {hasContext && open && (
        <pre className="overflow-x-auto border-t border-border/60 bg-muted/40 px-3 py-2 font-mono text-xs leading-relaxed text-foreground">
          {frame.context_line}
        </pre>
      )}
    </div>
  );
}
