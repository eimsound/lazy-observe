import uPlot from "uplot";

export interface UPlotTooltipSeries {
  color: string;
  dataIndex: number;
  label: string;
}

interface UPlotTooltipOptions {
  data: uPlot.AlignedData;
  formatTimestamp: (timestamp: number) => string;
  formatValue: (value: number) => string;
  series: UPlotTooltipSeries[];
}

/** Shared, DOM-safe tooltip used by both line and bar charts. */
export function createUPlotTooltip({
  data,
  formatTimestamp,
  formatValue,
  series,
}: UPlotTooltipOptions): uPlot.Plugin {
  const tooltip = document.createElement("div");
  tooltip.className = "lazy-uplot-tooltip";
  tooltip.setAttribute("role", "tooltip");
  tooltip.dataset.pinned = "false";
  let over: HTMLDivElement | null = null;
  let activeChart: uPlot | null = null;
  let hasContent = false;
  let pinned = false;

  type LockableCursor = uPlot.Cursor & { _lock?: boolean };

  const show = () => {
    if (hasContent) tooltip.style.display = "block";
  };
  const hide = () => {
    if (!pinned) tooltip.style.display = "none";
  };
  const setPinned = (nextPinned: boolean) => {
    pinned = nextPinned;
    tooltip.dataset.pinned = String(nextPinned);
    if (nextPinned) show();
  };
  const positionTooltip = (chart: uPlot) => {
    const { left, top } = chart.cursor;
    if (left === undefined || top === undefined) return;

    const plotRect = chart.over.getBoundingClientRect();
    const gap = 12;
    const viewportPadding = 8;
    const anchorLeft = plotRect.left + left;
    const anchorTop = plotRect.top + top;
    const rightPosition = anchorLeft + gap;
    const leftPosition = anchorLeft - gap - tooltip.offsetWidth;
    const bottomPosition = anchorTop + gap;
    const topPosition = anchorTop - gap - tooltip.offsetHeight;
    const horizontal = rightPosition + tooltip.offsetWidth <= window.innerWidth - viewportPadding
      ? rightPosition
      : leftPosition;
    const vertical = bottomPosition + tooltip.offsetHeight <= window.innerHeight - viewportPadding
      ? bottomPosition
      : topPosition;

    tooltip.style.left = `${
      Math.max(
        viewportPadding,
        Math.min(horizontal, window.innerWidth - tooltip.offsetWidth - viewportPadding),
      )
    }px`;
    tooltip.style.top = `${
      Math.max(
        viewportPadding,
        Math.min(vertical, window.innerHeight - tooltip.offsetHeight - viewportPadding),
      )
    }px`;
  };
  const unpin = () => {
    if (!pinned) return;
    setPinned(false);
    const cursor = activeChart?.cursor as LockableCursor | undefined;
    if (cursor) cursor._lock = false;
    activeChart?.setCursor({ left: -10, top: -10 });
    tooltip.style.display = "none";
  };
  const handleChartClick = () => {
    const cursor = activeChart?.cursor as LockableCursor | undefined;
    setPinned(cursor?._lock === true);
    if (!pinned && !over?.matches(":hover")) {
      tooltip.style.display = "none";
    }
  };
  const handleOutsidePointerDown = (event: PointerEvent) => {
    const target = event.target;
    if (!(target instanceof Node) || tooltip.contains(target) || over?.contains(target)) return;
    unpin();
  };
  const handleKeyDown = (event: KeyboardEvent) => {
    if (event.key === "Escape") unpin();
  };
  const handleViewportChange = () => {
    if (pinned && activeChart) positionTooltip(activeChart);
  };

  return {
    hooks: {
      init: [
        (chart) => {
          activeChart = chart;
          over = chart.over;
          over.addEventListener("mouseenter", show);
          over.addEventListener("mouseleave", hide);
          over.addEventListener("click", handleChartClick);
          document.addEventListener("pointerdown", handleOutsidePointerDown);
          document.addEventListener("keydown", handleKeyDown);
          window.addEventListener("resize", handleViewportChange);
          window.addEventListener("scroll", handleViewportChange, true);
          document.body.appendChild(tooltip);
        },
      ],
      setCursor: [
        (chart) => {
          const { idx, left, top } = chart.cursor;
          if (idx === null || idx === undefined || left === undefined || top === undefined) {
            hasContent = false;
            hide();
            return;
          }

          const timestamp = Number(data[0][idx]);
          const rows = series.flatMap((item) => {
            const value = data[item.dataIndex]?.[idx];
            return value === null || value === undefined || !Number.isFinite(value)
              ? []
              : [{ ...item, value: Number(value) }];
          });
          if (!Number.isFinite(timestamp) || rows.length === 0) {
            hasContent = false;
            hide();
            return;
          }

          const time = document.createElement("div");
          time.className = "lazy-uplot-tooltip-time";
          time.textContent = formatTimestamp(timestamp);
          const content = document.createDocumentFragment();
          content.appendChild(time);

          rows.forEach((item) => {
            const row = document.createElement("div");
            row.className = "lazy-uplot-tooltip-series";

            const marker = document.createElement("span");
            marker.className = "lazy-uplot-tooltip-marker";
            marker.style.backgroundColor = item.color;

            const label = document.createElement("span");
            label.className = "lazy-uplot-tooltip-label";
            label.textContent = item.label;

            const value = document.createElement("span");
            value.className = "lazy-uplot-tooltip-value";
            value.textContent = formatValue(item.value);

            row.append(marker, label, value);
            content.appendChild(row);
          });

          tooltip.replaceChildren(content);
          hasContent = true;
          tooltip.style.display = "block";
          positionTooltip(chart);
        },
      ],
      destroy: [
        () => {
          over?.removeEventListener("mouseenter", show);
          over?.removeEventListener("mouseleave", hide);
          over?.removeEventListener("click", handleChartClick);
          document.removeEventListener("pointerdown", handleOutsidePointerDown);
          document.removeEventListener("keydown", handleKeyDown);
          window.removeEventListener("resize", handleViewportChange);
          window.removeEventListener("scroll", handleViewportChange, true);
          tooltip.remove();
          activeChart = null;
          over = null;
        },
      ],
    },
  };
}

/** Double-clicking a zoomed chart restores its complete data extent. */
export function createResetZoomPlugin(): uPlot.Plugin {
  let over: HTMLDivElement | null = null;
  let chart: uPlot | null = null;
  const reset = () => chart?.setData(chart.data, true);

  return {
    hooks: {
      init: [
        (instance) => {
          chart = instance;
          over = instance.over;
          over.addEventListener("dblclick", reset);
        },
      ],
      destroy: [
        () => {
          over?.removeEventListener("dblclick", reset);
          over = null;
          chart = null;
        },
      ],
    },
  };
}
