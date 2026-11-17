/**
 * Theme option catalogs. Palettes map 1:1 to the `[data-palette]` selectors in
 * globals.css; radii map to concrete --radius values. Adding a preset =
 * add a CSS block + an entry here. No component code changes.
 */

export type ThemeMode = "light" | "dark" | "system";

export interface PaletteOption {
  id: string; // "" = base neutral (no data-palette attribute)
  labelKey: string; // i18n key
  swatch: string; // representative color for the swatch in the picker
}

export const PALETTES: PaletteOption[] = [
  { id: "", labelKey: "theme.paletteNeutral", swatch: "oklch(0.205 0 0)" },
  { id: "blue", labelKey: "theme.paletteBlue", swatch: "oklch(0.546 0.215 262.881)" },
  { id: "violet", labelKey: "theme.paletteViolet", swatch: "oklch(0.541 0.281 293.009)" },
  { id: "emerald", labelKey: "theme.paletteEmerald", swatch: "oklch(0.596 0.145 163.225)" },
  // Added set — OKLCH-authored for perceptually-even lightness, light/dark paired
  // (dark variants live in globals.css). See web/README.md §Theme.
  { id: "rose", labelKey: "theme.paletteRose", swatch: "oklch(0.586 0.222 17.585)" },
  { id: "amber", labelKey: "theme.paletteAmber", swatch: "oklch(0.666 0.169 58.318)" },
  { id: "cyan", labelKey: "theme.paletteCyan", swatch: "oklch(0.585 0.122 221.723)" },
];

export interface RadiusOption {
  id: string;
  label: string;
  value: string; // rem
}

export const RADII: RadiusOption[] = [
  { id: "none", label: "0", value: "0rem" },
  { id: "sm", label: "S", value: "0.3rem" },
  { id: "md", label: "M", value: "0.5rem" },
  { id: "lg", label: "L", value: "0.75rem" },
  { id: "xl", label: "XL", value: "1rem" },
];

export interface FontSizeOption {
  id: string;
  labelKey: string;
  /** Root font-size scale factor (drives the --font-scale CSS variable). */
  value: string;
}

/**
 * Font-size tiers. Applied as the inline `--font-scale` variable on <html>;
 * globals.css multiplies the root font-size by it, so every rem-based size in
 * the app (which is nearly all of them) scales in one shot.
 */
export const FONT_SIZES: FontSizeOption[] = [
  { id: "sm", labelKey: "theme.fontSm", value: "0.9375" },
  { id: "md", labelKey: "theme.fontMd", value: "1" },
  { id: "lg", labelKey: "theme.fontLg", value: "1.0625" },
];

export const STORAGE_KEYS = {
  mode: "lo-theme-mode",
  palette: "lo-theme-palette",
  radius: "lo-theme-radius",
  font: "lo-theme-font",
} as const;

export const DEFAULT_MODE: ThemeMode = "system";
export const DEFAULT_PALETTE = "";
export const DEFAULT_RADIUS = "0.5rem";
export const DEFAULT_FONT = "1";
