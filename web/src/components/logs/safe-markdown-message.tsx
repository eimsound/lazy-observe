import { renderInlineMarkdown, renderMarkdownBlocks } from "./log-message-renderers";

/**
 * A deliberately small Markdown renderer. It never interprets raw HTML and
 * only emits React text/elements, so log content cannot inject DOM.
 */
export function SafeMarkdownMessage({
  source,
  singleLine = false,
}: {
  source: string;
  singleLine?: boolean;
}) {
  if (singleLine) {
    return (
      <span>
        {renderInlineMarkdown(source.replace(/\s+/g, " "), "single-line")}
      </span>
    );
  }
  return <div className="space-y-1">{renderMarkdownBlocks(source)}</div>;
}
