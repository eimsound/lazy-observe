import { DEFAULT_REPEAT_INTERVAL, type AlertRule } from "@/lib/api/alerts-contract";
import { pairsToRecord, recordToPairs, type Pair } from "@/components/common/kv-pairs";

/**
 * Annotation key rendered as the alert title (`annotations.summary`, a Go
 * `text/template`). The rule form edits it in its own field, so the generic
 * annotations editor never shows it.
 */
export const TITLE_ANNOTATION = "summary";

/** Splits stored annotations into the title template and the remaining editor rows. */
export function splitRuleAnnotations(annotations: Record<string, string>): {
  title: string;
  pairs: Pair[];
} {
  const { [TITLE_ANNOTATION]: title = "", ...rest } = annotations;
  return { title, pairs: recordToPairs(rest) };
}

/**
 * Builds the annotations payload. A non-blank title is sent verbatim as
 * `summary` and wins over a `summary` row typed into the editor; a blank title
 * keeps such a row instead of dropping it, and otherwise omits `summary` so the
 * notification falls back to the rule name.
 */
export function joinRuleAnnotations(title: string, pairs: Pair[]): Record<string, string> {
  const annotations = pairsToRecord(pairs);
  if (title.trim() !== "") annotations[TITLE_ANNOTATION] = title;
  return annotations;
}

/**
 * Initial repeat interval: new rules start at the documented default; existing
 * rules show their stored override, where "" means they inherit the server's
 * global `alert.repeat_interval`.
 */
export function initialRepeatInterval(editing: AlertRule | null): string {
  return editing ? editing.repeat_interval : DEFAULT_REPEAT_INTERVAL;
}

/**
 * Inherited value to show as the empty-field placeholder. Only an existing rule
 * without an override tells us the server's global default; otherwise null.
 */
export function inheritedRepeatInterval(editing: AlertRule | null): string | null {
  return editing && editing.repeat_interval === "" ? editing.effective_repeat_interval : null;
}

/**
 * Template snippets shown in the form's help tips and title placeholder. They
 * are passed to i18n as interpolation values (never as dictionary text) so the
 * Go template braces are not parsed as i18next placeholders.
 */
export const TEMPLATE_EXAMPLES = {
  instance: "{{$labels.instance}}",
  value: '{{printf "%.0f" $value}}',
} as const;

export const ANNOTATION_EXAMPLES = [
  "description = {{$labels.instance}}: {{$value}}",
  "runbook = https://wiki.example.com/disk-full",
] as const;

export const LABEL_EXAMPLES = ["team = infra", "env = prod"] as const;
