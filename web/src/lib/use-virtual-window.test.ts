import assert from "node:assert/strict";
import { test } from "vitest";

import {
  DEFAULT_OVERSCAN,
  DEFAULT_VIRTUALIZE_THRESHOLD,
  MEASURE_TOLERANCE_PX,
  buildVirtualLayout,
  computeScrollToIndexOffset,
  computeVirtualItems,
  computeVirtualRange,
  pruneMeasuredSizes,
  reconcileScrollTopOnLayoutChange,
  shouldCommitScrollTop,
  syncMeasureStoreForResetKey,
  type VirtualMeasureStore,
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

test("reconcileScrollTopOnLayoutChange prevents blank screen after scroll-to-bottom -> shrink/empty -> restore", () => {
  const viewportHeight = 640;
  const fullLayout = buildVirtualLayout(2_000, 30); // totalSize = 60,000
  const bottomScrollTop = fullLayout.totalSize - viewportHeight; // 59,360

  // Scenario A: scrolled to bottom -> collapsed to <= 80 rows -> expanded back to 2,000 rows.
  const collapsedLayout = buildVirtualLayout(40, 30);
  const afterCollapseTop = reconcileScrollTopOnLayoutChange({
    isVirtualized: false,
    totalSize: collapsedLayout.totalSize,
    viewportHeight,
    currentScrollTop: bottomScrollTop,
    domScrollTop: 0,
  });
  assert.equal(afterCollapseTop, 0);

  const restoredRangeA = computeVirtualRange({
    layout: fullLayout,
    scrollTop: afterCollapseTop,
    viewportHeight,
  });
  assert.equal(restoredRangeA.isVirtualized, true);
  assert.equal(restoredRangeA.startIndex, 0);
  assert.equal(restoredRangeA.paddingTop, 0);
  assert.ok(restoredRangeA.endIndex > 0);

  // Scenario B: scrolled to bottom -> cleared to 0 rows (container unmounted) -> reloaded to 2,000 rows.
  const emptyTop = reconcileScrollTopOnLayoutChange({
    isVirtualized: false,
    totalSize: 0,
    viewportHeight,
    currentScrollTop: bottomScrollTop,
    domScrollTop: null,
  });
  assert.equal(emptyTop, 0);

  // Scenario C: scrolled to bottom -> shrunk to shorter virtualized list (500 rows) -> expanded back to 2,000 rows.
  const shorterLayout = buildVirtualLayout(500, 30); // totalSize = 15,000; maxScrollTop = 14,360
  const afterShrinkTop = reconcileScrollTopOnLayoutChange({
    isVirtualized: true,
    totalSize: shorterLayout.totalSize,
    viewportHeight,
    currentScrollTop: bottomScrollTop,
    domScrollTop: 14_360,
  });
  assert.equal(afterShrinkTop, 14_360);

  const restoredRangeC = computeVirtualRange({
    layout: fullLayout,
    scrollTop: afterShrinkTop,
    viewportHeight,
  });
  // Visible window at restored scrollTop = 14,360 must cover row floor(14360 / 30) = 478.
  assert.ok(restoredRangeC.startIndex <= 478 && restoredRangeC.endIndex > 478);
});

test("shouldCommitScrollTop commits when exiting virtualization or scrolling upward from clamped tail", () => {
  const layout = buildVirtualLayout(200, 30); // totalSize = 6,000
  const viewportHeight = 640;

  // Scrolling a few pixels inside the same middle slice skips commit.
  const midPrev = computeVirtualRange({ layout, scrollTop: 1_500, viewportHeight });
  const midNext = computeVirtualRange({ layout, scrollTop: 1_505, viewportHeight });
  assert.equal(
    shouldCommitScrollTop({
      prevRange: midPrev,
      nextRange: midNext,
      prevScrollTop: 1_500,
      nextScrollTop: 1_505,
      count: layout.count,
    }),
    false,
  );

  // Scrolling upward while endIndex === count commits so state never lags above DOM scrollTop.
  const tailPrev = computeVirtualRange({ layout, scrollTop: 5_360, viewportHeight });
  const tailNext = computeVirtualRange({ layout, scrollTop: 5_350, viewportHeight });
  assert.equal(tailPrev.endIndex, 200);
  assert.equal(
    shouldCommitScrollTop({
      prevRange: tailPrev,
      nextRange: tailNext,
      prevScrollTop: 5_360,
      nextScrollTop: 5_350,
      count: layout.count,
    }),
    true,
  );

  // Exiting virtualization with non-zero prevScrollTop commits reset.
  const smallLayout = buildVirtualLayout(20, 30);
  const smallRange = computeVirtualRange({ layout: smallLayout, scrollTop: 0, viewportHeight });
  assert.equal(
    shouldCommitScrollTop({
      prevRange: smallRange,
      nextRange: smallRange,
      prevScrollTop: 1_200,
      nextScrollTop: 0,
      count: smallLayout.count,
    }),
    true,
  );
});

test("pruneMeasuredSizes evicts stale numeric indices on shrink and clears on empty list", () => {
  const numericCache = new Map<string | number, number>([
    [0, 24],
    [10, 80],
    [50, 120],
    [99, 200],
  ]);

  assert.equal(pruneMeasuredSizes(numericCache, 100), false);
  assert.equal(numericCache.size, 4);

  assert.equal(pruneMeasuredSizes(numericCache, 50), true);
  assert.deepEqual([...numericCache.keys()], [0, 10]);

  assert.equal(pruneMeasuredSizes(numericCache, 0), true);
  assert.equal(numericCache.size, 0);

  // Custom keyed cache evicts inactive keys when exceeding size threshold.
  const keyedCache = new Map<string | number, number>();
  for (let i = 0; i < 300; i++) {
    keyedCache.set(`row-${i}:0`, 24);
  }
  assert.equal(
    pruneMeasuredSizes(keyedCache, 10, (i) => `row-${i}:0`),
    true,
  );
  assert.equal(keyedCache.size, 10);
  assert.equal(keyedCache.has("row-0:0"), true);
  assert.equal(keyedCache.has("row-299:0"), false);
});

test("reconcileScrollTopOnLayoutChange tracks real domScrollTop when collapsing a tall expanded row and preserves py-2 bottom scroll", () => {
  const viewportHeight = 640;

  // Regression: 500 rows (24px each, totalSize = 12,000). Row 100 was expanded to 2,246px,
  // user scrolled down inside row 100 (committedScrollTop = 4,500) and then scrolled back up
  // to row 100's top (domScrollTop = 2,400) without crossing a slice boundary, then collapsed it.
  // Even though 4,500 <= maxScrollTop (11,360), layout reconciliation MUST converge to domScrollTop (2,400).
  const collapsedLayout = buildVirtualLayout(500, 24);
  const reconciled = reconcileScrollTopOnLayoutChange({
    isVirtualized: true,
    totalSize: collapsedLayout.totalSize,
    viewportHeight,
    currentScrollTop: 4_500,
    domScrollTop: 2_400,
    domScrollHeight: collapsedLayout.totalSize + 16,
  });
  assert.equal(reconciled, 2_400);

  const range = computeVirtualRange({
    layout: collapsedLayout,
    scrollTop: reconciled,
    viewportHeight,
  });
  // Row 100 sits at offset 2,400px and MUST remain mounted inside the visible window (no blank screen).
  assert.ok(range.startIndex <= 100 && range.endIndex > 100);

  // Regression: container has py-2 (16px vertical padding), so el.scrollHeight = totalSize + 16.
  // Scrolling all the way to the bottom (scrollTop = totalSize + 16 - viewportHeight) must NOT
  // be pulled upward by 16px on layout updates.
  const paddedLayout = buildVirtualLayout(200, 24); // totalSize = 4,800
  const paddedScrollHeight = paddedLayout.totalSize + 16; // 4,816
  const atTrueBottom = paddedScrollHeight - viewportHeight; // 4,176
  assert.equal(
    reconcileScrollTopOnLayoutChange({
      isVirtualized: true,
      totalSize: paddedLayout.totalSize,
      viewportHeight,
      currentScrollTop: atTrueBottom,
      domScrollTop: atTrueBottom,
      domScrollHeight: paddedScrollHeight,
    }),
    atTrueBottom,
  );
});

test("syncMeasureStoreForResetKey isolates measured heights synchronously in render before child commit measurements", () => {
  const store: VirtualMeasureStore = {
    resetKey: "trace-a",
    sizes: new Map<string | number, number>([
      [0, 120],
      [1, 240],
    ]),
  };

  // Same resetKey -> no-op, existing measurements preserved.
  assert.equal(syncMeasureStoreForResetKey(store, "trace-a"), false);
  assert.equal(store.sizes.get(0), 120);

  // Switching resetKey synchronously swaps in a fresh Map during render.
  assert.equal(syncMeasureStoreForResetKey(store, "trace-b"), true);
  assert.equal(store.sizes.size, 0);

  // Child measureElement callbacks during the same commit record new heights for trace-b.
  store.sizes.set(0, 88);
  store.sizes.set(1, 144);

  // Subsequent render with the same resetKey keeps trace-b's first-frame measurements intact.
  assert.equal(syncMeasureStoreForResetKey(store, "trace-b"), false);
  assert.equal(store.sizes.get(0), 88);
  assert.equal(store.sizes.get(1), 144);

  // Measurement deadband absorbs 0.5px subpixel differences (e.g. 24px vs 23.5px).
  assert.ok(MEASURE_TOLERANCE_PX >= 1);
});
