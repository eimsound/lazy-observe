import { describe, expect, it } from "vitest";
import { MAX_SIZE_ESTIMATE_DEPTH, ReplayRingBuffer, sizeOf } from "../src/replay-buffer";

it("keeps only events inside the time window", () => {
  const ring = new ReplayRingBuffer<{ timestamp: number; value: string }>(1_000);
  ring.push({ timestamp: 1_000, value: "old" });
  ring.push({ timestamp: 1_500, value: "middle" });
  ring.push({ timestamp: 2_001, value: "new" });
  expect(ring.snapshot().map((event) => event.value)).toEqual(["middle", "new"]);
  expect(ring.drain()).toHaveLength(2);
  expect(ring.length).toBe(0);
});

it("evicts oldest events when byte budget is exceeded", () => {
  const ring = new ReplayRingBuffer<{ timestamp: number; value: string }>(60_000, 80);
  ring.push({ timestamp: 1, value: "a".repeat(30) });
  ring.push({ timestamp: 2, value: "b".repeat(30) });
  expect(ring.snapshot()).toHaveLength(1);
  expect(ring.snapshot()[0].value).toContain("b");
});

it("retains latest two checkout generations so snapshot() always starts with Meta + FullSnapshot after 60s+", () => {
  const ring = new ReplayRingBuffer<{ timestamp: number; type: number; label: string }>(60_000, 5_000_000);
  ring.push({ timestamp: 0, type: 4, label: "meta-0" });
  ring.push({ timestamp: 0, type: 2, label: "full-0" });
  ring.push({ timestamp: 10_000, type: 3, label: "inc-10" });
  ring.push({ timestamp: 30_000, type: 4, label: "meta-30" }, true);
  ring.push({ timestamp: 30_000, type: 2, label: "full-30" }, true);
  ring.push({ timestamp: 45_000, type: 3, label: "inc-45" });
  ring.push({ timestamp: 60_000, type: 4, label: "meta-60" }, true);
  ring.push({ timestamp: 60_000, type: 2, label: "full-60" }, true);
  ring.push({ timestamp: 75_000, type: 5, label: "error-75" });

  const labels = ring.snapshot().map((event) => event.label);
  expect(labels[0]).toBe("meta-30");
  expect(labels[1]).toBe("full-30");
  expect(labels).not.toContain("meta-0");
  expect(labels).not.toContain("full-0");
  expect(labels).toContain("meta-60");
  expect(labels).toContain("full-60");
  expect(labels[labels.length - 1]).toBe("error-75");
});

it("preserves the initial Meta + FullSnapshot during >60s idle when no checkout occurred yet", () => {
  const ring = new ReplayRingBuffer<{ timestamp: number; type: number; label: string }>(60_000, 5_000_000);
  ring.push({ timestamp: 0, type: 4, label: "meta-0" });
  ring.push({ timestamp: 0, type: 2, label: "full-0" });
  ring.push({ timestamp: 75_000, type: 5, label: "error-75" });

  expect(ring.snapshot().map((event) => event.label)).toEqual(["meta-0", "full-0", "error-75"]);
});

describe("sizeOf zero-alloc byte estimator", () => {
  const encoder = new TextEncoder();
  const actualUtf8JsonBytes = (value: unknown): number => encoder.encode(JSON.stringify(value) ?? "").byteLength;

  it("matches exact length for plain ASCII and UTF-8 / CJK upper bound for multi-byte strings", () => {
    for (const s of ["", "hello world", "a".repeat(30), "tag-name-v1"]) {
      expect(sizeOf(s)).toBe(actualUtf8JsonBytes(s));
    }

    for (const s of [
      "café αβγ",
      "中文测试数据：性能预算与回放片段",
      "emoji 🚀🎉👨‍👩‍👧‍👦 end",
    ]) {
      const est = sizeOf(s);
      expect(est).toBeGreaterThanOrEqual(actualUtf8JsonBytes(s));
      expect(est).toBeLessThanOrEqual(s.length * 3 + 2);
    }

    const loneSurrogate = "lone surrogate \ud800 and \udfff end";
    expect(sizeOf(loneSurrogate)).toBeGreaterThanOrEqual(actualUtf8JsonBytes(loneSurrogate));
    expect(sizeOf(loneSurrogate)).toBeLessThanOrEqual(loneSurrogate.length * 3 + 2);
  });

  it("never underestimates JSON escape expansion, lone surrogates, emoji pairs, or non-ASCII/escaped object keys", () => {
    const stringPayloads = [
      "\x01".repeat(10),
      '"'.repeat(20),
      "\\".repeat(20),
      "\u0001中",
      "\ud800",
      "a\udc00b",
      "\ud800\ud800\udc00",
      "😀🎉",
      "\b\t\n\f\r",
      "\x00\x07\x0b\x0e\x1f",
    ];
    for (const s of stringPayloads) {
      expect(sizeOf(s)).toBeGreaterThanOrEqual(actualUtf8JsonBytes(s));
    }

    const objectPayloads: Array<Record<string, unknown>> = [
      { "a中文测试": 1 },
      { "a\u0001b": 1 },
      { 'a"b\\c': 1 },
      {
        "mixed_键_\"\\_\n": "值\u0001😀\ud800\"\\",
        nested: {
          "prefix\udc00suffix": ["\b\t\n\f\r", "中文", "plain-ascii", "\x1f"],
        },
      },
    ];
    for (const obj of objectPayloads) {
      expect(sizeOf(obj)).toBeGreaterThanOrEqual(actualUtf8JsonBytes(obj));

      // Exercise walkSafe via re-entrant sizeOf() inside .toJSON().
      let innerEstimate = -1;
      const wrapper = {
        toJSON() {
          innerEstimate = sizeOf(obj);
          return null;
        },
      };
      sizeOf(wrapper);
      expect(innerEstimate).toBeGreaterThanOrEqual(actualUtf8JsonBytes(obj));
    }
  });

  it("honors custom .toJSON() on Array instances in both normal and re-entrant (walkSafe) paths", () => {
    const makeArrayWithToJSON = () => {
      const arr = [1, 2, 3] as number[] & { toJSON: (key: string) => unknown };
      arr.toJSON = (key: string) => ({ key, items: ["ok", "中文"] });
      return arr;
    };

    const directArr = makeArrayWithToJSON();
    expect(sizeOf(directArr)).toBeGreaterThanOrEqual(actualUtf8JsonBytes(directArr));
    expect(sizeOf({ list: directArr })).toBeGreaterThanOrEqual(actualUtf8JsonBytes({ list: directArr }));

    const arrUndef = [1, 2] as number[] & { toJSON: () => unknown };
    arrUndef.toJSON = () => undefined;
    expect(sizeOf({ a: 1, skip: arrUndef })).toBe(actualUtf8JsonBytes({ a: 1, skip: arrUndef }));
    expect(sizeOf([arrUndef, 2])).toBe(actualUtf8JsonBytes([arrUndef, 2]));

    // Exercise walkSafe via re-entrant sizeOf() inside .toJSON().
    let safeDirect = -1;
    let safeNested = -1;
    let safeOmittedObj = -1;
    let safeOmittedArr = -1;
    sizeOf({
      toJSON() {
        safeDirect = sizeOf(directArr);
        safeNested = sizeOf({ list: directArr });
        safeOmittedObj = sizeOf({ a: 1, skip: arrUndef });
        safeOmittedArr = sizeOf([arrUndef, 2]);
        return null;
      },
    });
    expect(safeDirect).toBe(sizeOf(directArr));
    expect(safeNested).toBe(sizeOf({ list: directArr }));
    expect(safeOmittedObj).toBe(actualUtf8JsonBytes({ a: 1, skip: arrUndef }));
    expect(safeOmittedArr).toBe(actualUtf8JsonBytes([arrUndef, 2]));
  });

  it("handles nested rrweb-like structures, omitted properties, and edge primitives", () => {
    const obj = {
      type: 3,
      timestamp: 1_700_000_000_000,
      negative: -42,
      zero: 0,
      flag: true,
      off: false,
      empty: null,
      skipUndef: undefined,
      skipFn: () => 1,
      skipSym: Symbol("x"),
      中文键: "中文值",
      nested: {
        arr: [1, "中文", true, undefined, () => {}, Symbol("y"), null],
        emptyObj: {},
        emptyArr: [],
      },
    };
    const est = sizeOf(obj);
    expect(est).toBeGreaterThanOrEqual(actualUtf8JsonBytes(obj));

    expect(sizeOf(undefined)).toBe(0);
    expect(sizeOf(() => {})).toBe(0);
    expect(sizeOf(Symbol("root"))).toBe(0);
    expect(sizeOf(null)).toBe(4);
    expect(sizeOf(true)).toBe(4);
    expect(sizeOf(false)).toBe(5);
    expect(sizeOf(NaN)).toBe(4);
    expect(sizeOf(Infinity)).toBe(4);
    expect(sizeOf(-Infinity)).toBe(4);
    expect(sizeOf(3.14159)).toBeGreaterThanOrEqual(actualUtf8JsonBytes(3.14159));
    expect(sizeOf(12345678901234567890n)).toBe(20);

    const nullProto = Object.assign(Object.create(null) as Record<string, unknown>, { a: "ok", b: 12 });
    expect(sizeOf(nullProto)).toBe(actualUtf8JsonBytes(nullProto));

    expect(sizeOf(new String("abc"))).toBe(actualUtf8JsonBytes(new String("abc")));
    expect(sizeOf(new Number(42))).toBe(actualUtf8JsonBytes(new Number(42)));
    expect(sizeOf(new Boolean(true))).toBe(actualUtf8JsonBytes(new Boolean(true)));
  });

  it("counts shared non-cyclic DAG references accurately without dropping repeats", () => {
    const shared = { tag: "node", attrs: { class: "btn" } };
    const single = { a: shared };
    const dag = { a: shared, b: shared, list: [shared, shared] };
    expect(sizeOf(dag)).toBeGreaterThan(sizeOf(single) * 2);
    expect(sizeOf(dag)).toBeGreaterThanOrEqual(actualUtf8JsonBytes(dag));
  });

  it("guards against shallow and deep circular references via ancestor stack and WeakSet promotion", () => {
    const self: Record<string, unknown> = { a: 1 };
    self.self = self;
    expect(sizeOf(self)).toBeGreaterThan(0);

    const a: Record<string, unknown> = { name: "a" };
    const b: Record<string, unknown> = { name: "b", a };
    a.b = b;
    expect(sizeOf(a)).toBeGreaterThan(0);

    const arr: unknown[] = [1];
    arr.push(arr);
    expect(sizeOf(arr)).toBeGreaterThan(0);

    // Build a cycle >= 20 levels deep to exercise WeakSet promotion at depth >= 16.
    const deepRoot: Record<string, unknown> = { id: 0 };
    let cursor = deepRoot;
    for (let i = 1; i <= 22; i++) {
      const next: Record<string, unknown> = { id: i };
      cursor.child = next;
      cursor = next;
    }
    cursor.child = deepRoot;
    expect(sizeOf(deepRoot)).toBeGreaterThan(0);
  });

  it("caps recursion cleanly past MAX_SIZE_ESTIMATE_DEPTH", () => {
    let deep: Record<string, unknown> = { leaf: "x" };
    for (let i = 0; i < MAX_SIZE_ESTIMATE_DEPTH + 36; i++) {
      deep = { child: deep };
    }
    expect(sizeOf(deep)).toBeGreaterThan(0);
  });

  it("supports .toJSON(), Date, re-entrant toJSON, cyclic toJSON, and throwing getters safely", () => {
    const d = new Date("2026-11-23T04:30:00.000Z");
    expect(sizeOf({ at: d })).toBeGreaterThanOrEqual(actualUtf8JsonBytes({ at: d }));

    const custom = {
      nested: { raw: true },
      toJSON(key: string) {
        return { key, ok: true };
      },
    };
    expect(sizeOf({ item: custom })).toBe(actualUtf8JsonBytes({ item: custom }));

    const reentrant = {
      toJSON() {
        const inner = sizeOf({ inner: "ok" });
        return { inner };
      },
    };
    expect(sizeOf(reentrant)).toBe(actualUtf8JsonBytes({ inner: 14 }));

    const omits = {
      a: 1,
      b: { toJSON: () => undefined },
      arr: [{ toJSON: () => undefined }, 2],
    };
    expect(sizeOf(omits)).toBe(actualUtf8JsonBytes(omits));

    const cyclicToJSON: Record<string, unknown> = {
      toJSON() {
        return cyclicToJSON;
      },
    };
    expect(sizeOf({ x: cyclicToJSON })).toBeGreaterThanOrEqual(0);

    const throwing = {
      toJSON() {
        throw new Error("boom");
      },
    };
    expect(() => sizeOf(throwing)).not.toThrow();

    const throwingGetter = Object.defineProperty({}, "bad", {
      enumerable: true,
      get() {
        throw new Error("getter boom");
      },
    });
    expect(() => sizeOf(throwingGetter)).not.toThrow();
  });
});
