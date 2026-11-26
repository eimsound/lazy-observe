import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { createElement } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import i18next from "i18next";
import { test } from "vitest";

import { HelpTip } from "@/components/common/help-tip";
import { TooltipProvider } from "@/components/ui/tooltip";
import { DEFAULT_REPEAT_INTERVAL, type AlertRule } from "@/lib/api/alerts-contract";
import { en } from "@/lib/i18n/locales/en";
import { zh } from "@/lib/i18n/locales/zh";
import {
  ANNOTATION_EXAMPLES,
  TEMPLATE_EXAMPLES,
  TITLE_ANNOTATION,
  inheritedRepeatInterval,
  initialRepeatInterval,
  joinRuleAnnotations,
  splitRuleAnnotations,
} from "./alert-rule-form";

const rule = (overrides: Partial<AlertRule> = {}): AlertRule => ({
  id: "7",
  name: "DiskFull",
  signal: "metrics",
  evaluation_engine: "native",
  expr: "disk_used_percent > 90",
  for: "5m",
  evaluation_interval: "",
  effective_evaluation_interval: "5s",
  repeat_interval: "",
  effective_repeat_interval: "1h",
  severity: "warning",
  labels: {},
  annotations: {},
  enabled: true,
  channel_ids: [],
  created_at: "",
  updated_at: "",
  ...overrides,
});

test("summary is extracted into the title template and kept out of the annotations editor", () => {
  const { title, pairs } = splitRuleAnnotations({
    summary: '{{$labels.instance}} 磁盘 {{printf "%.0f" $value}}%',
    description: "{{$labels.instance}} is full",
    runbook: "https://wiki.example.com/disk",
  });
  assert.equal(title, '{{$labels.instance}} 磁盘 {{printf "%.0f" $value}}%');
  assert.deepEqual(pairs, [
    { key: "description", value: "{{$labels.instance}} is full" },
    { key: "runbook", value: "https://wiki.example.com/disk" },
  ]);
  assert.ok(!pairs.some((pair) => pair.key === TITLE_ANNOTATION));

  assert.deepEqual(splitRuleAnnotations({}), { title: "", pairs: [] });
  assert.deepEqual(splitRuleAnnotations({ description: "x" }), {
    title: "",
    pairs: [{ key: "description", value: "x" }],
  });
});

test("load → edit → save round-trips annotations and keeps the custom title verbatim", () => {
  const stored = {
    summary: "  [prod] {{$labels.instance}}  ",
    description: "d",
  };
  const { title, pairs } = splitRuleAnnotations(stored);
  // Unchanged round trip reproduces the stored record exactly.
  assert.deepEqual(joinRuleAnnotations(title, pairs), stored);

  // Editing the title only changes summary.
  assert.deepEqual(joinRuleAnnotations("{{$labels.job}} down", pairs), {
    summary: "{{$labels.job}} down",
    description: "d",
  });

  // Clearing the title drops summary so the notification falls back to the rule name.
  assert.deepEqual(joinRuleAnnotations("", pairs), { description: "d" });
  assert.deepEqual(joinRuleAnnotations("   ", pairs), { description: "d" });
});

test("title field wins over a summary row typed into the annotations editor; a blank title keeps it", () => {
  const typed = [
    { key: "summary", value: "from editor" },
    { key: "description", value: "d" },
  ];
  assert.deepEqual(joinRuleAnnotations("from title", typed), { summary: "from title", description: "d" });
  assert.deepEqual(joinRuleAnnotations("", typed), { summary: "from editor", description: "d" });
});

test("repeat_interval prefills 1h for new rules and shows the stored override when editing", () => {
  assert.equal(DEFAULT_REPEAT_INTERVAL, "1h");
  assert.equal(initialRepeatInterval(null), "1h");
  assert.equal(inheritedRepeatInterval(null), null);

  // Existing rule inheriting the global default: empty field, placeholder shows the effective value.
  const inherited = rule({ repeat_interval: "", effective_repeat_interval: "4h" });
  assert.equal(initialRepeatInterval(inherited), "");
  assert.equal(inheritedRepeatInterval(inherited), "4h");

  // Existing rule with an override: field shows it, no inherited placeholder.
  const custom = rule({ repeat_interval: "15m", effective_repeat_interval: "15m" });
  assert.equal(initialRepeatInterval(custom), "15m");
  assert.equal(inheritedRepeatInterval(custom), null);
});

test("rule form sends repeat_interval and the joined annotations", () => {
  const source = readFileSync(new URL("./alert-rules-page.tsx", import.meta.url), "utf8");
  // Clearing the field sends "" so the server resets the rule to the global default.
  assert.match(source, /repeat_interval: repeatInterval\.trim\(\)/);
  assert.match(source, /annotations: joinRuleAnnotations\(titleTemplate, annotations\)/);
  // The enable toggle re-sends the stored override instead of dropping it.
  assert.match(source, /repeat_interval: rule\.repeat_interval/);
});

test("HelpTip exposes its explanation and literal template examples", () => {
  const html = renderToStaticMarkup(
    createElement(
      TooltipProvider,
      null,
      createElement(HelpTip, {
        scopeTitle: zh.alerts.titleTemplateHelpTitle,
        scope: [{ text: zh.alerts.titleTemplateHelpLabels }, { text: zh.alerts.titleTemplateHelpValue }],
        examples: [TEMPLATE_EXAMPLES.instance, TEMPLATE_EXAMPLES.value],
      }),
    ),
  );
  assert.match(html, /<button type="button" tabindex="-1"/);
  const label = /aria-label="([^"]*)"/.exec(html)?.[1] ?? "";
  const decoded = label.replaceAll("&quot;", '"');
  assert.ok(decoded.includes(zh.alerts.titleTemplateHelpTitle), decoded);
  assert.ok(decoded.includes("{{$labels.instance}}"), decoded);
  assert.ok(decoded.includes('{{printf "%.0f" $value}}'), decoded);

  const plain = renderToStaticMarkup(
    createElement(TooltipProvider, null, createElement(HelpTip, { content: zh.alerts.repeatIntervalHelp })),
  );
  assert.ok(plain.includes(`aria-label="${zh.alerts.repeatIntervalHelp}"`), plain);
});

test("title placeholder keeps Go template braces through i18next interpolation", async () => {
  const i18n = i18next.createInstance();
  await i18n.init({
    resources: { zh: { translation: zh }, en: { translation: en } },
    lng: "zh",
    interpolation: { escapeValue: false },
  });
  assert.equal(
    i18n.t("alerts.titleTemplatePlaceholder", TEMPLATE_EXAMPLES),
    '例如：{{$labels.instance}} 磁盘 {{printf "%.0f" $value}}%',
  );
  assert.equal(
    i18n.t("alerts.titleTemplatePlaceholder", { ...TEMPLATE_EXAMPLES, lng: "en" }),
    'e.g. {{$labels.instance}} disk {{printf "%.0f" $value}}%',
  );
  assert.equal(i18n.t("alerts.repeatIntervalInherited", { value: "4h" }), "全局默认 4h");
  assert.equal(i18n.t("alerts.repeatIntervalInvalid", { min: "1m", max: "7d", lng: "en" }),
    "Repeat interval must be between 1m and 7d in whole seconds");
  // Dictionary text never embeds raw Go template braces (they would be read as i18next placeholders);
  // examples live in code and are rendered verbatim.
  assert.ok(ANNOTATION_EXAMPLES.some((value) => value.includes("{{$labels.instance}}")));
  for (const dict of [zh, en]) {
    for (const key of [
      "titleTemplateHelpTitle",
      "titleTemplateHelpLabels",
      "titleTemplateHelpValue",
      "labelsHelpTitle",
      "annotationsHelpTitle",
      "annotationsHelpTemplate",
      "repeatIntervalHelp",
    ] as const) {
      assert.doesNotMatch(dict.alerts[key], /\{\{/);
    }
  }
});

test("alerts locale keys match between zh and en", () => {
  assert.deepEqual(Object.keys(en.alerts).sort(), Object.keys(zh.alerts).sort());
  for (const [key, value] of Object.entries(zh.alerts)) {
    const placeholders = (text: string) => [...text.matchAll(/\{\{(\w+)\}\}/g)].map((m) => m[1]).sort();
    assert.deepEqual(
      placeholders(en.alerts[key as keyof typeof en.alerts]),
      placeholders(value),
      `alerts.${key} interpolation placeholders differ`,
    );
  }
});
