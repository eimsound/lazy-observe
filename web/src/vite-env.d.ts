/// <reference types="vite/client" />

interface ImportMetaEnv {
  /** "1" enables the built-in mock backend (offline sine-wave data). */
  readonly VITE_MOCK?: string;
  /** Override the dev API proxy target (default http://localhost:3100). */
  readonly VITE_API_TARGET?: string;
}

interface ImportMeta {
  readonly env: ImportMetaEnv;
}
