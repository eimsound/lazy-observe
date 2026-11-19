import type { CSSProperties, ReactNode } from "react";

interface AnsiStyleState {
  foreground?: string;
  background?: string;
  bold: boolean;
  dim: boolean;
  italic: boolean;
  underline: boolean;
  strike: boolean;
  inverse: boolean;
}

const ANSI_BASE_COLORS = [
  "#000000",
  "#aa0000",
  "#00aa00",
  "#aa5500",
  "#0000aa",
  "#aa00aa",
  "#00aaaa",
  "#aaaaaa",
  "#555555",
  "#ff5555",
  "#55ff55",
  "#ffff55",
  "#5555ff",
  "#ff55ff",
  "#55ffff",
  "#ffffff",
];

// ANSI escape sequences are control characters by definition.
// oxlint-disable-next-line no-control-regex
const ANSI_OSC_PATTERN = /\u001B\][\s\S]*?(?:\u0007|\u001B\\)/g;
// oxlint-disable-next-line no-control-regex
const ANSI_CSI_PATTERN = /(?:\u001B\[|\u009B)([0-?]*)([ -/]*)([@-~])/g;

function defaultAnsiStyle(): AnsiStyleState {
  return {
    bold: false,
    dim: false,
    italic: false,
    underline: false,
    strike: false,
    inverse: false,
  };
}

function ansi256Color(index: number): string | undefined {
  if (!Number.isInteger(index) || index < 0 || index > 255) return undefined;
  if (index < ANSI_BASE_COLORS.length) return ANSI_BASE_COLORS[index];

  if (index < 232) {
    const offset = index - 16;
    const red = Math.floor(offset / 36);
    const green = Math.floor((offset % 36) / 6);
    const blue = offset % 6;
    const channel = (value: number) => (value === 0 ? 0 : value * 40 + 55);
    return `rgb(${channel(red)} ${channel(green)} ${channel(blue)})`;
  }

  const gray = 8 + (index - 232) * 10;
  return `rgb(${gray} ${gray} ${gray})`;
}

function ansiRgb(red: number, green: number, blue: number): string | undefined {
  if (![red, green, blue].every(Number.isInteger)) return undefined;
  if ([red, green, blue].some((channel) => channel < 0 || channel > 255)) {
    return undefined;
  }
  return `rgb(${red} ${green} ${blue})`;
}

function applyAnsiCodes(
  current: AnsiStyleState,
  codes: number[],
): AnsiStyleState {
  let next = { ...current };

  for (let index = 0; index < codes.length; index++) {
    const code = codes[index] ?? 0;
    if (code === 0) {
      next = defaultAnsiStyle();
    } else if (code === 1) {
      next.bold = true;
    } else if (code === 2) {
      next.dim = true;
    } else if (code === 3) {
      next.italic = true;
    } else if (code === 4) {
      next.underline = true;
    } else if (code === 7) {
      next.inverse = true;
    } else if (code === 9) {
      next.strike = true;
    } else if (code === 22) {
      next.bold = false;
      next.dim = false;
    } else if (code === 23) {
      next.italic = false;
    } else if (code === 24) {
      next.underline = false;
    } else if (code === 27) {
      next.inverse = false;
    } else if (code === 29) {
      next.strike = false;
    } else if (code >= 30 && code <= 37) {
      next.foreground = ANSI_BASE_COLORS[code - 30];
    } else if (code === 39) {
      next.foreground = undefined;
    } else if (code >= 40 && code <= 47) {
      next.background = ANSI_BASE_COLORS[code - 40];
    } else if (code === 49) {
      next.background = undefined;
    } else if (code >= 90 && code <= 97) {
      next.foreground = ANSI_BASE_COLORS[code - 90 + 8];
    } else if (code >= 100 && code <= 107) {
      next.background = ANSI_BASE_COLORS[code - 100 + 8];
    } else if (code === 38 || code === 48) {
      const target = code === 38 ? "foreground" : "background";
      const colorMode = codes[index + 1];
      if (colorMode === 5) {
        const color = ansi256Color(codes[index + 2] ?? -1);
        if (color) next[target] = color;
        index += 2;
      } else if (colorMode === 2) {
        const color = ansiRgb(
          codes[index + 2] ?? -1,
          codes[index + 3] ?? -1,
          codes[index + 4] ?? -1,
        );
        if (color) next[target] = color;
        index += 4;
      }
    }
  }

  return next;
}

function ansiCss(style: AnsiStyleState): CSSProperties {
  const foreground = style.inverse ? style.background : style.foreground;
  const background = style.inverse ? style.foreground : style.background;
  const decorations = [
    style.underline ? "underline" : "",
    style.strike ? "line-through" : "",
  ].filter(Boolean);

  return {
    color: foreground,
    backgroundColor: background,
    fontWeight: style.bold ? 700 : undefined,
    fontStyle: style.italic ? "italic" : undefined,
    opacity: style.dim ? 0.65 : undefined,
    textDecoration: decorations.length ? decorations.join(" ") : undefined,
  };
}

/**
 * Converts terminal SGR sequences into escaped React text nodes. Unsupported
 * cursor/control CSI sequences and OSC commands are removed rather than run.
 */
export function renderAnsiMessage(input: string): ReactNode[] {
  const source = input.replace(ANSI_OSC_PATTERN, "");
  const nodes: ReactNode[] = [];
  let style = defaultAnsiStyle();
  let cursor = 0;
  let segment = 0;

  ANSI_CSI_PATTERN.lastIndex = 0;
  for (let match = ANSI_CSI_PATTERN.exec(source); match; match = ANSI_CSI_PATTERN.exec(source)) {
    if (match.index > cursor) {
      nodes.push(
        <span key={`ansi-${segment++}`} style={ansiCss(style)}>
          {source.slice(cursor, match.index)}
        </span>,
      );
    }

    if (match[3] === "m") {
      const codes = match[1]
        ? match[1]
          .replaceAll(":", ";")
          .split(";")
          .map((value) => Number(value || 0))
          .filter(Number.isFinite)
        : [0];
      style = applyAnsiCodes(style, codes);
    }
    cursor = ANSI_CSI_PATTERN.lastIndex;
  }

  if (cursor < source.length) {
    nodes.push(
      <span key={`ansi-${segment}`} style={ansiCss(style)}>
        {source.slice(cursor).replaceAll("\u001B", "")}
      </span>,
    );
  }

  return nodes;
}

const INLINE_MARKDOWN_PATTERN =
  /(`[^`\n]*`|\*\*[^*\n]+\*\*|__[^_\n]+__|~~[^~\n]+~~|\[[^\]\n]+\]\([^\s)\n]+(?:\s+"[^"\n]*")?\)|\*[^*\n]+\*|_[^_\n]+_)/g;

function safeMarkdownHref(value: string): string | null {
  const href = value.trim();
  if (
    /^https?:\/\//i.test(href) ||
    /^mailto:/i.test(href) ||
    (href.startsWith("/") && !href.startsWith("//")) ||
    href.startsWith("#")
  ) {
    return href;
  }
  return null;
}

export function renderInlineMarkdown(
  text: string,
  keyPrefix: string,
  depth = 0,
): ReactNode[] {
  if (depth > 3) return [text];

  const nodes: ReactNode[] = [];
  let cursor = 0;
  let tokenIndex = 0;
  const pattern = new RegExp(INLINE_MARKDOWN_PATTERN.source, "g");

  for (
    let match = pattern.exec(text);
    match;
    match = pattern.exec(text)
  ) {
    if (match.index > cursor) nodes.push(text.slice(cursor, match.index));
    const token = match[0];
    const key = `${keyPrefix}-${tokenIndex++}`;

    if (token.startsWith("`")) {
      nodes.push(
        <code key={key} className="rounded bg-muted px-1 py-0.5 font-mono text-[0.95em]">
          {token.slice(1, -1)}
        </code>,
      );
    } else if (
      (token.startsWith("**") && token.endsWith("**")) ||
      (token.startsWith("__") && token.endsWith("__"))
    ) {
      nodes.push(
        <strong key={key} className="font-semibold">
          {renderInlineMarkdown(token.slice(2, -2), key, depth + 1)}
        </strong>,
      );
    } else if (token.startsWith("~~") && token.endsWith("~~")) {
      nodes.push(
        <del key={key}>
          {renderInlineMarkdown(token.slice(2, -2), key, depth + 1)}
        </del>,
      );
    } else if (token.startsWith("[")) {
      const link = token.match(/^\[([^\]]+)\]\(([^\s)]+)(?:\s+"([^"]*)")?\)$/);
      const href = link ? safeMarkdownHref(link[2]) : null;
      if (link && href) {
        nodes.push(
          <a
            key={key}
            href={href}
            title={link[3]}
            className="text-primary underline underline-offset-2"
            target={href.startsWith("#") || href.startsWith("/") ? undefined : "_blank"}
            rel={href.startsWith("#") || href.startsWith("/") ? undefined : "noreferrer"}
            onClick={(event) => event.stopPropagation()}
          >
            {renderInlineMarkdown(link[1], key, depth + 1)}
          </a>,
        );
      } else {
        nodes.push(token);
      }
    } else {
      nodes.push(
        <em key={key}>
          {renderInlineMarkdown(token.slice(1, -1), key, depth + 1)}
        </em>,
      );
    }
    cursor = match.index + token.length;
  }

  if (cursor < text.length) nodes.push(text.slice(cursor));
  return nodes;
}

function lineStartsMarkdownBlock(line: string): boolean {
  return (
    /^ {0,3}```/.test(line) ||
    /^ {0,3}#{1,6}\s+/.test(line) ||
    /^ {0,3}>\s?/.test(line) ||
    /^ {0,3}[-+*]\s+/.test(line) ||
    /^ {0,3}\d+[.)]\s+/.test(line) ||
    /^ {0,3}([-*_])(?:\s*\1){2,}\s*$/.test(line)
  );
}

function paragraphContent(lines: string[], keyPrefix: string): ReactNode[] {
  const nodes: ReactNode[] = [];
  lines.forEach((line, index) => {
    if (index > 0) nodes.push(<br key={`${keyPrefix}-br-${index}`} />);
    nodes.push(...renderInlineMarkdown(line, `${keyPrefix}-line-${index}`));
  });
  return nodes;
}

export function renderMarkdownBlocks(source: string): ReactNode[] {
  const lines = source.replaceAll("\r\n", "\n").split("\n");
  const blocks: ReactNode[] = [];
  let index = 0;
  let blockIndex = 0;

  while (index < lines.length) {
    const line = lines[index];
    if (!line.trim()) {
      index++;
      continue;
    }

    const fence = line.match(/^ {0,3}```\s*([^\s`]*)?.*$/);
    if (fence) {
      const code: string[] = [];
      index++;
      while (index < lines.length && !/^ {0,3}```\s*$/.test(lines[index])) {
        code.push(lines[index]);
        index++;
      }
      if (index < lines.length) index++;
      blocks.push(
        <pre
          key={`block-${blockIndex++}`}
          className="my-1 max-w-full overflow-x-auto rounded border border-border/70 bg-muted/50 p-2"
          data-language={fence[1] || undefined}
        >
          <code className="font-mono">{code.join("\n")}</code>
        </pre>,
      );
      continue;
    }

    const heading = line.match(/^ {0,3}(#{1,6})\s+(.+)$/);
    if (heading) {
      const level = heading[1].length;
      blocks.push(
        <div
          key={`block-${blockIndex++}`}
          role="heading"
          aria-level={level}
          className={level <= 2 ? "my-1 font-bold" : "my-1 font-semibold"}
        >
          {renderInlineMarkdown(heading[2], `heading-${index}`)}
        </div>,
      );
      index++;
      continue;
    }

    if (/^ {0,3}>\s?/.test(line)) {
      const quote: string[] = [];
      while (index < lines.length && /^ {0,3}>\s?/.test(lines[index])) {
        quote.push(lines[index].replace(/^ {0,3}>\s?/, ""));
        index++;
      }
      blocks.push(
        <blockquote
          key={`block-${blockIndex++}`}
          className="my-1 border-l-2 border-border pl-2 text-muted-foreground"
        >
          {paragraphContent(quote, `quote-${index}`)}
        </blockquote>,
      );
      continue;
    }

    const unordered = line.match(/^ {0,3}[-+*]\s+(.+)$/);
    if (unordered) {
      const items: string[] = [];
      while (index < lines.length) {
        const item = lines[index].match(/^ {0,3}[-+*]\s+(.+)$/);
        if (!item) break;
        items.push(item[1]);
        index++;
      }
      blocks.push(
        <ul key={`block-${blockIndex++}`} className="my-1 list-disc space-y-0.5 pl-5">
          {items.map((item, itemIndex) => (
            <li key={itemIndex}>
              {renderInlineMarkdown(item, `ul-${index}-${itemIndex}`)}
            </li>
          ))}
        </ul>,
      );
      continue;
    }

    const ordered = line.match(/^ {0,3}\d+[.)]\s+(.+)$/);
    if (ordered) {
      const items: string[] = [];
      while (index < lines.length) {
        const item = lines[index].match(/^ {0,3}\d+[.)]\s+(.+)$/);
        if (!item) break;
        items.push(item[1]);
        index++;
      }
      blocks.push(
        <ol key={`block-${blockIndex++}`} className="my-1 list-decimal space-y-0.5 pl-5">
          {items.map((item, itemIndex) => (
            <li key={itemIndex}>
              {renderInlineMarkdown(item, `ol-${index}-${itemIndex}`)}
            </li>
          ))}
        </ol>,
      );
      continue;
    }

    if (/^ {0,3}([-*_])(?:\s*\1){2,}\s*$/.test(line)) {
      blocks.push(<hr key={`block-${blockIndex++}`} className="my-2 border-border" />);
      index++;
      continue;
    }

    const paragraph: string[] = [];
    while (
      index < lines.length &&
      lines[index].trim() &&
      (paragraph.length === 0 || !lineStartsMarkdownBlock(lines[index]))
    ) {
      paragraph.push(lines[index]);
      index++;
    }
    blocks.push(
      <p key={`block-${blockIndex++}`} className="my-0.5">
        {paragraphContent(paragraph, `paragraph-${index}`)}
      </p>,
    );
  }

  return blocks;
}
