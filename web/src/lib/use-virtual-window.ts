import {
  useCallback,
  useEffect,
  useLayoutEffect,
  useMemo,
  useRef,
  useState,
  type RefCallback,
} from "react";

/**
 * Lightweight, zero-dependency virtual windowing engine + React hook.
 *
 * Design highlights:
 * - Pure, DOM-free layout and range calculators (`buildVirtualLayout`,
 *   `computeVirtualRange`, `computeVirtualItems`, `computeScrollToIndexOffset`)
 *   so every boundary condition is deterministically unit-tested.
 * - O(1) fast path for fixed-height lists (e.g. TraceWaterfall); O(log N)
 *   binary search over a Float64Array prefix-sum for variable/measured heights
 *   (e.g. LogsJsonView with expandable multi-line rows).
 * - Small-dataset bypass (`count <= virtualizeThreshold`, default 80) renders
 *   all items directly with zero spacer overhead.
 * - Single shared ResizeObserver measures mounted variable-height rows and
 *   corrects scrollTop when an above-viewport overscan row changes height.
 */

export const DEFAULT_VIRTUALIZE_THRESHOLD = 80;
export const DEFAULT_OVERSCAN = 12;
export const DEFAULT_VIEWPORT_HEIGHT = 640;

export type VirtualScrollAlign = "auto" | "start" | "center" | "end";

export interface VirtualItem {
  index: number;
  start: number;
  size: number;
  end: number;
}

export interface VirtualRange {
  startIndex: number;
  endIndex: number;
  paddingTop: number;
  paddingBottom: number;
  totalSize: number;
  isVirtualized: boolean;
}

export interface VirtualWindowLayout {
  readonly count: number;
  readonly totalSize: number;
  readonly isFixed: boolean;
  readonly fixedSize: number;
  readonly offsets: Float64Array | null;
  getOffset(index: number): number;
  getSize(index: number): number;
}

function sanitizeSize(size: number): number {
  if (!Number.isFinite(size) || size < 1) return 1;
  return size;
}

function sanitizeCount(count: number): number {
  if (!Number.isFinite(count) || count <= 0) return 0;
  return Math.floor(count);
}

/**
 * Builds the vertical geometry model for `count` items.
 *
 * Uses an allocation-free O(1) layout when `estimateSize` is a constant number
 * and no measured overrides exist; otherwise builds a Float64Array prefix sum.
 */
export function buildVirtualLayout(
  rawCount: number,
  estimateSize: number | ((index: number) => number),
  measuredSizes?: ReadonlyMap<string | number, number>,
  getItemKey?: (index: number) => string | number,
): VirtualWindowLayout {
  const count = sanitizeCount(rawCount);
  const hasMeasured = measuredSizes !== undefined && measuredSizes.size > 0;

  if (typeof estimateSize === "number" && !hasMeasured) {
    const fixedSize = sanitizeSize(estimateSize);
    const totalSize = count * fixedSize;
    return {
      count,
      totalSize,
      isFixed: true,
      fixedSize,
      offsets: null,
      getOffset(index: number): number {
        const clamped = Math.max(0, Math.min(count, index));
        return clamped * fixedSize;
      },
      getSize(index: number): number {
        if (index < 0 || index >= count) return 0;
        return fixedSize;
      },
    };
  }

  const offsets = new Float64Array(count + 1);
  let cursor = 0;
  for (let i = 0; i < count; i++) {
    const key = getItemKey ? getItemKey(i) : i;
    const measured = hasMeasured ? measuredSizes.get(key) : undefined;
    const rawSize =
      measured !== undefined
        ? measured
        : typeof estimateSize === "function"
          ? estimateSize(i)
          : estimateSize;
    cursor += sanitizeSize(rawSize);
    offsets[i + 1] = cursor;
  }

  return {
    count,
    totalSize: cursor,
    isFixed: false,
    fixedSize: 0,
    offsets,
    getOffset(index: number): number {
      const clamped = Math.max(0, Math.min(count, index));
      return offsets[clamped];
    },
    getSize(index: number): number {
      if (index < 0 || index >= count) return 0;
      return offsets[index + 1] - offsets[index];
    },
  };
}

/** Finds the first item index whose bottom edge (`offsets[i + 1]`) is strictly greater than `target`. */
function findStartIndex(offsets: Float64Array, count: number, target: number): number {
  let lo = 0;
  let hi = count - 1;
  while (lo < hi) {
    const mid = (lo + hi) >>> 1;
    if (offsets[mid + 1] <= target) {
      lo = mid + 1;
    } else {
      hi = mid;
    }
  }
  return lo;
}

/** Finds the exclusive end index: first `i` where `offsets[i] >= targetEnd`. */
function findEndIndex(
  offsets: Float64Array,
  count: number,
  fromIndex: number,
  targetEnd: number,
): number {
  if (targetEnd >= offsets[count]) return count;
  let lo = fromIndex;
  let hi = count;
  while (lo < hi) {
    const mid = (lo + hi) >>> 1;
    if (offsets[mid] < targetEnd) {
      lo = mid + 1;
    } else {
      hi = mid;
    }
  }
  return Math.max(fromIndex + 1, lo);
}

/** Computes the visible `[startIndex, endIndex)` slice plus top/bottom spacer heights. */
export function computeVirtualRange(params: {
  layout: VirtualWindowLayout;
  scrollTop: number;
  viewportHeight: number;
  overscan?: number;
  virtualizeThreshold?: number;
  enabled?: boolean;
}): VirtualRange {
  const {
    layout,
    scrollTop,
    viewportHeight,
    overscan = DEFAULT_OVERSCAN,
    virtualizeThreshold = DEFAULT_VIRTUALIZE_THRESHOLD,
    enabled = true,
  } = params;
  const { count, totalSize } = layout;

  if (count === 0) {
    return {
      startIndex: 0,
      endIndex: 0,
      paddingTop: 0,
      paddingBottom: 0,
      totalSize: 0,
      isVirtualized: false,
    };
  }

  const safeViewport = sanitizeSize(viewportHeight);
  const threshold = Math.max(0, Math.floor(virtualizeThreshold));
  const shouldVirtualize = enabled && count > threshold;

  if (!shouldVirtualize) {
    return {
      startIndex: 0,
      endIndex: count,
      paddingTop: 0,
      paddingBottom: 0,
      totalSize,
      isVirtualized: false,
    };
  }

  const safeOverscan = Math.max(0, Math.floor(overscan));
  const maxScrollTop = Math.max(0, totalSize - safeViewport);
  const clampedScrollTop = Math.max(
    0,
    Math.min(Number.isFinite(scrollTop) ? scrollTop : 0, maxScrollTop),
  );
  const viewportEnd = clampedScrollTop + safeViewport;

  let visibleStart: number;
  let visibleEnd: number;

  if (layout.isFixed) {
    visibleStart = Math.min(
      count - 1,
      Math.floor(clampedScrollTop / layout.fixedSize),
    );
    visibleEnd = Math.min(count, Math.ceil(viewportEnd / layout.fixedSize));
  } else {
    const offsets = layout.offsets!;
    visibleStart = findStartIndex(offsets, count, clampedScrollTop);
    visibleEnd = findEndIndex(offsets, count, visibleStart, viewportEnd);
  }

  const startIndex = Math.max(0, visibleStart - safeOverscan);
  const endIndex = Math.min(count, visibleEnd + safeOverscan);
  const paddingTop = layout.getOffset(startIndex);
  const paddingBottom = Math.max(0, totalSize - layout.getOffset(endIndex));

  return {
    startIndex,
    endIndex,
    paddingTop,
    paddingBottom,
    totalSize,
    isVirtualized: true,
  };
}

/** Materializes positioned descriptors for the active `[startIndex, endIndex)` slice. */
export function computeVirtualItems(
  layout: VirtualWindowLayout,
  startIndex: number,
  endIndex: number,
): VirtualItem[] {
  const safeStart = Math.max(0, Math.min(layout.count, startIndex));
  const safeEnd = Math.max(safeStart, Math.min(layout.count, endIndex));
  const items: VirtualItem[] = new Array(safeEnd - safeStart);
  for (let i = safeStart; i < safeEnd; i++) {
    const start = layout.getOffset(i);
    const size = layout.getSize(i);
    items[i - safeStart] = {
      index: i,
      start,
      size,
      end: start + size,
    };
  }
  return items;
}

/**
 * Computes the target `scrollTop` to bring `index` into view, or `null` when
 * `align === "auto"` and the item is already completely visible.
 */
export function computeScrollToIndexOffset(params: {
  layout: VirtualWindowLayout;
  index: number;
  scrollTop: number;
  viewportHeight: number;
  align?: VirtualScrollAlign;
}): number | null {
  const { layout, scrollTop, viewportHeight, align = "auto" } = params;
  if (layout.count === 0) return null;

  const clampedIndex = Math.max(0, Math.min(layout.count - 1, Math.floor(params.index)));
  const safeViewport = sanitizeSize(viewportHeight);
  const currentScrollTop = Math.max(0, Number.isFinite(scrollTop) ? scrollTop : 0);
  const itemStart = layout.getOffset(clampedIndex);
  const itemSize = layout.getSize(clampedIndex);
  const itemEnd = itemStart + itemSize;
  const maxScrollTop = Math.max(0, layout.totalSize - safeViewport);

  let target: number;
  switch (align) {
    case "start":
      target = itemStart;
      break;
    case "end":
      target = itemEnd - safeViewport;
      break;
    case "center":
      target = itemStart - (safeViewport - itemSize) / 2;
      break;
    case "auto": {
      if (itemStart >= currentScrollTop && itemEnd <= currentScrollTop + safeViewport) {
        return null;
      }
      if (itemStart < currentScrollTop) {
        target = itemStart;
      } else {
        target = itemEnd - safeViewport;
      }
      break;
    }
  }

  return Math.max(0, Math.min(maxScrollTop, target));
}

export interface UseVirtualWindowOptions {
  count: number;
  estimateSize: number | ((index: number) => number);
  overscan?: number;
  /** Render all rows directly without virtualization when `count <= threshold`. Default 80. */
  virtualizeThreshold?: number;
  /** Viewport height fallback (px) before DOM measurement or during SSR. Default 640. */
  defaultViewportHeight?: number;
  /** Explicitly toggle virtualization. Default true. */
  enabled?: boolean;
  /** Stable key per item used to cache measured variable heights. */
  getItemKey?: (index: number) => string | number;
}

export interface UseVirtualWindowResult extends VirtualRange {
  /** Attach to the scrollable container element. */
  scrollRef: RefCallback<HTMLElement>;
  /**
   * Optional ref callback for variable-height row elements.
   * Each observed element must carry `data-virtual-index={item.index}`.
   */
  measureElement: RefCallback<HTMLElement>;
  items: VirtualItem[];
  scrollToIndex: (
    index: number,
    options?: { align?: VirtualScrollAlign; behavior?: ScrollBehavior },
  ) => void;
}

const useIsomorphicLayoutEffect =
  typeof window !== "undefined" ? useLayoutEffect : useEffect;

export function useVirtualWindow({
  count,
  estimateSize,
  overscan = DEFAULT_OVERSCAN,
  virtualizeThreshold = DEFAULT_VIRTUALIZE_THRESHOLD,
  defaultViewportHeight = DEFAULT_VIEWPORT_HEIGHT,
  enabled = true,
  getItemKey,
}: UseVirtualWindowOptions): UseVirtualWindowResult {
  const [scrollElement, setScrollElement] = useState<HTMLElement | null>(null);
  const scrollElementRef = useRef<HTMLElement | null>(null);
  const scrollTopRef = useRef(0);
  const [scrollTop, setScrollTop] = useState(0);
  const [viewportHeight, setViewportHeight] = useState(() =>
    sanitizeSize(defaultViewportHeight),
  );
  const [measureVersion, setMeasureVersion] = useState(0);

  const measuredSizesRef = useRef<Map<string | number, number>>(new Map());
  const getItemKeyRef = useRef(getItemKey);
  getItemKeyRef.current = getItemKey;

  const layout = useMemo(() => {
    const measured = measureVersion > 0 ? measuredSizesRef.current : undefined;
    return buildVirtualLayout(count, estimateSize, measured, getItemKey);
  }, [count, estimateSize, getItemKey, measureVersion]);

  const layoutRef = useRef(layout);
  layoutRef.current = layout;

  const range = useMemo(
    () =>
      computeVirtualRange({
        layout,
        scrollTop,
        viewportHeight,
        overscan,
        virtualizeThreshold,
        enabled,
      }),
    [layout, scrollTop, viewportHeight, overscan, virtualizeThreshold, enabled],
  );

  const rangeRef = useRef(range);
  rangeRef.current = range;

  const viewportHeightRef = useRef(viewportHeight);
  viewportHeightRef.current = viewportHeight;

  const overscanRef = useRef(overscan);
  overscanRef.current = overscan;

  const thresholdRef = useRef(virtualizeThreshold);
  thresholdRef.current = virtualizeThreshold;

  const enabledRef = useRef(enabled);
  enabledRef.current = enabled;

  const items = useMemo(
    () => computeVirtualItems(layout, range.startIndex, range.endIndex),
    [layout, range.startIndex, range.endIndex],
  );

  const scrollRef = useCallback<RefCallback<HTMLElement>>((node) => {
    scrollElementRef.current = node;
    setScrollElement(node);
  }, []);

  // Track container scroll position and viewport height.
  useIsomorphicLayoutEffect(() => {
    const el = scrollElement;
    if (!el) return;

    const syncViewport = () => {
      const nextHeight = el.clientHeight;
      if (nextHeight > 0 && Math.abs(nextHeight - viewportHeightRef.current) >= 1) {
        viewportHeightRef.current = nextHeight;
        setViewportHeight(nextHeight);
      }
    };

    syncViewport();

    let rafId: number | null = null;
    const flushScroll = () => {
      rafId = null;
      const nextScrollTop = el.scrollTop;
      scrollTopRef.current = nextScrollTop;
      const prev = rangeRef.current;
      const next = computeVirtualRange({
        layout: layoutRef.current,
        scrollTop: nextScrollTop,
        viewportHeight: viewportHeightRef.current,
        overscan: overscanRef.current,
        virtualizeThreshold: thresholdRef.current,
        enabled: enabledRef.current,
      });
      // Skip React re-renders while scrolling stays inside the same row slice.
      if (
        next.startIndex !== prev.startIndex ||
        next.endIndex !== prev.endIndex ||
        next.isVirtualized !== prev.isVirtualized
      ) {
        setScrollTop(nextScrollTop);
      }
    };

    const handleScroll = () => {
      scrollTopRef.current = el.scrollTop;
      if (typeof requestAnimationFrame === "undefined") {
        flushScroll();
        return;
      }
      if (rafId === null) {
        rafId = requestAnimationFrame(flushScroll);
      }
    };

    el.addEventListener("scroll", handleScroll, { passive: true });

    let observer: ResizeObserver | null = null;
    if (typeof ResizeObserver !== "undefined") {
      observer = new ResizeObserver(syncViewport);
      observer.observe(el);
    }

    return () => {
      el.removeEventListener("scroll", handleScroll);
      if (rafId !== null && typeof cancelAnimationFrame !== "undefined") {
        cancelAnimationFrame(rafId);
      }
      observer?.disconnect();
    };
  }, [scrollElement]);

  // Keep cached measured sizes bounded when the underlying item set changes.
  useEffect(() => {
    const cache = measuredSizesRef.current;
    if (cache.size === 0 || count === 0) {
      if (count === 0 && cache.size > 0) cache.clear();
      return;
    }
    if (cache.size <= Math.max(256, count * 2)) return;
    const activeKeys = new Set<string | number>();
    for (let i = 0; i < count; i++) {
      activeKeys.add(getItemKey ? getItemKey(i) : i);
    }
    for (const key of cache.keys()) {
      if (!activeKeys.has(key)) cache.delete(key);
    }
  }, [count, getItemKey]);

  // Single shared ResizeObserver for variable-height rows.
  const itemObserverRef = useRef<ResizeObserver | null>(null);

  const recordElementHeight = useCallback((el: HTMLElement): number => {
    const indexAttr = el.dataset.virtualIndex;
    if (indexAttr === undefined) return 0;
    const index = Number(indexAttr);
    if (!Number.isInteger(index) || index < 0 || index >= layoutRef.current.count) {
      return 0;
    }

    const rawHeight = el.getBoundingClientRect().height;
    if (!Number.isFinite(rawHeight) || rawHeight <= 0) return 0;
    const height = Math.round(rawHeight * 2) / 2;

    const key = getItemKeyRef.current ? getItemKeyRef.current(index) : index;
    const prevSize = layoutRef.current.getSize(index);
    const cachedSize = measuredSizesRef.current.get(key);
    if (cachedSize !== undefined && Math.abs(cachedSize - height) < 0.5) {
      return 0;
    }
    if (cachedSize === undefined && Math.abs(prevSize - height) < 0.5) {
      measuredSizesRef.current.set(key, height);
      return 0;
    }

    const delta = height - prevSize;
    measuredSizesRef.current.set(key, height);

    const itemEnd = layoutRef.current.getOffset(index) + prevSize;
    if (itemEnd <= scrollTopRef.current && Math.abs(delta) >= 1) {
      return delta;
    }
    return Number.EPSILON; // Signal a height change without scrollTop adjustment.
  }, []);

  useEffect(() => {
    return () => {
      itemObserverRef.current?.disconnect();
      itemObserverRef.current = null;
    };
  }, []);

  const ensureItemObserver = useCallback((): ResizeObserver | null => {
    if (typeof ResizeObserver === "undefined") return null;
    if (!itemObserverRef.current) {
      itemObserverRef.current = new ResizeObserver((entries) => {
        let changed = false;
        let scrollCorrection = 0;
        for (const entry of entries) {
          if (!(entry.target instanceof HTMLElement)) continue;
          const result = recordElementHeight(entry.target);
          if (result !== 0) {
            changed = true;
            if (result !== Number.EPSILON) {
              scrollCorrection += result;
            }
          }
        }
        if (scrollCorrection !== 0 && scrollElementRef.current) {
          const nextTop = Math.max(0, scrollElementRef.current.scrollTop + scrollCorrection);
          scrollElementRef.current.scrollTop = nextTop;
          scrollTopRef.current = nextTop;
        }
        if (changed) {
          setMeasureVersion((v) => v + 1);
        }
      });
    }
    return itemObserverRef.current;
  }, [recordElementHeight]);

  const measureElement = useCallback<RefCallback<HTMLElement>>(
    (el) => {
      if (!el) return;
      const observer = ensureItemObserver();
      observer?.observe(el);
      const result = recordElementHeight(el);
      if (result !== 0) {
        if (result !== Number.EPSILON && scrollElementRef.current) {
          const nextTop = Math.max(0, scrollElementRef.current.scrollTop + result);
          scrollElementRef.current.scrollTop = nextTop;
          scrollTopRef.current = nextTop;
        }
        setMeasureVersion((v) => v + 1);
      }
      return () => {
        itemObserverRef.current?.unobserve(el);
      };
    },
    [ensureItemObserver, recordElementHeight],
  );

  const scrollToIndex = useCallback(
    (
      index: number,
      options?: { align?: VirtualScrollAlign; behavior?: ScrollBehavior },
    ) => {
      const el = scrollElementRef.current;
      const currentTop = el ? el.scrollTop : scrollTopRef.current;
      const targetTop = computeScrollToIndexOffset({
        layout: layoutRef.current,
        index,
        scrollTop: currentTop,
        viewportHeight: viewportHeightRef.current,
        align: options?.align ?? "auto",
      });
      if (targetTop === null) return;

      scrollTopRef.current = targetTop;
      if (el) {
        if (typeof el.scrollTo === "function") {
          el.scrollTo({ top: targetTop, behavior: options?.behavior ?? "auto" });
        } else {
          el.scrollTop = targetTop;
        }
      }
      setScrollTop(targetTop);
    },
    [],
  );

  return {
    ...range,
    items,
    scrollRef,
    measureElement,
    scrollToIndex,
  };
}
