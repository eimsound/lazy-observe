import { useEffect, useState } from "react";
import { useTheme } from "@/lib/theme/theme-provider";

/**
 * Bridges the CSS-variable theme system into canvas-rendered charts, which
 * cannot read CSS vars themselves. We resolve the SAME --chart-N / --foreground /
 * --border / --muted-foreground tokens via getComputedStyle and hand uPlot
 * concrete colors — re-reading whenever the ThemeProvider bumps `version`
 * (mode / palette / radius change). This is the single point where the chart
 * palette is derived; chart components must never hardcode colors.
 */

export interface ChartTheme {
  series: string[];
  foreground: string;
  mutedForeground: string;
  border: string;
  background: string;
  card: string;
  tooltipBg: string;
  isDark: boolean;
}

function readVar(styles: CSSStyleDeclaration, name: string, fallback: string): string {
  const v = styles.getPropertyValue(name).trim();
  return v || fallback;
}

function readChartTheme(resolvedMode: string): ChartTheme {
  const styles = getComputedStyle(document.documentElement);
  const series = [1, 2, 3, 4, 5].map((n) =>
    readVar(styles, `--chart-${n}`, "#3b82f6"),
  );
  return {
    series,
    foreground: readVar(styles, "--foreground", "#0a0a0a"),
    mutedForeground: readVar(styles, "--muted-foreground", "#71717a"),
    border: readVar(styles, "--border", "#e4e4e7"),
    background: readVar(styles, "--background", "#ffffff"),
    card: readVar(styles, "--card", "#ffffff"),
    tooltipBg: readVar(styles, "--popover", "#ffffff"),
    isDark: resolvedMode === "dark",
  };
}

export function useChartTheme(): ChartTheme {
  const { version, resolvedMode } = useTheme();
  const [theme, setTheme] = useState<ChartTheme>(() => readChartTheme(resolvedMode));

  // Re-read whenever the ThemeProvider bumps `version` or the mode flips, and
  // once more on the next frame so values settle after fonts/layout apply.
  useEffect(() => {
    setTheme(readChartTheme(resolvedMode));
    const id = requestAnimationFrame(() => setTheme(readChartTheme(resolvedMode)));
    return () => cancelAnimationFrame(id);
  }, [version, resolvedMode]);

  return theme;
}
