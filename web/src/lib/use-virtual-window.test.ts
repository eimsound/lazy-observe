import assert from "node:assert/strict";
import { test } from "vitest";

import {
  DEFAULT_OVERSCAN,
  DEFAULT_VIRTUALIZE_THRESHOLD,
  buildVirtualLayout,
  computeScrollToIndexOffset,
  computeVirtualItems,
  computeVirtualRange,
} from "./use-virtual-window.ts";

test("buildVirtualLayout uses O(1) fixed-height fast path when estimateSize is constant and unmeasured", () => {
  const layout = buildVirtualLayout(2_000, 30);
  assert.equal(layout.count, 2_000);
  assert.equal(layout.isFixed, true);
  assert.equal(layout.fixedSize, 30);
  assert.equal(layout.offsets, null);
  assert.equal(layout.totalSize, 60_000);
  assert.equal(layout.getOffset(0), 0);
  assert.equal(layout.getOffset(500), 15_000);
  assert.equal(layout.getOffset(2_000), 60_000);
  assert.equal(layout.getSize(0), 30);
  assert.equal(layout.getSize(1_999), 30);
  assert.equal(layout.getSize(-1), 0);
  assert.equal(layout.getSize(2_000), 0);
});

test("buildVirtualLayout handles empty, negative, and invalid sizes safely", () => {
  const empty = buildVirtualLayout(0, 30);
  assert.equal(empty.count, 0);
  assert.equal(empty.totalSize, 0);
  assert.equal(empty.getOffset(0), 0);

  const negative = buildVirtualLayout(-5, 30);
  assert.equal(negative.count, 0);
  assert.equal(negative.totalSize, 0);

  const clamped = buildVirtualLayout(3, Number.NaN);
  assert.equal(clamped.fixedSize, 1);
  assert.equal(clamped.totalSize, 3);

  const variableClamped = buildVirtualLayout(3, () => -10);
  assert.equal(variableClamped.totalSize, 3);
  assert.equal(variableClamped.getSize(1), 1);
});

test("buildVirtualLayout builds prefix sums for variable estimates and measured height overrides", () => {
  const measured = new Map<string | number, number>([
    ["row-1:1", 220],
    ["row-3:0", 26],
  ]);
  const getKey = (i: number) => (i === 1 ? "row-1:1" : `row-${i}:0`);
  const estimate = (i: number) => (i === 1 ? 180 : 24);

  const layout = buildVirtualLayout(5, estimate, measured, getKey);
  assert.equal(layout.isFixed, false);
  assert.ok(layout.offsets instanceof Float64Array);
  assert.equal(layout.getSize(0), 24);
  assert.equal(layout.getSize(1), 220); // measured override wins
  assert.equal(layout.getSize(2), 24);
  assert.equal(layout.getSize(3), 26); // measured override wins
  assert.equal(layout.getSize(4), 24);
  assert.equal(layout.getOffset(0), 0);
  assert.equal(layout.getOffset(1), 24);
  assert.equal(layout.getOffset(2), 244);
  assert.equal(layout.getOffset(3), 268);
  assert.equal(layout.getOffset(4), 294);
  assert.equal(layout.totalSize, 318);
});

test("computeVirtualRange bypasses virtualization at or below threshold and activates above it", () => {
  const smallLayout = buildVirtualLayout(DEFAULT_VIRTUALIZE_THRESHOLD, 30);
  const smallRange = computeVirtualRange({
    layout: smallLayout,
    scrollTop: 500,
    viewportHeight: 640,
  });
  assert.equal(smallRange.isVirtualized, false);
  assert.equal(smallRange.startIndex, 0);
  assert.equal(smallRange.endIndex, DEFAULT_VIRTUALIZE_THRESHOLD);
  assert.equal(smallRange.paddingTop, 0);
  assert.equal(smallRange.paddingBottom, 0);
  assert.equal(smallRange.totalSize, DEFAULT_VIRTUALIZE_THRESHOLD * 30);

  const aboveLayout = buildVirtualLayout(DEFAULT_VIRTUALIZE_THRESHOLD + 1, 30);
  const aboveRange = computeVirtualRange({
    layout: aboveLayout,
    scrollTop: 0,
    viewportHeight: 640,
  });
  assert.equal(aboveRange.isVirtualized, true);
  assert.equal(aboveRange.startIndex, 0);
  assert.equal(aboveRange.endIndex, Math.ceil(640 / 30) + DEFAULT_OVERSCAN);
  assert.equal(aboveRange.paddingTop, 0);
  assert.equal(
    aboveRange.paddingBottom,
    (DEFAULT_VIRTUALIZE_THRESHOLD + 1 - aboveRange.endIndex) * 30,
  );

  const disabledRange = computeVirtualRange({
    layout: aboveLayout,
    scrollTop: 0,
    viewportHeight: 640,
    enabled: false,
  });
  assert.equal(disabledRange.isVirtualized, false);
  assert.equal(disabledRange.endIndex, DEFAULT_VIRTUALIZE_THRESHOLD + 1);
});

test("computeVirtualRange maintains exact total height invariant across top, middle, and bottom scroll positions", () => {
  const layout = buildVirtualLayout(2_000, 30);
  const viewportHeight = 640;
  const overscan = 12;

  for (const scrollTop of [0, 15, 30, 15_000, 30_005, 59_360, 999_999]) {
    const range = computeVirtualRange({
      layout,
      scrollTop,
      viewportHeight,
      overscan,
    });
    assert.equal(range.isVirtualized, true);
    assert.ok(range.endIndex > range.startIndex);
    assert.ok(range.endIndex - range.startIndex <= Math.ceil(viewportHeight / 30) + overscan * 2 + 1);

    const items = computeVirtualItems(layout, range.startIndex, range.endIndex);
    const renderedSize = items.reduce((sum, item) => sum + item.size, 0);
    assert.equal(range.paddingTop + renderedSize + range.paddingBottom, layout.totalSize);
    assert.equal(items[0].start, range.paddingTop);
    assert.equal(items[items.length - 1].end, layout.totalSize - range.paddingBottom);
  }
});

test("computeVirtualRange binary search handles variable heights and exact row boundaries", () => {
  const sizes = Array.from({ length: 200 }, (_, i) => {
    if (i === 5) return 320;
    if (i === 50) return 480;
    if (i % 15 === 0) return 96;
    return 24;
  });
  const layout = buildVirtualLayout(sizes.length, (i) => sizes[i]);
  const viewportHeight = 300;
  const overscan = 0;

  // Check exact boundary right before, at, and right after expanded row #50.
  const row50Start = layout.getOffset(50);
  const row50End = row50Start + 480;

  const before = computeVirtualRange({
    layout,
    scrollTop: row50Start - 1,
    viewportHeight,
    overscan,
  });
  assert.equal(before.startIndex, 49);

  const exact = computeVirtualRange({
    layout,
    scrollTop: row50Start,
    viewportHeight,
    overscan,
  });
  assert.equal(exact.startIndex, 50);
  // Row 50 alone (480px) covers the entire 300px viewport.
  assert.equal(exact.endIndex, 51);

  const after = computeVirtualRange({
    layout,
    scrollTop: row50End,
    viewportHeight,
    overscan,
  });
  assert.equal(after.startIndex, 51);

  const items = computeVirtualItems(layout, after.startIndex, after.endIndex);
  const renderedSize = items.reduce((sum, item) => sum + item.size, 0);
  assert.equal(after.paddingTop + renderedSize + after.paddingBottom, layout.totalSize);
});

test("computeVirtualRange clamps scrollTop when dataset shrinks (e.g. collapsing trace subtrees)", () => {
  const shrunkLayout = buildVirtualLayout(120, 30); // totalSize = 3600
  const range = computeVirtualRange({
    layout: shrunkLayout,
    scrollTop: 50_000, // stale scrollTop from when there were 2000 rows
    viewportHeight: 640,
    overscan: 10,
  });

  assert.equal(range.isVirtualized, true);
  assert.equal(range.endIndex, 120);
  assert.ok(range.startIndex < 120);
  assert.equal(range.paddingBottom, 0);
});

test("computeScrollToIndexOffset respects auto, start, center, and end alignment on fixed and variable layouts", () => {
  const fixed = buildVirtualLayout(2_000, 30);
  const viewportHeight = 600; // 20 rows visible

  // Already visible at [300, 900) -> auto returns null.
  assert.equal(
    computeScrollToIndexOffset({
      layout: fixed,
      index: 15, // [450, 480)
      scrollTop: 300,
      viewportHeight,
      align: "auto",
    }),
    null,
  );

  // Above viewport -> auto aligns to itemStart.
  assert.equal(
    computeScrollToIndexOffset({
      layout: fixed,
      index: 5, // [150, 180)
      scrollTop: 300,
      viewportHeight,
      align: "auto",
    }),
    150,
  );

  // Below viewport -> auto aligns bottom of item with bottom of viewport.
  assert.equal(
    computeScrollToIndexOffset({
      layout: fixed,
      index: 100, // [3000, 3030)
      scrollTop: 300,
      viewportHeight,
      align: "auto",
    }),
    3030 - viewportHeight,
  );

  // Explicit alignments.
  assert.equal(
    computeScrollToIndexOffset({
      layout: fixed,
      index: 100,
      scrollTop: 0,
      viewportHeight,
      align: "start",
    }),
    3000,
  );
  assert.equal(
    computeScrollToIndexOffset({
      layout: fixed,
      index: 100,
      scrollTop: 0,
      viewportHeight,
      align: "center",
    }),
    3000 - (viewportHeight - 30) / 2,
  );
  assert.equal(
    computeScrollToIndexOffset({
      layout: fixed,
      index: 100,
      scrollTop: 0,
      viewportHeight,
      align: "end",
    }),
    3030 - viewportHeight,
  );

  // Clamping at boundaries and empty list.
  assert.equal(
    computeScrollToIndexOffset({
      layout: fixed,
      index: 9_999,
      scrollTop: 0,
      viewportHeight,
      align: "start",
    }),
    fixed.totalSize - viewportHeight,
  );
  assert.equal(
    computeScrollToIndexOffset({
      layout: buildVirtualLayout(0, 30),
      index: 0,
      scrollTop: 0,
      viewportHeight,
    }),
    null,
  );

  // Variable-height targeting accounts for preceding expanded rows.
  const variable = buildVirtualLayout(100, (i) => (i === 10 ? 300 : 24));
  const targetOffset = computeScrollToIndexOffset({
    layout: variable,
    index: 50,
    scrollTop: 0,
    viewportHeight: 400,
    align: "start",
  });
  assert.equal(targetOffset, 49 * 24 + 300);
});
