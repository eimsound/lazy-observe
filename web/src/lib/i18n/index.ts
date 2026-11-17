import i18n from "i18next";
import { initReactI18next } from "react-i18next";
import LanguageDetector from "i18next-browser-languagedetector";
import { zh } from "./locales/zh";
import { en } from "./locales/en";

/**
 * i18n bootstrap. Default language is Chinese (requirements §1.3-4). Language is
 * persisted to localStorage and every UI string goes through the dictionary —
 * no inline literals in components. Adding a language = add a locale file typed
 * against `zh` and register it here.
 */

export const SUPPORTED_LANGUAGES = [
  { code: "zh", label: "中文" },
  { code: "en", label: "English" },
] as const;

export type LanguageCode = (typeof SUPPORTED_LANGUAGES)[number]["code"];

export const LANGUAGE_STORAGE_KEY = "lo-language";

void i18n
  .use(LanguageDetector)
  .use(initReactI18next)
  .init({
    resources: {
      zh: { translation: zh },
      en: { translation: en },
    },
    fallbackLng: "zh",
    supportedLngs: ["zh", "en"],
    interpolation: { escapeValue: false },
    detection: {
      order: ["localStorage", "navigator"],
      lookupLocalStorage: LANGUAGE_STORAGE_KEY,
      caches: ["localStorage"],
    },
  });

// Keep <html lang> in sync with the active language (a11y + correct hyphenation).
function syncHtmlLang(lng: string) {
  document.documentElement.setAttribute("lang", lng === "zh" ? "zh-CN" : lng);
}
syncHtmlLang(i18n.resolvedLanguage ?? "zh");
i18n.on("languageChanged", syncHtmlLang);

export default i18n;
