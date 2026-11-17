import { useState } from "react";
import { Navigate } from "@tanstack/react-router";
import { useTranslation } from "react-i18next";
import { Activity, Loader2, LogOut, ShieldAlert } from "lucide-react";
import { useAuth } from "@/lib/auth/auth-provider";
import { changePasswordRoute } from "@/router";
import { PasswordCard } from "@/components/settings/password-card";
import { ThemeSwitcher } from "@/components/theme/theme-switcher";
import { LanguageSwitcher } from "@/components/i18n/language-switcher";
import { Button } from "@/components/ui/button";

/**
 * Forced first-login password change for the generated bootstrap admin
 * password. Rendered outside the AppShell (like the login page) because the
 * backend refuses every other API with 403 `password_change_required` until
 * the password is changed. Reuses the settings page password form.
 */
export function ChangePasswordPage() {
  const { t } = useTranslation();
  const { status, user, refresh, logout } = useAuth();
  const { redirect } = changePasswordRoute.useSearch();
  const [signingOut, setSigningOut] = useState(false);
  const target = redirect && redirect.startsWith("/") && !redirect.startsWith("/change-password")
    ? redirect
    : "/";

  if (status === "loading") {
    return (
      <div className="flex h-screen w-full items-center justify-center bg-background text-muted-foreground">
        <div className="flex items-center gap-2 text-sm">
          <Loader2 className="size-4 animate-spin" />
          {t("auth.guardLoading")}
        </div>
      </div>
    );
  }
  if (status === "anon") {
    return <Navigate to="/login" search={{ redirect: target }} replace />;
  }
  if (!user?.must_change_password) {
    return <Navigate to={target} replace />;
  }

  return (
    <div className="relative flex min-h-screen items-center justify-center bg-background px-4 text-foreground">
      <div className="absolute right-4 top-4 flex items-center gap-2">
        <LanguageSwitcher />
        <ThemeSwitcher />
      </div>

      <div className="w-full max-w-md space-y-4">
        <div className="flex flex-col items-center gap-3 text-center">
          <div className="flex size-12 items-center justify-center rounded-xl bg-primary text-primary-foreground shadow-sm">
            <Activity className="size-7" />
          </div>
          <div className="text-xl font-semibold tracking-tight">{t("common.appName")}</div>
        </div>

        <div
          role="alert"
          className="flex items-start gap-2 rounded-md border border-border bg-muted/40 px-3 py-2 text-sm"
        >
          <ShieldAlert className="mt-0.5 size-4 shrink-0 text-primary" />
          <div>
            <h1 className="font-medium">{t("password.requiredTitle")}</h1>
            <p className="text-muted-foreground">{t("password.requiredDescription")}</p>
          </div>
        </div>

        <PasswordCard onChanged={refresh} />

        <Button
          variant="ghost"
          className="w-full gap-1.5"
          disabled={signingOut}
          onClick={async () => {
            setSigningOut(true);
            try {
              await logout();
            } finally {
              setSigningOut(false);
            }
          }}
        >
          <LogOut className="size-4" />
          {t("topbar.signOut")}
        </Button>
      </div>
    </div>
  );
}
