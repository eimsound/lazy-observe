import { useState, type FormEvent } from "react";
import { useTranslation } from "react-i18next";
import { Activity, Loader2, Info } from "lucide-react";
import { useAuth } from "@/lib/auth/auth-provider";
import { loginErrorTranslationKey } from "@/lib/api/login-error";
import { IS_MOCK } from "@/lib/settings";
import { loginRoute } from "@/router";
import { ThemeSwitcher } from "@/components/theme/theme-switcher";
import { LanguageSwitcher } from "@/components/i18n/language-switcher";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";

/**
 * Branded sign-in screen (rendered outside the AppShell). On success it routes
 * to the originally requested URL (`?redirect=`) preserved by the route guard.
 * Theme + language switchers stay available so the login screen honors prefs.
 */
export function LoginPage() {
  const { t } = useTranslation();
  const { login } = useAuth();
  const { redirect } = loginRoute.useSearch();

  const [username, setUsername] = useState(IS_MOCK ? "admin" : "");
  const [password, setPassword] = useState(IS_MOCK ? "demo" : "");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  async function onSubmit(e: FormEvent) {
    e.preventDefault();
    if (busy) return;
    setBusy(true);
    setError(null);
    try {
      await login(username, password);
      // Hard navigation to the originally requested path (handles targets with
      // their own query string) — the session persists across the reload.
      const target = redirect && redirect.startsWith("/") ? redirect : "/";
      window.location.assign(target);
    } catch (err) {
      setError(t(loginErrorTranslationKey(err)));
      setBusy(false);
    }
  }

  return (
    <div className="relative flex min-h-screen items-center justify-center bg-background px-4 text-foreground">
      <div className="absolute right-4 top-4 flex items-center gap-2">
        <LanguageSwitcher />
        <ThemeSwitcher />
      </div>

      <div className="w-full max-w-sm">
        <div className="mb-8 flex flex-col items-center gap-3 text-center">
          <div className="flex size-12 items-center justify-center rounded-xl bg-primary text-primary-foreground shadow-sm">
            <Activity className="size-7" />
          </div>
          <div>
            <div className="text-xl font-semibold tracking-tight">
              {t("common.appName")}
            </div>
            <div className="text-sm text-muted-foreground">{t("auth.brandTagline")}</div>
          </div>
        </div>

        <div className="rounded-xl border border-border bg-card p-6 shadow-sm">
          <h1 className="mb-4 text-base font-semibold">{t("auth.signInTitle")}</h1>
          <form onSubmit={onSubmit} className="space-y-4">
            <div className="space-y-1.5">
              <Label htmlFor="username">{t("auth.username")}</Label>
              <Input
                id="username"
                value={username}
                onChange={(e) => setUsername(e.target.value)}
                placeholder={t("auth.usernamePlaceholder")}
                autoComplete="username"
                autoFocus
                spellCheck={false}
              />
            </div>
            <div className="space-y-1.5">
              <Label htmlFor="password">{t("auth.password")}</Label>
              <Input
                id="password"
                type="password"
                value={password}
                onChange={(e) => setPassword(e.target.value)}
                placeholder={t("auth.passwordPlaceholder")}
                autoComplete="current-password"
              />
            </div>

            {error && (
              <p className="text-sm text-destructive" role="alert">
                {error}
              </p>
            )}

            <Button type="submit" className="w-full" disabled={busy}>
              {busy && <Loader2 className="size-4 animate-spin" />}
              {busy ? t("auth.signingIn") : t("auth.signIn")}
            </Button>
          </form>
        </div>

        {IS_MOCK && (
          <div className="mt-4 flex items-start gap-2 rounded-md border border-border bg-muted/40 px-3 py-2 text-xs text-muted-foreground">
            <Info className="mt-0.5 size-3.5 shrink-0" />
            <span>
              <span className="font-medium text-foreground">{t("auth.mockTitle")}</span>
              {" — "}
              {t("auth.mockHint")}
            </span>
          </div>
        )}
      </div>
    </div>
  );
}
