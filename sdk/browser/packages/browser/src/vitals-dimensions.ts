import type {
  VitalBrowserFamily,
  VitalDeviceType,
  VitalDimensions,
  VitalOSFamily,
} from "./types";

export interface NavigatorUABrand {
  readonly brand?: string;
}

export interface NavigatorUADataLike {
  readonly mobile?: boolean;
  readonly platform?: string;
  readonly brands?: ReadonlyArray<NavigatorUABrand>;
}

export interface VitalDimensionEnv {
  readonly userAgent?: string;
  readonly userAgentData?: NavigatorUADataLike;
  readonly maxTouchPoints?: number;
  readonly viewportWidth?: number;
}

const UNKNOWN_COUNTRY_CODES = new Set(["XX", "T1", "ZZ", "A1", "A2", "O1"]);

/**
 * Normalize an explicit application-supplied ISO 3166-1 alpha-2 country code.
 * Never fabricates a country from locale, language, or timezone; invalid or
 * reserved placeholder codes (such as XX/T1) are omitted.
 */
export function normalizeCountryCode(raw: string | undefined): string | undefined {
  if (typeof raw !== "string") return undefined;
  const trimmed = raw.trim().toUpperCase();
  if (!/^[A-Z]{2}$/.test(trimmed)) return undefined;
  if (UNKNOWN_COUNTRY_CODES.has(trimmed)) return undefined;
  return trimmed;
}

function readRuntimeEnv(): VitalDimensionEnv {
  let userAgent: string | undefined;
  let userAgentData: NavigatorUADataLike | undefined;
  let maxTouchPoints: number | undefined;
  let viewportWidth: number | undefined;

  if (typeof navigator !== "undefined") {
    try {
      if (typeof navigator.userAgent === "string") userAgent = navigator.userAgent;
    } catch { /* ignore restricted navigator */ }
    try {
      const candidate = (navigator as Navigator & { userAgentData?: NavigatorUADataLike }).userAgentData;
      if (candidate && typeof candidate === "object") userAgentData = candidate;
    } catch { /* ignore restricted userAgentData */ }
    try {
      if (typeof navigator.maxTouchPoints === "number") maxTouchPoints = navigator.maxTouchPoints;
    } catch { /* ignore */ }
  }

  if (typeof window !== "undefined") {
    try {
      if (typeof window.innerWidth === "number" && window.innerWidth > 0) {
        viewportWidth = window.innerWidth;
      }
    } catch { /* ignore */ }
  }

  return { userAgent, userAgentData, maxTouchPoints, viewportWidth };
}

function isIPadDesktopMode(ua: string, maxTouchPoints: number | undefined): boolean {
  return /Macintosh/i.test(ua) && typeof maxTouchPoints === "number" && maxTouchPoints > 1;
}

/**
 * Classify device form factor into a strict 4-value low-cardinality enum:
 * desktop | mobile | tablet | unknown.
 */
export function detectDeviceType(env: VitalDimensionEnv = readRuntimeEnv()): VitalDeviceType {
  const ua = (env.userAgent ?? "").slice(0, 1024);
  const touchPoints = env.maxTouchPoints;

  // Check tablet signatures before generic mobile/desktop checks (iPadOS may
  // report a Macintosh UA with multi-touch, and Android tablets omit "Mobile").
  if (
    /iPad|Tablet|PlayBook|Silk|Kindle/i.test(ua) ||
    (/Android/i.test(ua) && !/Mobile/i.test(ua)) ||
    isIPadDesktopMode(ua, touchPoints)
  ) {
    return "tablet";
  }

  if (env.userAgentData?.mobile === true) return "mobile";
  if (/Mobi|iPhone|iPod|Windows Phone|BlackBerry|Opera Mini|IEMobile/i.test(ua)) {
    return "mobile";
  }

  if (env.userAgentData?.mobile === false) return "desktop";

  // Ignore synthetic test/headless/bare UA strings when falling through to viewport.
  if (/\b(?:Windows NT|Macintosh|Mac OS X|CrOS|X11|Linux)\b/i.test(ua) && !/jsdom/i.test(ua)) {
    return "desktop";
  }

  if (typeof env.viewportWidth === "number" && env.viewportWidth > 0) {
    if (env.viewportWidth < 768) return "mobile";
    if (env.viewportWidth <= 1024 && typeof touchPoints === "number" && touchPoints > 0) {
      return "tablet";
    }
    return "desktop";
  }

  return "unknown";
}

/**
 * Classify browser engine/product into a finite 6-value enum:
 * chrome | edge | safari | firefox | opera | other.
 */
export function detectBrowserFamily(env: VitalDimensionEnv = readRuntimeEnv()): VitalBrowserFamily {
  const brands = env.userAgentData?.brands;
  if (Array.isArray(brands) && brands.length > 0) {
    const names = brands
      .map((entry) => (typeof entry?.brand === "string" ? entry.brand.toLowerCase() : ""))
      .filter(Boolean);
    if (names.some((name) => name.includes("edge"))) return "edge";
    if (names.some((name) => name.includes("opera"))) return "opera";
    if (names.some((name) => name.includes("firefox"))) return "firefox";
    if (names.some((name) => name.includes("chrome") || name.includes("chromium"))) return "chrome";
    if (names.some((name) => name.includes("safari"))) return "safari";
  }

  const ua = (env.userAgent ?? "").slice(0, 1024);
  if (!ua || /jsdom/i.test(ua)) return "other";
  if (/\b(?:Edg|EdgA|EdgiOS|Edge)\//i.test(ua)) return "edge";
  if (/\b(?:OPR|OPiOS|Opera)\//i.test(ua) || /\bOpera\b/i.test(ua)) return "opera";
  if (/\b(?:Firefox|FxiOS)\//i.test(ua)) return "firefox";
  if (/\b(?:Chrome|CriOS|HeadlessChrome)\//i.test(ua)) return "chrome";
  if (/\bSafari\//i.test(ua)) return "safari";
  return "other";
}

/**
 * Classify OS into a finite 7-value enum:
 * windows | macos | ios | android | linux | chromeos | other.
 */
export function detectOSFamily(env: VitalDimensionEnv = readRuntimeEnv()): VitalOSFamily {
  const ua = (env.userAgent ?? "").slice(0, 1024);
  const platform = (env.userAgentData?.platform ?? "").trim().toLowerCase();

  if (isIPadDesktopMode(ua, env.maxTouchPoints)) return "ios";

  if (platform) {
    if (platform.includes("chrome os") || platform === "chromeos" || platform === "cros") return "chromeos";
    if (platform.includes("android")) return "android";
    if (platform === "ios" || platform.includes("iphone") || platform.includes("ipad")) return "ios";
    if (platform.includes("win")) return "windows";
    if (platform.includes("mac")) return "macos";
    if (platform.includes("linux")) return "linux";
  }

  if (!ua || /jsdom/i.test(ua)) return "other";
  if (/\bCrOS\b/i.test(ua)) return "chromeos";
  if (/\bAndroid\b/i.test(ua)) return "android";
  if (/\b(?:iPhone|iPad|iPod)\b|CPU (?:iPhone )?OS\b/i.test(ua)) return "ios";
  if (/\bWindows\b/i.test(ua)) return "windows";
  if (/\b(?:Mac OS X|Macintosh)\b/i.test(ua)) return "macos";
  if (/\b(?:Linux|X11)\b/i.test(ua)) return "linux";
  return "other";
}

/**
 * Collect privacy-friendly, low-cardinality Web Vitals dimensions.
 * Never collects raw User-Agent, IP addresses, or inferred geography.
 */
export function collectVitalDimensions(
  explicitCountry?: string,
  env: VitalDimensionEnv = readRuntimeEnv(),
): VitalDimensions {
  const dimensions: VitalDimensions = {
    device_type: detectDeviceType(env),
    browser_family: detectBrowserFamily(env),
    os_family: detectOSFamily(env),
  };
  const country = normalizeCountryCode(explicitCountry);
  if (country) dimensions.country = country;
  return dimensions;
}
