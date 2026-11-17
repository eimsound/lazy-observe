import { cn } from "@/lib/utils";

/**
 * Minimal inline SVG sparkline for list mini-trends. Stroke uses `currentColor`
 * so it inherits the surrounding text token (no hardcoded color); size is fixed
 * and scales via viewBox with a non-scaling stroke.
 */
export function Sparkline({
  data,
  className,
  width = 84,
  height = 24,
}: {
  data: number[];
  className?: string;
  width?: number;
  height?: number;
}) {
  if (data.length === 0) return <svg width={width} height={height} className={className} />;
  const max = Math.max(1, ...data);
  const step = data.length > 1 ? width / (data.length - 1) : width;
  const points = data
    .map((v, i) => `${(i * step).toFixed(1)},${(height - (v / max) * (height - 2) - 1).toFixed(1)}`)
    .join(" ");
  return (
    <svg
      width={width}
      height={height}
      viewBox={`0 0 ${width} ${height}`}
      preserveAspectRatio="none"
      className={cn("overflow-visible", className)}
      aria-hidden="true"
    >
      <polyline
        points={points}
        fill="none"
        stroke="currentColor"
        strokeWidth={1.5}
        strokeLinejoin="round"
        strokeLinecap="round"
        vectorEffect="non-scaling-stroke"
      />
    </svg>
  );
}
