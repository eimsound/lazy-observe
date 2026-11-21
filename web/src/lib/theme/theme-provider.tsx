import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useLayoutEffect,
  useMemo,
  useState,
  type ReactNode,
} from "react";
import {
  DEFAULT_FONT,
  DEFAULT_MODE,
  DEFAULT_PALETTE,
  DEFAULT_RADIUS,
  STORAGE_KEYS,
  type ThemeMode,
} from "./presets";

/**
 * Central theme state. Owns the four orthogonal axes of the theme system:
 *   mode    -> .dark class on <html>        (light | dark | system)
 *   palette -> [data-palette] attribute     (brand color set)
 *   radius  -> inline --radius variable     (corner rounding)
 *   font    -> inline --font-scale variable (root font-size scale)
 * All four persist to localStorage and re-apply to <html> on change. The
 * pre-paint script in index.html applies the same values before React mounts
 * to prevent a flash. `resolvedMode` is what charts/anything reading the DOM
 * should branch on (system resolved to light/dark).
 */

interface ThemeContextValue {
  mode: ThemeMode;
  resolvedMode: "light" | "dark";
  palette: string;
  radius: string;
  font: string;
  setMode: (m: ThemeMode) => void;
  setPalette: (p: string) => void;
  setRadius: (r: string) => void;
  setFont: (f: string) => void;
  /** Reset all axes to their defaults. */
  reset: () => void;
  /** Bumped whenever any visual token changes — subscribe to re-read CSS vars. */
  version: number;
}

const ThemeContext = createContext<ThemeContextValue | null>(null);

function read(key: string, fallback: string): string {
  try {
    return localStorage.getItem(key) ?? fallback;
  } catch {
    return fallback;
  }
}

function write(key: string, value: string, clearWhenEmpty = false) {
  try {
    if (clearWhenEmpty && !value) localStorage.removeItem(key);
    else localStorage.setItem(key, value);
  } catch {
    /* ignore */
  }
}

function systemPrefersDark(): boolean {
  return (
    typeof window !== "undefined" &&
    window.matchMedia("(prefers-color-scheme: dark)").matches
  );
}

const useIsomorphicLayoutEffect =
  typeof window !== "undefined" ? useLayoutEffect : useEffect;

export function ThemeProvider({ children }: { children: ReactNode }) {
  const [mode, setModeState] = useState<ThemeMode>(
    () => read(STORAGE_KEYS.mode, DEFAULT_MODE) as ThemeMode,
  );
  const [palette, setPaletteState] = useState<string>(() =>
    read(STORAGE_KEYS.palette, DEFAULT_PALETTE),
  );
  const [radius, setRadiusState] = useState<string>(() =>
    read(STORAGE_KEYS.radius, DEFAULT_RADIUS),
  );
  const [font, setFontState] = useState<string>(() =>
    read(STORAGE_KEYS.font, DEFAULT_FONT),
  );
  const [systemDark, setSystemDark] = useState<boolean>(systemPrefersDark);
  const [version, setVersion] = useState(0);

  // Track OS preference while in "system" mode.
  useEffect(() => {
    const mq = window.matchMedia("(prefers-color-scheme: dark)");
    const onChange = () => {
      setSystemDark(mq.matches);
      setVersion((v) => v + 1);
    };
    mq.addEventListener("change", onChange);
    return () => mq.removeEventListener("change", onChange);
  }, []);

  const resolvedMode: "light" | "dark" =
    mode === "system" ? (systemDark ? "dark" : "light") : mode;

  // Apply mode -> .dark class before paint / child chart effects read CSS vars.
  useIsomorphicLayoutEffect(() => {
    document.documentElement.classList.toggle("dark", resolvedMode === "dark");
  }, [resolvedMode]);

  // Apply palette -> data-palette attribute.
  useIsomorphicLayoutEffect(() => {
    const root = document.documentElement;
    if (palette) root.setAttribute("data-palette", palette);
    else root.removeAttribute("data-palette");
  }, [palette]);

  // Apply radius -> inline --radius.
  useIsomorphicLayoutEffect(() => {
    document.documentElement.style.setProperty("--radius", radius);
  }, [radius]);

  // Apply font -> inline --font-scale (globals.css scales root font-size by it).
  useIsomorphicLayoutEffect(() => {
    document.documentElement.style.setProperty("--font-scale", font);
  }, [font]);

  const setMode = useCallback((m: ThemeMode) => {
    setModeState(m);
    setVersion((v) => v + 1);
    write(STORAGE_KEYS.mode, m);
  }, []);
  const setPalette = useCallback((p: string) => {
    setPaletteState(p);
    setVersion((v) => v + 1);
    write(STORAGE_KEYS.palette, p, true);
  }, []);
  const setRadius = useCallback((r: string) => {
    setRadiusState(r);
    setVersion((v) => v + 1);
    write(STORAGE_KEYS.radius, r);
  }, []);
  const setFont = useCallback((f: string) => {
    setFontState(f);
    setVersion((v) => v + 1);
    write(STORAGE_KEYS.font, f);
  }, []);
  const reset = useCallback(() => {
    setMode(DEFAULT_MODE);
    setPalette(DEFAULT_PALETTE);
    setRadius(DEFAULT_RADIUS);
    setFont(DEFAULT_FONT);
  }, [setMode, setPalette, setRadius, setFont]);

  const value = useMemo<ThemeContextValue>(
    () => ({
      mode,
      resolvedMode,
      palette,
      radius,
      font,
      setMode,
      setPalette,
      setRadius,
      setFont,
      reset,
      version,
    }),
    [mode, resolvedMode, palette, radius, font, setMode, setPalette, setRadius, setFont, reset, version],
  );

  return <ThemeContext.Provider value={value}>{children}</ThemeContext.Provider>;
}

// oxlint-disable-next-line react/only-export-components
export function useTheme(): ThemeContextValue {
  const ctx = useContext(ThemeContext);
  if (!ctx) throw new Error("useTheme must be used within ThemeProvider");
  return ctx;
}
