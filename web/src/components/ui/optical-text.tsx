import { Children, cloneElement, isValidElement, type ReactNode } from "react";

/**
 * Wraps bare text in control primitives with the `optical-center` utility
 * (globals.css). text-box-trim does not reach the anonymous flex item a bare
 * string becomes inside an `inline-flex` button, so the text needs its own box.
 */

type TextNode = string | number;

function isTextNode(node: ReactNode): node is TextNode {
  return typeof node === "string" || typeof node === "number";
}

/**
 * Groups each run of adjacent strings/numbers into one `<span class="optical-center">`
 * so `Delete {n}` stays a single flex item (no extra `gap`). Whitespace-only runs
 * stay bare because flex layout drops them; wrapping them would add an empty item.
 * Returns `children` unchanged (same reference) when there is no text to wrap.
 */
export function withOpticalText(children: ReactNode): ReactNode {
  if (isTextNode(children)) {
    return String(children).trim() === "" ? children : <span className="optical-center">{children}</span>;
  }
  if (!Array.isArray(children)) return children;

  const nodes = Children.toArray(children);
  if (!nodes.some(isTextNode)) return children;

  const out: ReactNode[] = [];
  let run: TextNode[] = [];
  const flush = () => {
    if (run.length === 0) return;
    const text = run.join("");
    out.push(
      text.trim() === "" ? (
        text
      ) : (
        <span key={`optical-text-${out.length}`} className="optical-center">
          {text}
        </span>
      ),
    );
    run = [];
  };
  for (const node of nodes) {
    if (isTextNode(node)) {
      run.push(node);
    } else {
      flush();
      out.push(node);
    }
  }
  flush();
  return out;
}

/**
 * `asChild` variant: Radix Slot needs exactly one element child, so the text is
 * wrapped one level down, inside that child (e.g. `<Button asChild><Link>…</Link></Button>`).
 */
export function withOpticalSlotText(children: ReactNode): ReactNode {
  if (!isValidElement<{ children?: ReactNode }>(children)) return children;
  const inner = children.props.children;
  const wrapped = withOpticalText(inner);
  return wrapped === inner ? children : cloneElement(children, undefined, wrapped);
}
