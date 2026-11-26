import { useState } from "react";
import { useTranslation } from "react-i18next";
import { Check, Copy } from "lucide-react";
import { Button } from "@/components/ui/button";

interface SnippetBlockProps {
  /** Short caption above the snippet, e.g. "MCP proxy command". */
  label?: string;
  /** The text to show and copy verbatim. */
  value: string;
}

/**
 * Read-only command/config snippet with a copy button. Used where a token is
 * shown once (ADR 0010): the operator copies the exact DSN, MCP command or
 * Grafana header pair instead of assembling it by hand.
 */
export function SnippetBlock({ label, value }: SnippetBlockProps) {
  const { t } = useTranslation();
  const [copied, setCopied] = useState(false);
  const [error, setError] = useState<string | null>(null);

  async function copy() {
    try {
      await navigator.clipboard.writeText(value);
      setError(null);
      setCopied(true);
      setTimeout(() => setCopied(false), 1500);
    } catch {
      // Same fallback as the token-value copy: say so, keep the text selectable.
      setError(t("common.copyFailed"));
    }
  }

  return (
    <div className="space-y-1">
      <div className="flex items-center justify-between gap-2">
        {label ? (
          <span className="text-xs font-medium uppercase tracking-wider text-muted-foreground">
            {label}
          </span>
        ) : (
          <span />
        )}
        <Button
          size="sm"
          variant="ghost"
          className="h-7 gap-1.5 px-2 text-xs"
          aria-label={t("common.copy")}
          onClick={copy}
        >
          {copied ? <Check className="size-3.5" /> : <Copy className="size-3.5" />}
          {copied ? t("common.copied") : t("common.copy")}
        </Button>
      </div>
      <pre className="overflow-x-auto whitespace-pre-wrap break-all rounded border border-border bg-background px-2 py-1.5 font-mono text-xs">
        {value}
      </pre>
      {error && (
        <p className="text-xs text-destructive" role="alert">
          {error}
        </p>
      )}
    </div>
  );
}
