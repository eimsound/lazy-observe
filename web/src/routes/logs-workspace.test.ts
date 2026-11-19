import assert from "node:assert/strict";
import { test } from "vitest";

import type { LogRow } from "../lib/api/logs.ts";
import {
  createLogsExplorationWorkspace,
  type LogsFiniteQueryPlan,
  type LogsHistoryAdapter,
  type LogsLiveObserver,
  type LogsLiveRequest,
  type LogsWorkspaceClock,
} from "./logs-workspace.ts";

class FakeClock implements LogsWorkspaceClock {
  nowMs = 1_800_000_000_000;
  randomValue = 0.5;
  nextTimer = 1;
  timers = new Map<number, {
    callback: () => void;
    repeat: boolean;
    delay: number;
    dueAt: number;
  }>();

  now = () => this.nowMs;
  random = () => this.randomValue;

  setTimeout = (callback: () => void, delay: number) => {
    const id = this.nextTimer++;
    this.timers.set(id, { callback, repeat: false, delay, dueAt: this.nowMs + delay });
    return id;
  };

  clearTimeout = (id: number) => {
    this.timers.delete(id);
  };

  setInterval = (callback: () => void, delay: number) => {
    const id = this.nextTimer++;
    this.timers.set(id, { callback, repeat: true, delay, dueAt: this.nowMs + delay });
    return id;
  };

  clearInterval = (id: number) => {
    this.timers.delete(id);
  };

  advance(delay: number) {
    this.nowMs += delay;
    const due = [...this.timers.entries()].filter(([, timer]) => timer.dueAt <= this.nowMs);
    for (const [id, timer] of due) {
      if (timer.repeat) timer.dueAt += timer.delay;
      else this.timers.delete(id);
      timer.callback();
    }
  }
}

function createHistoryAdapter(): LogsHistoryAdapter {
  const byProject = new Map<string, Array<{
    query: string;
    lastRunAt: number;
    favorite: boolean;
  }>>();
  const load = (project: string) => byProject.get(project) ?? [];
  return {
    load,
    push(project, query, now) {
      const previous = load(project).find((entry) => entry.query === query);
      const next = [
        { query, lastRunAt: now, favorite: previous?.favorite ?? false },
        ...load(project).filter((entry) => entry.query !== query),
      ];
      byProject.set(project, next);
      return next;
    },
    toggleFavorite(project, query) {
      const next = load(project).map((entry) => (
        entry.query === query ? { ...entry, favorite: !entry.favorite } : entry
      ));
      byProject.set(project, next);
      return next;
    },
    remove(project, query) {
      const next = load(project).filter((entry) => entry.query !== query);
      byProject.set(project, next);
      return next;
    },
    clear(project) {
      const next = load(project).filter((entry) => entry.favorite);
      byProject.set(project, next);
      return next;
    },
  };
}

function row(id: string): LogRow {
  return {
    id,
    time: 1_800_000_000_000,
    timeRaw: "2027-01-15T08:00:00Z",
    message: id,
    stream: "api",
    level: "info",
    raw: { _time: "2027-01-15T08:00:00Z", _msg: id, _stream_id: "api" },
  };
}

function createHarness(location = "/logs?panel=detail") {
  const clock = new FakeClock();
  const plans: LogsFiniteQueryPlan[] = [];
  const replacements: string[] = [];
  const cancelledExecutions: number[] = [];
  const liveConnections: Array<{
    request: LogsLiveRequest;
    observer: LogsLiveObserver;
    closed: boolean;
  }> = [];
  const workspace = createLogsExplorationWorkspace({
    project: "alpha",
    location,
    clock,
    history: createHistoryAdapter(),
    onLocationReplace: (next) => replacements.push(next),
    onFiniteQueryPlan: (plan) => plans.push(plan),
    onFiniteQueryCancel: (executionId) => cancelledExecutions.push(executionId),
    openLiveStream: (request, observer) => {
      const connection = { request, observer, closed: false };
      liveConnections.push(connection);
      return { close: () => { connection.closed = true; } };
    },
  });
  return {
    workspace,
    clock,
    plans,
    replacements,
    cancelledExecutions,
    liveConnections,
  };
}

test("workspace normalizes behavior URL fields while preserving presentation fields", () => {
  const { workspace, replacements } = createHarness(
    "/logs?q=%20error%20&run=wat&range=nope&limit=99999&refresh=-4"
      + "&filters=bad&view=wat&panel=detail",
  );

  assert.deepEqual(workspace.getSnapshot().intent, {
    project: "alpha",
    draft: "error",
    range: { mode: "relative", presetId: "1h", seconds: 3_600 },
    filters: [],
    limit: 10_000,
    refreshSeconds: 0,
    mode: "historical",
    historicalView: "group",
    liveOffset: "5s",
    liveLimit: 100,
  });
  assert.equal(workspace.getSnapshot().executed, null);
  assert.equal(replacements.length, 1);
  const normalized = new URL(replacements[0], "https://lazy-observe.test");
  assert.equal(normalized.searchParams.get("q"), "error");
  assert.equal(normalized.searchParams.has("run"), false);
  assert.equal(normalized.searchParams.has("range"), false);
  assert.equal(normalized.searchParams.get("limit"), "10000");
  assert.equal(normalized.searchParams.get("panel"), "detail");
  assert.equal(normalized.searchParams.has("filters"), false);
  assert.equal(normalized.searchParams.has("view"), false);

  workspace.dispose();
});

test("editing a draft updates the shareable URL without clearing accepted results", () => {
  const { workspace, plans, replacements } = createHarness("/logs?q=old&run=1&range=1h");
  const initialPlan = plans.at(-1);
  assert.equal(initialPlan?.slot, "rows");
  workspace.send({
    type: "finiteSucceeded",
    identity: initialPlan!.identity,
    data: [row("old-result")],
  });

  workspace.send({ type: "draftChanged", draft: "new draft" });

  assert.equal(workspace.getSnapshot().intent.draft, "new draft");
  assert.equal(workspace.getSnapshot().executed?.query, "old");
  assert.deepEqual(workspace.getSnapshot().finite.rows.data, [row("old-result")]);
  assert.equal(plans.length, 1);
  const nextURL = new URL(replacements.at(-1)!, "https://lazy-observe.test");
  assert.equal(nextURL.searchParams.get("q"), "new draft");
  assert.equal(nextURL.searchParams.has("run"), false);

  workspace.dispose();
});

test("Historical Run clears old rows and rejects results from stale identities", () => {
  const { workspace, plans, cancelledExecutions } = createHarness();
  workspace.send({ type: "draftChanged", draft: "level:error" });
  workspace.send({ type: "run" });
  const first = plans.at(-1)!;
  assert.equal(first.slot, "rows");
  assert.equal(workspace.getSnapshot().finite.rows.status, "loading");
  assert.equal(workspace.getSnapshot().finite.rows.data, null);

  workspace.send({ type: "draftChanged", draft: "service:api" });
  workspace.send({ type: "run" });
  const second = plans.at(-1)!;
  assert.notEqual(first.identity, second.identity);
  assert.deepEqual(cancelledExecutions, [first.executionId]);

  workspace.send({
    type: "finiteSucceeded",
    identity: first.identity,
    data: [row("stale")],
  });
  assert.equal(workspace.getSnapshot().finite.rows.status, "loading");
  assert.equal(workspace.getSnapshot().finite.rows.data, null);

  workspace.send({
    type: "finiteSucceeded",
    identity: second.identity,
    data: [row("current")],
  });
  assert.equal(workspace.getSnapshot().finite.rows.status, "success");
  assert.deepEqual(workspace.getSnapshot().finite.rows.data, [row("current")]);
  assert.deepEqual(workspace.getSnapshot().history.map((entry) => entry.query), [
    "service:api",
    "level:error",
  ]);

  workspace.dispose();
});

test("Historical refresh failure keeps prior rows and the executed filter until retry succeeds", () => {
  const { workspace, plans } = createHarness();
  workspace.send({ type: "draftChanged", draft: "service:checkout" });
  workspace.send({ type: "run" });
  const initial = plans.at(-1)!;
  workspace.send({
    type: "finiteSucceeded",
    identity: initial.identity,
    data: [row("stable-row")],
  });

  workspace.send({ type: "refresh" });
  const refresh = plans.at(-1)!;
  assert.equal(workspace.getSnapshot().finite.rows.status, "loading");
  assert.deepEqual(workspace.getSnapshot().finite.rows.data, [row("stable-row")]);
  assert.equal(workspace.getSnapshot().executed?.query, "service:checkout");

  workspace.send({
    type: "finiteFailed",
    identity: refresh.identity,
    error: "internal endpoint must not be rendered",
  });
  assert.equal(workspace.getSnapshot().finite.rows.status, "error");
  assert.deepEqual(workspace.getSnapshot().finite.rows.data, [row("stable-row")]);
  assert.equal(workspace.getSnapshot().executed?.query, "service:checkout");

  workspace.send({ type: "retry", slot: "rows" });
  const retry = plans.at(-1)!;
  assert.deepEqual(workspace.getSnapshot().finite.rows.data, [row("stable-row")]);
  workspace.send({ type: "finiteSucceeded", identity: retry.identity, data: [row("fresh-row")] });
  assert.equal(workspace.getSnapshot().finite.rows.status, "success");
  assert.deepEqual(workspace.getSnapshot().finite.rows.data, [row("fresh-row")]);

  workspace.dispose();
});

test("Historical failure, retry, and cancel keep the executed snapshot authoritative", () => {
  const { workspace, plans, cancelledExecutions } = createHarness();
  workspace.send({ type: "draftChanged", draft: "broken" });
  workspace.send({ type: "run" });
  const failed = plans.at(-1)!;
  workspace.send({
    type: "finiteFailed",
    identity: failed.identity,
    error: "invalid query",
  });
  workspace.send({ type: "draftChanged", draft: "not executed" });
  workspace.send({ type: "retry", slot: "rows" });

  const retry = plans.at(-1)!;
  assert.equal(retry.snapshot.query, "broken");
  assert.equal(workspace.getSnapshot().finite.rows.status, "loading");
  workspace.send({ type: "cancelFinite" });
  assert.equal(workspace.getSnapshot().finite.rows.status, "idle");
  assert.equal(workspace.getSnapshot().finite.rows.data, null);
  assert.equal(cancelledExecutions.at(-1), retry.executionId);

  workspace.dispose();
});

test("primary rows remain usable when an auxiliary result fails", () => {
  const { workspace, plans } = createHarness();
  workspace.send({
    type: "configureAuxiliaryQueries",
    config: {
      rowsEnabled: true,
      chartEnabled: true,
      graphMode: "hits",
      groupBy: "service",
      topN: 7,
      step: 15,
    },
  });
  workspace.send({ type: "run" });
  const rowsPlan = plans.find((plan) => plan.slot === "rows")!;
  const hitsPlan = plans.find((plan) => plan.slot === "hits")!;

  workspace.send({
    type: "finiteSucceeded",
    identity: rowsPlan.identity,
    data: [row("usable")],
  });
  workspace.send({
    type: "finiteFailed",
    identity: hitsPlan.identity,
    error: "histogram unavailable",
  });

  assert.equal(workspace.getSnapshot().finite.rows.status, "success");
  assert.deepEqual(workspace.getSnapshot().finite.rows.data, [row("usable")]);
  assert.equal(workspace.getSnapshot().finite.hits.status, "error");
  assert.equal(workspace.getSnapshot().finite.hits.error, "histogram unavailable");

  workspace.send({ type: "retry", slot: "hits" });
  const retry = plans.at(-1)!;
  assert.equal(retry.slot, "hits");
  assert.equal(retry.executionId, hitsPlan.executionId);
  assert.equal(workspace.getSnapshot().finite.rows.status, "success");

  workspace.dispose();
});

test("query time-range resolution gates the other finite plans", () => {
  const { workspace, plans } = createHarness();
  workspace.send({
    type: "configureAuxiliaryQueries",
    config: {
      rowsEnabled: true,
      chartEnabled: true,
      graphMode: "stats",
      groupBy: "",
      topN: 5,
      step: null,
    },
  });
  workspace.send({ type: "draftChanged", draft: "_time:5m | stats count()" });
  workspace.send({ type: "run" });
  assert.deepEqual(plans.map((plan) => plan.slot), ["timeRange"]);

  workspace.send({
    type: "finiteSucceeded",
    identity: plans[0].identity,
    data: {
      hasTimeFilter: true,
      start: "2027-01-15T07:55:00Z",
      end: "2027-01-15T08:00:00Z",
    },
  });

  assert.deepEqual(plans.map((plan) => plan.slot), ["timeRange", "rows", "stats"]);
  assert.deepEqual(workspace.getSnapshot().effectiveRange, {
    start: 1_799_999_700,
    end: 1_800_000_000,
  });

  workspace.dispose();
});

test("Project switching cancels stale work and re-executes intent without sharing history", () => {
  const { workspace, plans, cancelledExecutions } = createHarness();
  workspace.send({ type: "draftChanged", draft: "service:api" });
  workspace.send({ type: "run" });
  const alphaExecution = plans.at(-1)!.executionId;

  workspace.send({ type: "projectChanged", project: "beta" });

  assert.equal(workspace.getSnapshot().intent.project, "beta");
  assert.equal(workspace.getSnapshot().intent.draft, "service:api");
  assert.equal(workspace.getSnapshot().executed?.project, "beta");
  assert.notEqual(workspace.getSnapshot().executed?.executionId, alphaExecution);
  assert.deepEqual(workspace.getSnapshot().history, []);
  assert.equal(cancelledExecutions.includes(alphaExecution), true);

  workspace.send({
    type: "finiteSucceeded",
    identity: plans.find((plan) => plan.executionId === alphaExecution)!.identity,
    data: [row("wrong-project")],
  });
  assert.equal(workspace.getSnapshot().finite.rows.status, "loading");

  workspace.dispose();
});

test("Reset returns to an unexecuted Historical default and preserves presentation URL fields", () => {
  const { workspace, replacements, cancelledExecutions } = createHarness(
    "/logs?q=error&run=1&range=6h&limit=500&filters=%5B%5D&refresh=30"
      + "&view=table&rows_per_page=200&panel=detail",
  );
  const executionId = workspace.getSnapshot().executed?.executionId;

  workspace.send({ type: "reset" });

  assert.equal(cancelledExecutions.includes(executionId!), true);
  assert.equal(workspace.getSnapshot().executed, null);
  assert.equal(workspace.getSnapshot().finite.rows.status, "idle");
  assert.deepEqual(workspace.getSnapshot().intent, {
    project: "alpha",
    draft: "*",
    range: { mode: "relative", presetId: "1h", seconds: 3_600 },
    filters: [],
    limit: 50,
    refreshSeconds: 0,
    mode: "historical",
    historicalView: "group",
    liveOffset: "5s",
    liveLimit: 100,
  });
  const resetURL = new URL(replacements.at(-1)!, "https://lazy-observe.test");
  assert.equal(resetURL.searchParams.has("q"), false);
  assert.equal(resetURL.searchParams.has("run"), false);
  assert.equal(resetURL.searchParams.has("range"), false);
  assert.equal(resetURL.searchParams.get("panel"), "detail");
  assert.equal(resetURL.searchParams.has("view"), false);
  assert.equal(resetURL.searchParams.has("limit"), false);

  workspace.dispose();
});

test("auto-refresh skips a loading execution instead of cancelling or queueing it", () => {
  const { workspace, plans, clock, cancelledExecutions } = createHarness();
  workspace.send({ type: "run" });
  workspace.send({ type: "refreshSecondsChanged", refreshSeconds: 5 });
  const first = plans.at(-1)!;

  clock.advance(5_000);
  assert.equal(plans.length, 1);
  assert.deepEqual(cancelledExecutions, []);

  workspace.send({
    type: "finiteSucceeded",
    identity: first.identity,
    data: [row("first")],
  });
  clock.advance(5_000);
  assert.equal(plans.length, 2);
  assert.equal(workspace.getSnapshot().finite.rows.status, "loading");
  assert.deepEqual(workspace.getSnapshot().finite.rows.data, [row("first")]);

  workspace.dispose();
});

test("Live mode owns connection, deduplication, batching, and the latest-N bound", () => {
  const { workspace, clock, plans, liveConnections } = createHarness();
  workspace.send({ type: "draftChanged", draft: "service:api" });
  workspace.send({ type: "liveLimitChanged", limit: 10 });
  workspace.send({ type: "viewChanged", view: "live" });

  assert.equal(plans.length, 0);
  assert.equal(workspace.getSnapshot().live.status, "connecting");
  assert.equal(liveConnections.length, 1);
  assert.equal(liveConnections[0].request.query, "service:api");
  assert.equal(workspace.getSnapshot().history[0].query, "service:api");

  liveConnections[0].observer.opened();
  for (let index = 0; index < 12; index += 1) {
    liveConnections[0].observer.row(row(`row-${index}`));
  }
  liveConnections[0].observer.row(row("row-11"));
  assert.deepEqual(workspace.getSnapshot().live.rows, []);
  clock.advance(1_000);

  assert.equal(workspace.getSnapshot().live.status, "open");
  assert.equal(workspace.getSnapshot().live.rows.length, 10);
  assert.equal(workspace.getSnapshot().live.rows[0].message, "row-2");
  assert.equal(workspace.getSnapshot().live.rows.at(-1)?.message, "row-11");
  assert.equal(workspace.getSnapshot().live.dropped, 2);
  assert.equal(workspace.getSnapshot().live.rate, 12);

  workspace.dispose();
});

test("Live retries transient failures with bounded backoff and stops on authorization errors", () => {
  const { workspace, clock, liveConnections } = createHarness();
  workspace.send({ type: "viewChanged", view: "live" });
  liveConnections[0].observer.failed({ message: "gateway unavailable", status: 503 });

  assert.equal(workspace.getSnapshot().live.status, "retrying");
  assert.equal(workspace.getSnapshot().live.retryAttempt, 1);
  assert.equal(liveConnections[0].closed, true);
  clock.advance(999);
  assert.equal(liveConnections.length, 1);
  clock.advance(1);
  assert.equal(liveConnections.length, 2);

  liveConnections[1].observer.opened();
  assert.equal(workspace.getSnapshot().live.retryAttempt, 0);
  liveConnections[1].observer.failed({ message: "forbidden", status: 403 });
  assert.equal(workspace.getSnapshot().live.status, "error");
  assert.equal(workspace.getSnapshot().live.error, "forbidden");
  clock.advance(30_000);
  assert.equal(liveConnections.length, 2);

  workspace.dispose();
});

test("Live retry jitter never exceeds the cap and unknown failures are terminal", () => {
  const { workspace, clock, liveConnections } = createHarness();
  clock.randomValue = 1;
  workspace.send({ type: "viewChanged", view: "live" });

  for (let attempt = 0; attempt < 6; attempt += 1) {
    liveConnections.at(-1)!.observer.failed({ message: "unavailable", status: 503 });
    const retryTimer = [...clock.timers.values()].find((timer) => !timer.repeat);
    assert.ok(retryTimer);
    assert.ok(retryTimer.delay <= 30_000);
    clock.advance(retryTimer.delay);
  }

  liveConnections.at(-1)!.observer.failed({ message: "unexpected failure", kind: "unknown" });
  assert.equal(workspace.getSnapshot().live.status, "error");
  assert.equal([...clock.timers.values()].filter((timer) => !timer.repeat).length, 0);

  workspace.dispose();
});

test("stopping Live closes the network and resume starts a clean window", () => {
  const { workspace, clock, liveConnections } = createHarness();
  workspace.send({ type: "viewChanged", view: "live" });
  const first = liveConnections[0];
  first.observer.opened();
  first.observer.row(row("before-stop"));
  clock.advance(1_000);
  assert.equal(workspace.getSnapshot().live.rows.length, 1);

  workspace.send({ type: "liveStopped" });
  assert.equal(first.closed, true);
  assert.equal(workspace.getSnapshot().live.status, "stopped");
  first.observer.row(row("after-stop"));
  clock.advance(1_000);
  assert.deepEqual(
    workspace.getSnapshot().live.rows.map((item) => item.message),
    ["before-stop"],
  );

  workspace.send({ type: "liveResumed" });
  assert.equal(liveConnections.length, 2);
  assert.equal(workspace.getSnapshot().live.status, "connecting");
  assert.deepEqual(workspace.getSnapshot().live.rows, []);
  assert.equal(workspace.getSnapshot().live.dropped, 0);
  assert.equal(workspace.getSnapshot().history.length, 1);
  liveConnections[1].observer.row(row("before-stop"));
  clock.advance(1_000);
  assert.equal(workspace.getSnapshot().live.rows[0].message, "before-stop");

  workspace.dispose();
});

test("switching modes clears visible results and starts a fresh mode identity", () => {
  const { workspace, clock, plans, liveConnections } = createHarness();
  workspace.send({ type: "run" });
  const historical = plans.at(-1)!;
  workspace.send({
    type: "finiteSucceeded",
    identity: historical.identity,
    data: [row("historical")],
  });

  workspace.send({ type: "viewChanged", view: "live" });
  assert.equal(workspace.getSnapshot().finite.rows.status, "idle");
  liveConnections[0].observer.row(row("live"));
  clock.advance(1_000);
  assert.equal(workspace.getSnapshot().live.rows.length, 1);

  workspace.send({ type: "viewChanged", view: "table" });
  assert.equal(liveConnections[0].closed, true);
  assert.deepEqual(workspace.getSnapshot().live.rows, []);
  assert.equal(workspace.getSnapshot().finite.rows.status, "loading");
  assert.equal(plans.at(-1)?.slot, "rows");

  workspace.dispose();
});

test("external location replaces intent and workspace-authored locations do not loop", () => {
  const { workspace, plans, replacements, cancelledExecutions } = createHarness();
  workspace.send({ type: "run" });
  const previous = plans.at(-1)!;
  const externalLocation = "/logs?q=from-link&run=1&range=15m&panel=detail";

  workspace.send({
    type: "externalLocationChanged",
    location: externalLocation,
  });
  assert.equal(cancelledExecutions.includes(previous.executionId), true);
  assert.equal(workspace.getSnapshot().intent.draft, "from-link");
  assert.equal(workspace.getSnapshot().executed?.query, "from-link");
  assert.deepEqual(workspace.getSnapshot().intent.range, {
    mode: "relative",
    presetId: "15m",
    seconds: 900,
  });
  const replacementCount = replacements.length;

  workspace.send({
    type: "externalLocationChanged",
    location: externalLocation,
  });
  assert.equal(replacements.length, replacementCount);
  assert.equal(plans.length, 2);

  workspace.dispose();
});

test("refresh keeps using the executed snapshot while an edited draft remains run=false", () => {
  const { workspace, plans, replacements } = createHarness();
  workspace.send({ type: "draftChanged", draft: "executed" });
  workspace.send({ type: "run" });
  const first = plans.at(-1)!;
  workspace.send({
    type: "finiteSucceeded",
    identity: first.identity,
    data: [row("done")],
  });
  workspace.send({ type: "draftChanged", draft: "next draft" });
  workspace.send({ type: "limitChanged", limit: 500 });
  workspace.send({ type: "refresh" });

  const refresh = plans.at(-1)!;
  assert.equal(refresh.snapshot.query, "executed");
  assert.equal(refresh.snapshot.limit, 50);
  const url = new URL(replacements.at(-1)!, "https://lazy-observe.test");
  assert.equal(url.searchParams.get("q"), "next draft");
  assert.equal(url.searchParams.has("run"), false);

  workspace.dispose();
});

test("presentation-only location changes do not cancel or re-run exploration", () => {
  const { workspace, plans, cancelledExecutions } = createHarness(
    "/logs?q=error&run=1&range=1h&view=table&rows_per_page=100",
  );
  const plan = plans.at(-1)!;
  workspace.send({
    type: "finiteSucceeded",
    identity: plan.identity,
    data: [row("stable")],
  });

  workspace.send({
    type: "externalLocationChanged",
    location: "/logs?q=error&run=1&range=1h&view=json&rows_per_page=500&panel=detail",
  });

  assert.equal(plans.length, 1);
  assert.deepEqual(cancelledExecutions, []);
  assert.deepEqual(workspace.getSnapshot().finite.rows.data, [row("stable")]);
  assert.equal(workspace.getSnapshot().intent.historicalView, "json");

  workspace.dispose();
});

test("changing chart configuration re-runs only the selected auxiliary slot", () => {
  const { workspace, plans } = createHarness();
  workspace.send({ type: "run" });
  const rowsPlan = plans.at(-1)!;
  workspace.send({
    type: "finiteSucceeded",
    identity: rowsPlan.identity,
    data: [row("stable")],
  });

  workspace.send({
    type: "configureAuxiliaryQueries",
    config: {
      rowsEnabled: true,
      chartEnabled: true,
      graphMode: "hits",
      groupBy: "service",
      topN: 5,
      step: null,
    },
  });

  assert.deepEqual(plans.map((plan) => plan.slot), ["rows", "hits"]);
  assert.deepEqual(workspace.getSnapshot().finite.rows.data, [row("stable")]);

  workspace.dispose();
});

test("mode switching writes history only when it executes a new draft", () => {
  const { workspace, clock } = createHarness();
  workspace.send({ type: "draftChanged", draft: "first" });
  workspace.send({ type: "run" });
  const firstRunAt = workspace.getSnapshot().history[0].lastRunAt;
  clock.advance(5_000);

  workspace.send({ type: "viewChanged", view: "live" });
  assert.equal(workspace.getSnapshot().history[0].lastRunAt, firstRunAt);

  workspace.send({ type: "draftChanged", draft: "second" });
  workspace.send({ type: "viewChanged", view: "table" });
  assert.deepEqual(workspace.getSnapshot().history.map((entry) => entry.query), [
    "second",
    "first",
  ]);

  workspace.dispose();
});

test("a deferred workspace starts URL-requested work only after adapter activation", () => {
  const plans: LogsFiniteQueryPlan[] = [];
  const workspace = createLogsExplorationWorkspace({
    project: "alpha",
    location: "/logs?q=*&run=1&range=1h",
    clock: new FakeClock(),
    history: createHistoryAdapter(),
    deferInitialExecution: true,
    onLocationReplace: () => undefined,
    onFiniteQueryPlan: (plan) => plans.push(plan),
    onFiniteQueryCancel: () => undefined,
    openLiveStream: () => ({ close() {} }),
  });

  assert.equal(plans.length, 0);
  assert.equal(workspace.getSnapshot().executed, null);
  workspace.send({ type: "activated" });
  assert.equal(plans.length, 1);
  assert.equal(workspace.getSnapshot().finite.rows.status, "loading");

  workspace.dispose();
});
