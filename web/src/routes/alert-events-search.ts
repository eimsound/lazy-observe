import type { AlertEvent, AlertStatus } from "@/lib/api/alerts-contract";

/**
 * `/alerts` search params. Alert notifications link here with
 * `?fingerprint=<fp>` (one alert instance) or `?rule=<name>` (a grouped
 * notification), and storm summaries with `?status=firing`.
 */
export interface AlertEventsSearch {
  status?: AlertStatus;
  fingerprint?: string;
  rule?: string;
}

const MAX_FINGERPRINT_LENGTH = 128;
const MAX_RULE_LENGTH = 256;

function boundedString(value: unknown, max: number): string | undefined {
  if (typeof value !== "string") return undefined;
  const trimmed = value.trim();
  return trimmed && trimmed.length <= max ? trimmed : undefined;
}

export function parseAlertEventsSearch(search: Record<string, unknown>): AlertEventsSearch {
  const status = search.status === "firing" || search.status === "resolved" ? search.status : undefined;
  return {
    status,
    fingerprint: boundedString(search.fingerprint, MAX_FINGERPRINT_LENGTH),
    rule: boundedString(search.rule, MAX_RULE_LENGTH),
  };
}

/** Events matching the deep-link filter (fingerprint AND rule), before the status filter. */
export function filterLinkedEvents(events: readonly AlertEvent[], search: AlertEventsSearch): AlertEvent[] {
  return events.filter((e) => (
    (!search.fingerprint || e.fingerprint === search.fingerprint) &&
    (!search.rule || e.rule === search.rule)
  ));
}

export type AlertEventsChip =
  | { kind: "event"; rule: string }
  | { kind: "fingerprint"; fingerprint: string }
  | { kind: "rule"; rule: string };

/**
 * Label for the removable deep-link chip. The rule name is looked up in the
 * unfiltered list so the chip stays readable when the status filter empties
 * the table; an unmatched fingerprint shows the hash itself.
 */
export function alertEventsChip(
  events: readonly AlertEvent[],
  search: AlertEventsSearch,
): AlertEventsChip | null {
  if (search.fingerprint) {
    const match = events.find((e) => (
      e.fingerprint === search.fingerprint && (!search.rule || e.rule === search.rule)
    ));
    return match ? { kind: "event", rule: match.rule } : { kind: "fingerprint", fingerprint: search.fingerprint };
  }
  if (search.rule) return { kind: "rule", rule: search.rule };
  return null;
}
