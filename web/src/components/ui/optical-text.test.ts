import assert from "node:assert/strict";
import { createElement, type ReactElement, type ReactNode } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import { test } from "vitest";

import { withOpticalSlotText, withOpticalText } from "./optical-text.tsx";

const render = (node: ReactNode) => renderToStaticMarkup(createElement("button", null, node));
const icon = createElement("svg", { "data-icon": "x" });

test("wraps a bare string or number in one optical-center span", () => {
  assert.equal(render(withOpticalText("frontend")), '<button><span class="optical-center">frontend</span></button>');
  assert.equal(render(withOpticalText(20)), '<button><span class="optical-center">20</span></button>');
});

test("merges adjacent text into one span so flex gap does not split it", () => {
  assert.equal(
    render(withOpticalText([icon, "删除 ", 3, " 项"])),
    '<button><svg data-icon="x"></svg><span class="optical-center">删除 3 项</span></button>',
  );
});

test("leaves element-only and whitespace-only children untouched", () => {
  const span = createElement("span", { className: "truncate" }, "x");
  assert.equal(withOpticalText(span), span);
  const elementsOnly = [icon, span];
  assert.equal(withOpticalText(elementsOnly), elementsOnly);
  assert.equal(render(withOpticalText([icon, " ", span])), '<button><svg data-icon="x"></svg> <span class="truncate">x</span></button>');
  assert.equal(withOpticalText(null), null);
});

test("asChild wraps text inside the single slotted child", () => {
  const link = createElement("a", { href: "/logs" }, icon, "日志");
  assert.equal(
    renderToStaticMarkup(withOpticalSlotText(link) as ReactElement),
    '<a href="/logs"><svg data-icon="x"></svg><span class="optical-center">日志</span></a>',
  );
  const bare = createElement("a", { href: "/" }, icon);
  assert.equal(withOpticalSlotText(bare), bare);
});
