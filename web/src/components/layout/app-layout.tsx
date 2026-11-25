import { Navigate, useRouterState } from "@tanstack/react-router";
import { useTranslation } from "react-i18next";
import { Loader2 } from "lucide-react";
import { lazyRouteComponent } from "@/components/router/lazy-route";
import { useAuth } from "@/lib/auth/auth-provider";

const AuthenticatedAppLayout = lazyRouteComponent(
  () => import("./authenticated-app-layout"),
  "AuthenticatedAppLayout",
);

/**
 * Authenticated layout + route guard. Every non-login route renders through
 * here: while the session is resolving we show a spinner; unauthenticated users
 * are redirected to /login with the originally requested path preserved in
 * `?redirect=`. Once authed, the project scope + AppShell wrap the page.
 */
export function AppLayout() {
  const { t } = useTranslation();
  const { status, user } = useAuth();
  const { pathname, searchStr, hash } = useRouterState({
    select: (s) => ({ pathname: s.location.pathname, searchStr: s.location.searchStr, hash: s.location.hash }),
  });
  // Full in-app URL the guard returns to after sign-in / password change.
  const redirect = pathname + (searchStr ?? "") + (hash ? `#${hash}` : "");

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
    // Guard against the login route itself (defensive — it lives outside this
    // layout) so a transient match can't build a self-referential redirect.
    if (pathname === "/login") return null;
    return <Navigate to="/login" search={{ redirect }} replace />;
  }

  if (user?.must_change_password) {
    // The backend refuses everything else with 403 password_change_required
    // until the generated initial password is replaced. Same transient-match
    // guard as the login redirect above.
    if (pathname === "/change-password") return null;
    return <Navigate to="/change-password" search={{ redirect }} replace />;
  }

  return <AuthenticatedAppLayout />;
}
